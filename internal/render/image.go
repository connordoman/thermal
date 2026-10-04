package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // register decoders
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/connordoman/escpos"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Image limits. Decoding is bounded by pixel count before any pixels are
// allocated, so a small file claiming huge dimensions is rejected cheaply.
const (
	MaxImagePixels    = 40_000_000
	MaxImageDimension = 12_000
	MaxImagesPerJob   = 16
	// MaxPrintedImageHeight is 1 m of paper at 8 dots per mm.
	MaxPrintedImageHeight = 8000
	maxRedirects          = 3
)

// allowedImageTypes maps accepted MIME types to the format name the image
// package reports. SVG is deliberately absent: it is a document format that
// can reference other resources.
var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpeg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// ImageError is a problem with an image a request referenced. It is the
// client's fault, so the server reports it as a bad request.
type ImageError struct {
	Source string
	Err    error
}

func (e *ImageError) Error() string {
	if e.Source == "" {
		return "image: " + e.Err.Error()
	}
	return fmt.Sprintf("image %s: %v", e.Source, e.Err)
}
func (e *ImageError) Unwrap() error { return e.Err }

// ImageLoader loads images for jobs from URLs and data URIs.
type ImageLoader struct {
	// AllowPrivateHosts permits URLs that resolve to loopback, private,
	// link-local and other non-public addresses. Off by default, so a job
	// cannot make the server probe its own network (SSRF).
	AllowPrivateHosts bool
	MaxBytes          int64
	Timeout           time.Duration

	client *http.Client
}

