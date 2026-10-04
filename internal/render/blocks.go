package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/layout"
)

// Document is a parsed block document.
type Document struct {
	Schema   string            `json:"$schema,omitempty"`
	Version  int               `json:"version,omitempty"`
	Metadata map[string]any    `json:"metadata,omitempty"`
	Options  DocumentOptions   `json:"options"`
	Blocks   []json.RawMessage `json:"blocks"`
}

// DocumentOptions are a document's job-wide settings.
type DocumentOptions struct {
	Cut        string `json:"cut,omitempty"`
	Feed       uint8  `json:"feed,omitempty"`
	Copies     int    `json:"copies,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	OpenDrawer bool   `json:"open_drawer,omitempty"`
	Beep       bool   `json:"beep,omitempty"`
	Unicode    string `json:"unicode,omitempty"`
}

// Label returns metadata.label, if set.
func (d *Document) Label() string {
	s, _ := d.Metadata["label"].(string)
	return s
}

// Finish returns the document's cut and peripheral settings.
func (d *Document) Finish() Finish {
	cut, _ := ParseCutMode(d.Options.Cut)
	return Finish{Cut: cut, Feed: d.Options.Feed, OpenDrawer: d.Options.OpenDrawer, Beep: d.Options.Beep}
}

// ParseDocument validates body against the schema and decodes it. The body
// is a document object or a bare array of blocks.
func ParseDocument(body []byte) (*Document, error) {
	if err := Validate(body); err != nil {
		return nil, err
	}
	var doc Document
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		if err := json.Unmarshal(t, &doc.Blocks); err != nil {
			return nil, err
		}
		return &doc, nil
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// BlockError is a problem with one block that the schema cannot catch, such
// as an invalid EAN-13 check digit.
type BlockError struct {
	Path string // JSON pointer to the block
	Err  error
}

func (e *BlockError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *BlockError) Unwrap() error { return e.Err }

const (
	maxDepth  = 8
	maxBlocks = 5000
)

// scope is the style and alignment inherited from enclosing groups.
type scope struct {
	style layout.Style
	align escpos.Align
}

type docRenderer struct {
	ctx     context.Context
	env     *Env
	b       *escpos.Builder
	w       *layout.Writer
	unicode UnicodeMode
	count   int
	inPage  bool
	// lastCut records whether the last top-level block was a cut, so the
	// document's own cut is not added after it.
	lastCut bool
}

// RenderDocument renders a parsed block document.
func RenderDocument(ctx context.Context, env *Env, doc *Document) ([]byte, error) {
	b, w := env.NewJob()
	r := &docRenderer{ctx: ctx, env: env, b: b, w: w, unicode: UnicodeMode(doc.Options.Unicode)}
	if r.unicode == "" {
		r.unicode = UnicodeImage
	}
	if err := r.blocks(doc.Blocks, scope{}, "/blocks", 0); err != nil {
		return nil, err
	}
	w.Apply(layout.Style{})
	f := doc.Finish()
	if r.lastCut {
		f.Cut, f.Feed = CutNone, 0
	}
	f.Apply(b)
	return b.Bytes(), nil
}

func (r *docRenderer) blocks(raw []json.RawMessage, sc scope, path string, depth int) error {
	if depth > maxDepth {
		return &BlockError{path, fmt.Errorf("groups and pages may nest at most %d deep", maxDepth)}
	}
	for i, m := range raw {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		r.count++
		if r.count > maxBlocks {
			return &BlockError{path, fmt.Errorf("a document may contain at most %d blocks", maxBlocks)}
		}
		p := fmt.Sprintf("%s/%d", path, i)
		r.lastCut = false
		if err := r.block(m, sc, p, depth); err != nil {
			var be *BlockError
			if errors.As(err, &be) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return &BlockError{p, err}
		}
		// Each block starts from the default style.
		r.w.Apply(layout.Style{})
	}
	return nil
}

// jsonStyle is a style in a document. Unset fields inherit.
type jsonStyle struct {
	Bold         *bool      `json:"bold"`
	Underline    *underline `json:"underline"`
	DoubleStrike *bool      `json:"double_strike"`
	Invert       *bool      `json:"invert"`
	Font         *string    `json:"font"`
	Size         *uint8     `json:"size"`
	Width        *uint8     `json:"width"`
	Height       *uint8     `json:"height"`
	UpsideDown   *bool      `json:"upside_down"`
}

type underline uint8

func (u *underline) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "true":
		*u = 1
	case "false":
		*u = 0
	default:
		n, err := strconv.ParseUint(string(b), 10, 8)
		if err != nil || n > 2 {
			return fmt.Errorf("underline must be a boolean or 0–2")
		}
		*u = underline(n)
	}
	return nil
}

func (j *jsonStyle) apply(s layout.Style) layout.Style {
	if j == nil {
		return s
	}
	set := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	set(&s.Bold, j.Bold)
	set(&s.DoubleStrike, j.DoubleStrike)
	set(&s.Invert, j.Invert)
	set(&s.UpsideDown, j.UpsideDown)
	if j.Underline != nil {
		s.Underline = uint8(*j.Underline)
	}
	if j.Font != nil {
		s.FontB = strings.EqualFold(*j.Font, "B")
	}
	if j.Size != nil {
		s.Width, s.Height = *j.Size, *j.Size
	}
	if j.Width != nil {
		s.Width = *j.Width
	}
	if j.Height != nil {
		s.Height = *j.Height
	}
	return s
}

func parseAlign(s *string, def escpos.Align) escpos.Align {
	if s == nil {
		return def
	}
	switch *s {
	case "center":
		return escpos.AlignCenter
	case "right":
		return escpos.AlignRight
	}
	return escpos.AlignLeft
}

// ParseAlign parses left, center or right; anything else is left.
func ParseAlign(s string) escpos.Align { return parseAlign(&s, escpos.AlignLeft) }

func (r *docRenderer) useUnicode(s string) bool {
	return r.unicode == UnicodeImage && !r.inPage && NeedsUnicode(s)
}

// text prints a paragraph with printer fonts, or as an image if it holds
// characters the fonts lack.
func (r *docRenderer) text(spans []layout.Span, first, rest string, align escpos.Align, wrap bool) error {
	if r.useUnicode(spansText(spans)) {
		st := layout.Style{}
		if len(spans) > 0 {
			st = spans[0].Style
		}
		return PrintUnicode(r.b, spansText(spans), UnicodeOptions{
			Scale: 1.5 * float64(max(st.Height, 1)), Bold: st.Bold, Invert: st.Invert,
			Align: align, NoWrap: !wrap, FirstPrefix: first, RestPrefix: rest,
		})
	}
	r.w.SetAlign(align)
	if wrap {
		r.w.Paragraph(spans, []layout.Span{{Text: first}}, []layout.Span{{Text: rest}})
	} else {
		r.w.Line(append([]layout.Span{{Text: first}}, spans...)...)
	}
	return nil
}

func (r *docRenderer) block(raw json.RawMessage, sc scope, path string, depth int) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return err
	}
	decode := func(v any) error { return decodeStrict(raw, v) }
	b, w := r.b, r.w

	switch head.Type {
	case "text":
		var v struct {
			Type    string  `json:"type"`
			Content *string `json:"content"`
			Spans   []struct {
				Content string     `json:"content"`
				Style   *jsonStyle `json:"style"`
			} `json:"spans"`
			Style *jsonStyle `json:"style"`
			Align *string    `json:"align"`
			Wrap  *bool      `json:"wrap"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		base := v.Style.apply(sc.style)
		var spans []layout.Span
		if v.Content != nil {
			spans = []layout.Span{{Text: *v.Content, Style: base}}
		}
		for _, s := range v.Spans {
			spans = append(spans, layout.Span{Text: s.Content, Style: s.Style.apply(base)})
		}
		for i := range spans {
			spans[i].Text = Clean(spans[i].Text)
		}
		return r.text(spans, "", "", parseAlign(v.Align, sc.align), v.Wrap == nil || *v.Wrap)

	case "heading":
		var v struct {
			Type    string  `json:"type"`
			Content string  `json:"content"`
			Level   int     `json:"level"`
			Align   *string `json:"align"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		st, _ := headingStyle(max(v.Level, 1))
		return r.text([]layout.Span{{Text: Clean(v.Content), Style: st}}, "", "", parseAlign(v.Align, sc.align), true)

	case "markdown":
		var v struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			Links   string `json:"links"`
			Images  *bool  `json:"images"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		um := r.unicode
		if r.inPage {
			um = UnicodeTransliterate
		}
		err := WriteMarkdown(r.ctx, r.env, w, v.Content, MarkdownOptions{
			Links: LinkMode(v.Links), Unicode: um, Images: (v.Images == nil || *v.Images) && !r.inPage,
		})
		w.Apply(layout.Style{})
		return err

	case "unicode", "utf8":
		var v struct {
			Type    string  `json:"type"`
			Content string  `json:"content"`
			Scale   float64 `json:"scale"`
			Bold    bool    `json:"bold"`
			Invert  bool    `json:"invert"`
			Align   *string `json:"align"`
			LineGap *int    `json:"line_gap"`
			Wrap    *bool   `json:"wrap"`
			Weight  *int    `json:"weight"`
			Solid   bool    `json:"solid_emoji"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		if r.inPage {
			return errors.New("unicode blocks are images, which do not print in page mode")
		}
		return PrintUnicode(b, v.Content, UnicodeOptions{
			Scale: v.Scale, Bold: v.Bold, Invert: v.Invert, Align: parseAlign(v.Align, sc.align),
			LineGap: v.LineGap, NoWrap: v.Wrap != nil && !*v.Wrap,
			Weight: v.Weight, SolidEmoji: v.Solid,
		})

	case "feed":
		var v struct {
			Type  string `json:"type"`
			Lines *uint8 `json:"lines"`
			Dots  *uint8 `json:"dots"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		if v.Lines != nil {
			w.Apply(layout.Style{})
			b.FeedLines(*v.Lines)
		} else if v.Dots != nil {
			b.FeedUnits(*v.Dots)
		}

	case "rule":
		var v struct {
			Type  string     `json:"type"`
			Char  string     `json:"char"`
			Style *jsonStyle `json:"style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		c := '─'
		if v.Char != "" {
			c = []rune(Clean(v.Char) + "-")[0]
		}
		w.SetAlign(escpos.AlignLeft)
		w.Rule(c, v.Style.apply(sc.style))

	case "key_value":
		var v struct {
			Type       string     `json:"type"`
			Key        string     `json:"key"`
			Value      string     `json:"value"`
			Leader     string     `json:"leader"`
			KeyStyle   *jsonStyle `json:"key_style"`
			ValueStyle *jsonStyle `json:"value_style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		leader := ' '
		if v.Leader != "" {
			leader = []rune(Clean(v.Leader) + " ")[0]
		}
		w.SetAlign(escpos.AlignLeft)
		w.KeyValue(Clean(v.Key), Clean(v.Value), leader, v.KeyStyle.apply(sc.style), v.ValueStyle.apply(sc.style))

	case "columns":
		var v struct {
			Type    string `json:"type"`
			Columns []struct {
				Content string  `json:"content"`
				Align   *string `json:"align"`
				Width   int     `json:"width"`
			} `json:"columns"`
			Style  *jsonStyle `json:"style"`
			Border bool       `json:"border"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		st := v.Style.apply(sc.style)
		t := layout.Table{Border: v.Border, Style: st, Rows: [][]string{nil}}
		for _, c := range v.Columns {
			t.Rows[0] = append(t.Rows[0], Clean(c.Content))
			t.Aligns = append(t.Aligns, parseAlign(c.Align, escpos.AlignLeft))
			t.Weights = append(t.Weights, max(c.Width, 1))
		}
		// Built as a table rather than with Writer.Columns so the Unicode
		// fallback in r.table applies.
		return r.table(t)

	case "table":
		var v struct {
			Type        string     `json:"type"`
			Headers     []string   `json:"headers"`
			Rows        [][]any    `json:"rows"`
			Align       []string   `json:"align"`
			Border      bool       `json:"border"`
			Style       *jsonStyle `json:"style"`
			HeaderStyle *jsonStyle `json:"header_style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		st := v.Style.apply(sc.style)
		hs := v.HeaderStyle.apply(layout.Style{Bold: true})
		// Header and body must share character widths to line up.
		hs.FontB, hs.Width = st.FontB, st.Width
		t := layout.Table{Border: v.Border, Style: st, HeaderStyle: hs}
		for _, h := range v.Headers {
			t.Headers = append(t.Headers, Clean(h))
		}
		for _, a := range v.Align {
			t.Aligns = append(t.Aligns, ParseAlign(a))
		}
		for _, row := range v.Rows {
			var cells []string
			for _, c := range row {
				switch c := c.(type) {
				case string:
					cells = append(cells, Clean(c))
				case float64:
					cells = append(cells, strconv.FormatFloat(c, 'f', -1, 64))
				default:
					cells = append(cells, fmt.Sprint(c))
				}
			}
			t.Rows = append(t.Rows, cells)
		}
		return r.table(t)

	case "list":
		var v struct {
			Type    string     `json:"type"`
			Items   []string   `json:"items"`
			Ordered bool       `json:"ordered"`
			Start   *int       `json:"start"`
			Marker  string     `json:"marker"`
			Style   *jsonStyle `json:"style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		st := v.Style.apply(sc.style)
		n := 1
		if v.Start != nil {
			n = *v.Start
		}
		marker := "-"
		if v.Marker != "" {
			marker = Clean(v.Marker)
		}
		for _, item := range v.Items {
			m := marker + " "
			if v.Ordered {
				m = fmt.Sprintf("%d. ", n)
				n++
			}
			if err := r.text([]layout.Span{{Text: Clean(item), Style: st}}, m, strings.Repeat(" ", len([]rune(m))), escpos.AlignLeft, true); err != nil {
				return err
			}
		}

	case "box":
		var v struct {
			Type    string     `json:"type"`
			Content string     `json:"content"`
			Style   *jsonStyle `json:"style"`
			Align   *string    `json:"align"`
			Double  bool       `json:"double"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		w.Box(Clean(v.Content), v.Style.apply(sc.style), parseAlign(v.Align, escpos.AlignLeft), v.Double)

	case "qr_code":
		var v struct {
			Type            string  `json:"type"`
			Content         string  `json:"content"`
			Size            uint8   `json:"size"`
			ErrorCorrection string  `json:"error_correction"`
			Model           int     `json:"model"`
			Align           *string `json:"align"`
			Caption         string  `json:"caption"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		size := v.Size
		if size == 0 {
			size = 6
		}
		ec := escpos.QRErrorM
		if v.ErrorCorrection != "" {
			ec = escpos.QRErrorCorrection(v.ErrorCorrection[0])
		}
		w.Apply(layout.Style{})
		w.SetAlign(parseAlign(v.Align, escpos.AlignCenter))
		if v.Model == 1 {
			if err := b.SelectQRCodeModel(escpos.QRModel1); err != nil {
				return err
			}
		}
		if err := b.PrintQRCode(v.Content, ec, size); err != nil {
			return err
		}
		b.LineFeed()
		if v.Caption != "" {
			w.Line(layout.Span{Text: Clean(v.Caption), Style: layout.Style{FontB: true}})
			w.Apply(layout.Style{})
		}

	case "barcode":
		var v struct {
			Type      string  `json:"type"`
			Symbology string  `json:"symbology"`
			Content   string  `json:"content"`
			Height    uint8   `json:"height"`
			Width     uint8   `json:"width"`
			HRI       string  `json:"hri"`
			HRIFont   string  `json:"hri_font"`
			Align     *string `json:"align"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		return r.barcode(v.Symbology, v.Content, v.Height, v.Width, v.HRI, v.HRIFont, parseAlign(v.Align, escpos.AlignCenter))

	case "pdf417":
		var v struct {
			Type     string  `json:"type"`
			Content  string  `json:"content"`
			Columns  *uint8  `json:"columns"`
			Security *uint8  `json:"security"`
			Ratio    *uint8  `json:"ratio"`
			Width    *uint8  `json:"width"`
			Align    *string `json:"align"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		def := func(p *uint8, d uint8) uint8 {
			if p == nil {
				return d
			}
			return *p
		}
		w.Apply(layout.Style{})
		w.SetAlign(parseAlign(v.Align, escpos.AlignCenter))
		if err := b.SetBarcodeWidth(def(v.Width, 2)); err != nil {
			return err
		}
		if err := b.PrintPDF417(v.Content, def(v.Columns, 4), def(v.Security, 2), def(v.Ratio, 3)); err != nil {
			return err
		}
		b.LineFeed()

	case "image":
		var v struct {
			Type      string  `json:"type"`
			Src       string  `json:"src"`
			Width     int     `json:"width"`
			Align     *string `json:"align"`
			Dither    *bool   `json:"dither"`
			Threshold uint8   `json:"threshold"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		if r.inPage {
			return errors.New("images do not print in page mode")
		}
		img, err := r.env.loadImage(r.ctx, v.Src)
		if err != nil {
			return err
		}
		w.SetAlign(escpos.AlignLeft)
		return PrintImage(b, img, ImagePlacement{
			Width: v.Width, Align: parseAlign(v.Align, escpos.AlignCenter),
			Dither: v.Dither == nil || *v.Dither, Threshold: v.Threshold,
		})

	case "cut":
		var v struct {
			Type    string `json:"type"`
			Mode    string `json:"mode"`
			Content string `json:"content"`
			Feed    uint8  `json:"feed"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		mode := v.Mode
		if mode == "" {
			mode = v.Content
		}
		w.Apply(layout.Style{})
		if strings.EqualFold(mode, "full") {
			b.FeedAndFullCut(v.Feed)
		} else {
			b.FeedAndCut(v.Feed)
		}
		r.lastCut = depth == 0

	case "drawer":
		var v struct {
			Type  string `json:"type"`
			Pin   int    `json:"pin"`
			OnMs  int    `json:"on_ms"`
			OffMs int    `json:"off_ms"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		pin := escpos.DrawerPin2
		if v.Pin == 5 {
			pin = escpos.DrawerPin5
		}
		on, off := v.OnMs, v.OffMs
		if on == 0 {
			on = 100
		}
		if off == 0 {
			off = 500
		}
		return b.GeneratePulse(pin, uint8(min(on/2, 255)), uint8(min(off/2, 255)))

	case "beep":
		var v struct {
			Type     string `json:"type"`
			Tone     uint8  `json:"tone"`
			Duration uint8  `json:"duration"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		return b.Beep(max(v.Tone, 1), max(v.Duration, 1))

	case "raw":
		var v struct {
			Type   string `json:"type"`
			Hex    string `json:"hex"`
			Base64 string `json:"base64"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		var data []byte
		var err error
		if v.Hex != "" {
			data, err = hex.DecodeString(strings.Join(strings.Fields(v.Hex), ""))
		} else {
			data, err = base64.StdEncoding.DecodeString(v.Base64)
		}
		if err != nil {
			return err
		}
		b.Raw(data...)

	case "initialize":
		b.Initialize()
		w.Reset()

	case "code_page":
		var v struct {
			Type     string `json:"type"`
			CodePage uint8  `json:"code_page"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		b.SelectCodePage(escpos.CodePage(v.CodePage))

	case "charset":
		var v struct {
			Type    string `json:"type"`
			Charset uint8  `json:"charset"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		return b.SelectInternationalCharset(escpos.Charset(v.Charset))

	case "line_spacing":
		var v struct {
			Type string          `json:"type"`
			Dots json.RawMessage `json:"dots"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		if string(v.Dots) == `"default"` {
			b.DefaultLineSpacing()
		} else {
			n, err := strconv.ParseUint(string(v.Dots), 10, 8)
			if err != nil {
				return err
			}
			b.SetLineSpacing(uint8(n))
		}

	case "margins":
		var v struct {
			Type  string  `json:"type"`
			Left  uint16  `json:"left"`
			Width *uint16 `json:"width"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		width := uint16(max(r.env.PaperWidth-int(v.Left), 8))
		if v.Width != nil && *v.Width > 0 {
			width = min(*v.Width, width)
		}
		b.SetLeftMargin(v.Left)
		b.SetPrintAreaWidth(width)
		w.Width = int(width)

	case "group":
		var v struct {
			Type   string            `json:"type"`
			Style  *jsonStyle        `json:"style"`
			Align  *string           `json:"align"`
			Blocks []json.RawMessage `json:"blocks"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		inner := scope{style: v.Style.apply(sc.style), align: parseAlign(v.Align, sc.align)}
		return r.blocks(v.Blocks, inner, path+"/blocks", depth+1)

	case "page":
		var v struct {
			Type      string            `json:"type"`
			Width     *uint16           `json:"width"`
			Height    *uint16           `json:"height"`
			X         uint16            `json:"x"`
			Y         uint16            `json:"y"`
			Direction string            `json:"direction"`
			Blocks    []json.RawMessage `json:"blocks"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		if r.inPage {
			return errors.New("pages cannot be nested")
		}
		pw, ph := uint16(r.env.PaperWidth), uint16(400)
		if v.Width != nil {
			pw = *v.Width
		}
		if v.Height != nil {
			ph = *v.Height
		}
		dir := map[string]escpos.PageDirection{
			"": escpos.PageLeftToRight, "left_to_right": escpos.PageLeftToRight,
			"bottom_to_top": escpos.PageBottomToTop, "right_to_left": escpos.PageRightToLeft,
			"top_to_bottom": escpos.PageTopToBottom,
		}[v.Direction]
		w.Apply(layout.Style{})
		b.EnterPageMode()
		if err := b.SetPageArea(v.X, v.Y, pw, ph); err != nil {
			return err
		}
		if err := b.SetPageDirection(dir); err != nil {
			return err
		}
		saved := w.Width
		w.Width = int(pw)
		if dir == escpos.PageBottomToTop || dir == escpos.PageTopToBottom {
			w.Width = int(ph)
		}
		r.inPage = true
		err := r.blocks(v.Blocks, sc, path+"/blocks", depth+1)
		r.inPage = false
		w.Width = saved
		w.Apply(layout.Style{})
		b.PrintPageAndExit()
		return err

	case "counter":
		var v struct {
			Type   string     `json:"type"`
			Set    *uint16    `json:"set"`
			From   *uint16    `json:"from"`
			To     *uint16    `json:"to"`
			Step   *uint8     `json:"step"`
			Repeat *uint8     `json:"repeat"`
			Digits uint8      `json:"digits"`
			Pad    string     `json:"pad"`
			Prefix string     `json:"prefix"`
			Style  *jsonStyle `json:"style"`
			Align  *string    `json:"align"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		pad := map[string]escpos.CounterAlign{
			"": escpos.CounterAlignRightSpaces, "spaces": escpos.CounterAlignRightSpaces,
			"zeros": escpos.CounterAlignRightZeros, "left": escpos.CounterAlignLeftSpaces,
		}[v.Pad]
		if err := b.SetCounterPrintMode(v.Digits, pad); err != nil {
			return err
		}
		if v.From != nil || v.To != nil {
			from, to := ptrOr(v.From, 0), ptrOr(v.To, 65535)
			b.SetCounterModeA(from, to, ptrOr(v.Step, 1), ptrOr(v.Repeat, 1))
		}
		if v.Set != nil {
			b.SetCounter(*v.Set)
		}
		w.SetAlign(parseAlign(v.Align, sc.align))
		st := v.Style.apply(sc.style)
		w.Apply(st)
		b.Text(ForCodePage437(Clean(v.Prefix)))
		b.PrintCounter()
		b.LineFeed()

	case "datetime":
		var v struct {
			Type     string     `json:"type"`
			Format   string     `json:"format"`
			Timezone string     `json:"timezone"`
			Prefix   string     `json:"prefix"`
			Style    *jsonStyle `json:"style"`
			Align    *string    `json:"align"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		now := r.env.Now()
		if v.Timezone != "" {
			loc, err := time.LoadLocation(v.Timezone)
			if err != nil {
				return fmt.Errorf("unknown time zone %q", v.Timezone)
			}
			now = now.In(loc)
		}
		format := map[string]string{
			"": "2006-01-02 15:04", "datetime": "2006-01-02 15:04", "date": "2006-01-02",
			"time": "15:04", "rfc3339": time.RFC3339,
		}[v.Format]
		if format == "" {
			format = v.Format
		}
		s := Clean(v.Prefix) + now.Format(format)
		return r.text([]layout.Span{{Text: s, Style: v.Style.apply(sc.style)}}, "", "", parseAlign(v.Align, sc.align), true)

	case "self_test":
		b.PrintTestPage()

	default:
		return fmt.Errorf("unknown block type %q", head.Type)
	}
	return nil
}

func ptrOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

func (r *docRenderer) table(t layout.Table) error {
	all := strings.Join(t.Headers, " ")
	for _, row := range t.Rows {
		all += " " + strings.Join(row, " ")
	}
	r.w.SetAlign(escpos.AlignLeft)
	if r.useUnicode(all) {
		cols := r.w.Width / 12
		var lines []string
		for _, l := range t.Layout(cols, UnicodeCells) {
			lines = append(lines, spansText(l))
		}
		return PrintUnicode(r.b, strings.Join(lines, "\n"), UnicodeOptions{Scale: 1.5, NoWrap: true})
	}
	r.w.Table(t)
	r.w.Apply(layout.Style{})
	return nil
}

var symbologies = map[string]escpos.BarcodeSystem{
	"UPC-A": escpos.BarcodeUPCA, "UPC-E": escpos.BarcodeUPCE, "EAN13": escpos.BarcodeEAN13,
	"EAN8": escpos.BarcodeEAN8, "CODE39": escpos.BarcodeCode39, "ITF": escpos.BarcodeITF,
	"CODABAR": escpos.BarcodeCodabar, "CODE93": escpos.BarcodeCode93, "CODE128": escpos.BarcodeCode128,
}

func (r *docRenderer) barcode(sym, content string, height, width uint8, hri, hriFont string, align escpos.Align) error {
	b := r.b
	sys, ok := symbologies[sym]
	if !ok {
		return fmt.Errorf("unknown symbology %q", sym)
	}
	if height == 0 {
		height = 80
	}
	if width == 0 {
		width = 3
	}
	pos := map[string]escpos.HRIPosition{
		"": escpos.HRIBelow, "below": escpos.HRIBelow, "above": escpos.HRIAbove,
		"both": escpos.HRIBoth, "none": escpos.HRINone,
	}[hri]
	font := escpos.FontA
	if hriFont == "B" {
		font = escpos.FontB
	}
	if sys == escpos.BarcodeCode128 && !strings.HasPrefix(content, "{") {
		content = escpos.Code128(content)
	}
	r.w.Apply(layout.Style{})
	r.w.SetAlign(align)
	for _, err := range []error{
		b.SetBarcodeHeight(height), b.SetBarcodeWidth(width), b.SetHRIPosition(pos), b.SetHRIFont(font),
	} {
		if err != nil {
			return err
		}
	}
	if err := b.PrintBarcode(sys, content); err != nil {
		return err
	}
	b.LineFeed()
	return nil
}
