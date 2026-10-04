package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fd00::1": false, "224.0.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v", addr, got)
		}
	}
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

func TestImageLoader(t *testing.T) {
	l := NewImageLoader(false, 1<<20, time.Second)
	ctx := context.Background()
	good := base64.StdEncoding.EncodeToString(pngBytes(4, 4))
	if _, err := l.Load(ctx, "data:image/png;base64,"+good); err != nil {
		t.Errorf("valid data URI: %v", err)
	}
	if _, err := l.Load(ctx, good); err != nil {
		t.Errorf("bare base64: %v", err)
	}
	for name, src := range map[string]string{
		"declared jpeg": "data:image/jpeg;base64," + good,
		"svg":           "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=",
		"not an image":  base64.StdEncoding.EncodeToString([]byte("hello")),
		"file scheme":   "file:///etc/passwd",
		"loopback url":  "http://127.0.0.1/x.png",
		"metadata url":  "http://169.254.169.254/latest/meta-data",
		"too many px":   base64.StdEncoding.EncodeToString(pngBytes(MaxImageDimension+1, 1)),
	} {
		var ie *ImageError
		if _, err := l.Load(ctx, src); !errors.As(err, &ie) {
			t.Errorf("%s: got %v, want an ImageError", name, err)
		}
	}
}

func TestImageLoaderHTTP(t *testing.T) {
	img := pngBytes(8, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(img)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
		case "/lying.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("GIF89a not really"))
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, 2<<20))
		case "/redirect":
			http.Redirect(w, r, "/ok.png", http.StatusFound)
		}
	}))
	defer srv.Close()

	// The test server is on loopback, so it is refused by default...
	strict := NewImageLoader(false, 1<<20, time.Second)
	if _, err := strict.Load(context.Background(), srv.URL+"/ok.png"); err == nil {
		t.Error("loopback server allowed")
	}
	// ...and allowed when private hosts are enabled.
	l := NewImageLoader(true, 1<<20, time.Second)
	for path, ok := range map[string]bool{
		"/ok.png": true, "/redirect": true, "/html": false, "/lying.png": false, "/big.png": false,
	} {
		_, err := l.Load(context.Background(), srv.URL+path)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v", path, err)
		}
	}
}

func TestASCII(t *testing.T) {
	for in, want := range map[string]string{
		"Café — “naïve” résumé…": `Cafe - "naive" resume...`,
		"Straße Łódź":            "Strasse Lodz",
		"tab\there\x1b@":         "tab here@",
		"日本 😀":                   "?? ?",
	} {
		if got := ASCII(in); got != want {
			t.Errorf("ASCII(%q) = %q, want %q", in, got, want)
		}
	}
}