// NewImageLoader returns a loader with the given limits.
func NewImageLoader(allowPrivate bool, maxBytes int64, timeout time.Duration) *ImageLoader {
	l := &ImageLoader{AllowPrivateHosts: allowPrivate, MaxBytes: maxBytes, Timeout: timeout}
	dialer := &net.Dialer{Timeout: timeout, Control: l.checkDial}
	l.client = &http.Client{
		Transport: &http.Transport{
			// No proxy: the address check must see the real destination.
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    timeout,
			ResponseHeaderTimeout:  timeout,
			MaxResponseHeaderBytes: 64 << 10,
			DisableCompression:     true, // keep size limits meaningful
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
	return l
}

// checkDial runs after DNS resolution for every connection, including
// redirects, so a hostname cannot be rebound to an internal address between
// checking and connecting.
func (l *ImageLoader) checkDial(network, address string, _ syscall.RawConn) error {
	if l.AllowPrivateHosts {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !publicAddr(ip.Unmap()) {
		return fmt.Errorf("refusing to connect to non-public address %s", ip)
	}
	return nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 internals
	netip.MustParsePrefix("2001:db8::/32"),
}

func publicAddr(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Load fetches or decodes src: an http(s) URL, a data: URI, or bare base64.
func (l *ImageLoader) Load(ctx context.Context, src string) (image.Image, error) {
	src = strings.TrimSpace(src)
	label := src
	if len(label) > 80 {
		label = label[:77] + "..."
	}
	var data []byte
	var declared string
	var err error
	switch {
	case strings.HasPrefix(src, "data:"):
		label = "data URI"
		data, declared, err = l.decodeDataURI(src)
	case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
		data, declared, err = l.fetch(ctx, src)
	default:
		if strings.Contains(src, "://") {
			return nil, &ImageError{label, errors.New("only http, https and data: sources are supported")}
		}
		label = "base64 data"
		data, err = l.decodeBase64(src)
	}
	if err != nil {
		return nil, &ImageError{label, err}
	}
	img, err := l.decode(data, declared)
	if err != nil {
		return nil, &ImageError{label, err}
	}
	return img, nil
}

func (l *ImageLoader) decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if int64(base64.StdEncoding.DecodedLen(len(s))) > l.MaxBytes+3 {
		return nil, fmt.Errorf("larger than %d bytes", l.MaxBytes)
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func (l *ImageLoader) decodeDataURI(s string) ([]byte, string, error) {
	meta, payload, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	if !ok {
		return nil, "", errors.New("malformed data URI")
	}
	mt, rest, _ := strings.Cut(meta, ";")
	if !strings.Contains(rest, "base64") {
		return nil, "", errors.New("data URI must be base64 encoded")
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	if _, ok := allowedImageTypes[mt]; !ok {
		return nil, "", fmt.Errorf("unsupported type %q (want PNG, JPEG, GIF or WebP)", mt)
	}
	b, err := l.decodeBase64(payload)
	return b, mt, err
}

func (l *ImageLoader) fetch(ctx context.Context, src string) ([]byte, string, error) {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return nil, "", errors.New("invalid URL")
	}
	if u.User != nil {
		return nil, "", errors.New("URLs with credentials are not allowed")
	}
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif, image/webp")
	req.Header.Set("User-Agent", "thermal-print-server/1")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("server returned %s", resp.Status)
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", errors.New("response has no valid Content-Type")
	}
	if _, ok := allowedImageTypes[mt]; !ok {
		return nil, "", fmt.Errorf("unsupported Content-Type %q (want PNG, JPEG, GIF or WebP)", mt)
	}
	if resp.ContentLength > l.MaxBytes {
		return nil, "", fmt.Errorf("image is %d bytes, more than the %d byte limit", resp.ContentLength, l.MaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, l.MaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > l.MaxBytes {
		return nil, "", fmt.Errorf("image is more than the %d byte limit", l.MaxBytes)
	}
	return data, mt, nil
}

// decode checks that data really is an allowed image of acceptable size and
// decodes it. The declared type (from the HTTP header or data URI) must
// agree with the bytes.
func (l *ImageLoader) decode(data []byte, declared string) (image.Image, error) {
	if int64(len(data)) > l.MaxBytes {
		return nil, fmt.Errorf("image is more than the %d byte limit", l.MaxBytes)
	}
	sniffed := http.DetectContentType(data)
	format, ok := allowedImageTypes[sniffed]
	if !ok {
		return nil, fmt.Errorf("content is %s, not PNG, JPEG, GIF or WebP", sniffed)
	}
	if declared != "" && declared != sniffed {
		return nil, fmt.Errorf("declared as %s but content is %s", declared, sniffed)
	}
	cfg, got, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading image header: %w", err)
	}
	if got != format {
		return nil, fmt.Errorf("content is %s, not %s", got, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxImageDimension || cfg.Height > MaxImageDimension ||
		cfg.Width*cfg.Height > MaxImagePixels {
		return nil, fmt.Errorf("image is %d×%d; the limit is %d pixels and %d on a side",
			cfg.Width, cfg.Height, MaxImagePixels, MaxImageDimension)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}
	return img, nil
}

// ImagePlacement controls how an image is sized and positioned.
type ImagePlacement struct {
	Width     int // dots; 0 fits the paper width (images are never enlarged unless Width is set)
	Align     escpos.Align
	Dither    bool
	Threshold uint8
}

// PrintImage scales img, places it on a paper-width canvas according to p and
// prints it. Placing it on a canvas rather than relying on ESC a works on
// every printer.
func PrintImage(b *escpos.Builder, img image.Image, p ImagePlacement) error {
	paper := b.PaperWidth()
	src := img.Bounds()
	w := src.Dx()
	if p.Width > 0 {
		w = p.Width
	}
	w = min(w, paper)
	h := max(src.Dy()*w/max(src.Dx(), 1), 1)
	if h > MaxPrintedImageHeight {
		return &ImageError{"", fmt.Errorf("image would print %d dots (%.1f m) tall; the limit is %d",
			h, float64(h)/8000, MaxPrintedImageHeight)}
	}

	canvas := image.NewRGBA(image.Rect(0, 0, paper, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	x := 0
	switch p.Align {
	case escpos.AlignCenter:
		x = (paper - w) / 2
	case escpos.AlignRight:
		x = paper - w
	}
	dst := image.Rect(x, 0, x+w, h)
	// Composite over white so transparent areas print as paper.
	xdraw.CatmullRom.Scale(canvas, dst, img, src, xdraw.Over, nil)
	return b.PrintImage(canvas, escpos.ImageOptions{
		MaxWidth:  paper,
		Dither:    p.Dither,
		Threshold: p.Threshold,
	})
}
