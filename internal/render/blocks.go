package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/connordoman/escpos/unifont"
	"strconv"
	"strings"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/css"
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
	Feed       *uint8 `json:"feed,omitempty"`
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

// Finish returns the document's cut and peripheral settings. Without a
// feed of its own, a cut feeds defaultFeed dots first.
func (d *Document) Finish(defaultFeed uint8) Finish {
	cut, _ := ParseCutMode(d.Options.Cut)
	feed := uint8(0)
	if cut != CutNone {
		feed = defaultFeed
	}
	if d.Options.Feed != nil {
		feed = *d.Options.Feed
	}
	return Finish{Cut: cut, Feed: feed, OpenDrawer: d.Options.OpenDrawer, Beep: d.Options.Beep}
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

// scope is the computed style inherited from enclosing groups.
type scope struct {
	css css.Computed
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
	// marginLeft and width are the printer's current left margin and
	// printable width in dots (GS L, GS W); width 0 means the rest of the
	// paper.
	marginLeft, width int
}

// resolve computes a block's style from its CSS and the inherited style.
func (r *docRenderer) resolve(p *css.Properties, sc scope) (css.Computed, error) {
	c, err := p.Resolve(sc.css)
	if err != nil {
		return c, fmt.Errorf("style: %w", err)
	}
	return c, nil
}

// around applies the block-level parts of c (lineHeight, marginLeft,
// width) while fn prints, then restores parent's.
func (r *docRenderer) around(c, parent css.Computed, fn func() error) error {
	if c.LineHeight != parent.LineHeight {
		setLineHeight(r.b, c.LineHeight)
		defer setLineHeight(r.b, parent.LineHeight)
	}
	if c.MarginLeft > 0 || c.Width > 0 {
		left, width := r.marginLeft, r.areaWidth()
		w := width - c.MarginLeft
		if c.Width > 0 {
			w = min(c.Width, w)
		}
		r.setMargins(left+c.MarginLeft, max(w, 8))
		defer r.setMargins(left, width)
	}
	return fn()
}

func (r *docRenderer) areaWidth() int {
	if r.width > 0 {
		return r.width
	}
	return r.env.PaperWidth - r.marginLeft
}

func (r *docRenderer) setMargins(left, width int) {
	r.b.SetLeftMargin(uint16(left))
	r.b.SetPrintAreaWidth(uint16(width))
	r.marginLeft, r.width = left, width
	r.w.Width = width
}

func setLineHeight(b *escpos.Builder, dots int) {
	if dots == 0 {
		b.DefaultLineSpacing()
	} else {
		b.SetLineSpacing(uint8(dots))
	}
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
	f := doc.Finish(env.CutFeed)
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
		return unifont.Print(r.b, spansText(spans), unifont.Options{
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
				Content string          `json:"content"`
				Style   *css.Properties `json:"style"`
			} `json:"spans"`
			Style *css.Properties `json:"style"`
			Align *string         `json:"align"`
			Wrap  *bool           `json:"wrap"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		c, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		var spans []layout.Span
		if v.Content != nil {
			spans = []layout.Span{{Text: c.Text(Clean(*v.Content)), Style: c.Style}}
		}
		for i, s := range v.Spans {
			sc, err := r.resolve(s.Style, scope{c})
			if err != nil {
				return fmt.Errorf("spans/%d: %w", i, err)
			}
			spans = append(spans, layout.Span{Text: sc.Text(Clean(s.Content)), Style: sc.Style})
		}
		wrap := !c.NoWrap
		if v.Wrap != nil {
			wrap = *v.Wrap
		}
		return r.around(c, sc.css, func() error {
			return r.text(spans, "", "", parseAlign(v.Align, c.Align), wrap)
		})

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
		return r.text([]layout.Span{{Text: Clean(v.Content), Style: st}}, "", "", parseAlign(v.Align, sc.css.Align), true)

	case "alert":
		var v struct {
			Type     string          `json:"type"`
			Variant  string          `json:"variant"`
			Title    string          `json:"title"`
			Content  string          `json:"content"`
			Markdown string          `json:"markdown"`
			Links    string          `json:"links"`
			Style    *css.Properties `json:"style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		as, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		title := v.Title
		if title == "" {
			title = v.Variant
		}
		um := r.unicode
		if r.inPage {
			um = UnicodeTransliterate
		}
		w.SetAlign(escpos.AlignLeft)
		return r.around(as, sc.css, func() error {
			err := WriteAlert(r.ctx, r.env, w, Alert{
				Title:  title,
				Double: as.Border == "double",
				// The box takes the style's weight; the body inherits
				// everything, as text in a bordered element does.
				Border:   layout.Style{Bold: as.Style.Bold, DoubleStrike: as.Style.DoubleStrike},
				Base:     as.Style,
				Text:     as.Text(v.Content),
				Markdown: v.Markdown,
			}, MarkdownOptions{Links: LinkMode(v.Links), Unicode: um, Images: !r.inPage})
			w.Apply(layout.Style{})
			return err
		})

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
		return unifont.Print(b, v.Content, unifont.Options{
			Scale: v.Scale, Bold: v.Bold, Invert: v.Invert, Align: parseAlign(v.Align, sc.css.Align),
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
			Type  string          `json:"type"`
			Char  string          `json:"char"`
			Style *css.Properties `json:"style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		c := '─'
		if v.Char != "" {
			c = []rune(Clean(v.Char) + "-")[0]
		}
		st, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		w.SetAlign(escpos.AlignLeft)
		return r.around(st, sc.css, func() error { w.Rule(c, st.Style); return nil })

	case "key_value":
		var v struct {
			Type       string          `json:"type"`
			Key        string          `json:"key"`
			Value      string          `json:"value"`
			Leader     string          `json:"leader"`
			KeyStyle   *css.Properties `json:"key_style"`
			ValueStyle *css.Properties `json:"value_style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		leader := ' '
		if v.Leader != "" {
			leader = []rune(Clean(v.Leader) + " ")[0]
		}
		ks, err := r.resolve(v.KeyStyle, sc)
		if err != nil {
			return fmt.Errorf("key_%w", err)
		}
		vs, err := r.resolve(v.ValueStyle, sc)
		if err != nil {
			return fmt.Errorf("value_%w", err)
		}
		w.SetAlign(escpos.AlignLeft)
		w.KeyValue(ks.Text(Clean(v.Key)), vs.Text(Clean(v.Value)), leader, ks.Style, vs.Style)

	case "columns":
		var v struct {
			Type    string `json:"type"`
			Columns []struct {
				Content string  `json:"content"`
				Align   *string `json:"align"`
				Width   int     `json:"width"`
			} `json:"columns"`
			Style  *css.Properties `json:"style"`
			Border bool            `json:"border"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		cs, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		t := layout.Table{Border: v.Border, Style: cs.Style, Rows: [][]string{nil}}
		for _, c := range v.Columns {
			t.Rows[0] = append(t.Rows[0], cs.Text(Clean(c.Content)))
			t.Aligns = append(t.Aligns, parseAlign(c.Align, escpos.AlignLeft))
			t.Weights = append(t.Weights, max(c.Width, 1))
		}
		// Built as a table rather than with Writer.Columns so the Unicode
		// fallback in r.table applies.
		return r.around(cs, sc.css, func() error { return r.table(t) })

	case "table":
		var v struct {
			Type        string          `json:"type"`
			Headers     []string        `json:"headers"`
			Rows        [][]any         `json:"rows"`
			Align       []string        `json:"align"`
			Border      bool            `json:"border"`
			Style       *css.Properties `json:"style"`
			HeaderStyle *css.Properties `json:"header_style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		ts, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		// Headers are bold unless their style says otherwise.
		headerParent := ts
		headerParent.Style.Bold = true
		hc, err := r.resolve(v.HeaderStyle, scope{headerParent})
		if err != nil {
			return fmt.Errorf("header_%w", err)
		}
		hs := hc.Style
		// Header and body must share character widths to line up.
		hs.FontB, hs.Width, hs.Spacing = ts.Style.FontB, ts.Style.Width, ts.Style.Spacing
		t := layout.Table{Border: v.Border, Style: ts.Style, HeaderStyle: hs}
		for _, h := range v.Headers {
			t.Headers = append(t.Headers, hc.Text(Clean(h)))
		}
		for _, a := range v.Align {
			t.Aligns = append(t.Aligns, ParseAlign(a))
		}
		for _, row := range v.Rows {
			var cells []string
			for _, c := range row {
				switch c := c.(type) {
				case string:
					cells = append(cells, ts.Text(Clean(c)))
				case float64:
					cells = append(cells, strconv.FormatFloat(c, 'f', -1, 64))
				default:
					cells = append(cells, fmt.Sprint(c))
				}
			}
			t.Rows = append(t.Rows, cells)
		}
		return r.around(ts, sc.css, func() error { return r.table(t) })

	case "list":
		var v struct {
			Type    string          `json:"type"`
			Items   []string        `json:"items"`
			Ordered bool            `json:"ordered"`
			Start   *int            `json:"start"`
			Marker  string          `json:"marker"`
			Style   *css.Properties `json:"style"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		ls, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		n := 1
		if v.Start != nil {
			n = *v.Start
		}
		marker := "-"
		if v.Marker != "" {
			marker = Clean(v.Marker)
		}
		return r.around(ls, sc.css, func() error {
			for _, item := range v.Items {
				m := marker + " "
				if v.Ordered {
					m = fmt.Sprintf("%d. ", n)
					n++
				}
				if err := r.text([]layout.Span{{Text: ls.Text(Clean(item)), Style: ls.Style}}, m, strings.Repeat(" ", len([]rune(m))), escpos.AlignLeft, !ls.NoWrap); err != nil {
					return err
				}
			}
			return nil
		})

	case "box":
		var v struct {
			Type    string          `json:"type"`
			Content string          `json:"content"`
			Style   *css.Properties `json:"style"`
			Align   *string         `json:"align"`
			Double  bool            `json:"double"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		bs, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		return r.around(bs, sc.css, func() error {
			w.Box(bs.Text(Clean(v.Content)), bs.Style, parseAlign(v.Align, bs.Align), v.Double)
			return nil
		})

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
			Feed    *uint8 `json:"feed"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		mode := v.Mode
		if mode == "" {
			mode = v.Content
		}
		w.Apply(layout.Style{})
		feed := ptrOr(v.Feed, r.env.CutFeed)
		if strings.EqualFold(mode, "full") {
			b.FeedAndFullCut(feed)
		} else {
			b.FeedAndCut(feed)
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
		width := max(r.env.PaperWidth-int(v.Left), 8)
		if v.Width != nil && *v.Width > 0 {
			width = min(int(*v.Width), width)
		}
		r.setMargins(int(v.Left), width)

	case "group":
		var v struct {
			Type   string            `json:"type"`
			Style  *css.Properties   `json:"style"`
			Align  *string           `json:"align"`
			Blocks []json.RawMessage `json:"blocks"`
		}
		if err := decode(&v); err != nil {
			return err
		}
		gs, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		gs.Align = parseAlign(v.Align, gs.Align)
		// Children inherit the group's style; its line height and margins
		// apply while they print.
		return r.around(gs, sc.css, func() error {
			inner := gs
			inner.MarginLeft, inner.Width = 0, 0
			return r.blocks(v.Blocks, scope{inner}, path+"/blocks", depth+1)
		})

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
			Type   string          `json:"type"`
			Set    *uint16         `json:"set"`
			From   *uint16         `json:"from"`
			To     *uint16         `json:"to"`
			Step   *uint8          `json:"step"`
			Repeat *uint8          `json:"repeat"`
			Digits uint8           `json:"digits"`
			Pad    string          `json:"pad"`
			Prefix string          `json:"prefix"`
			Style  *css.Properties `json:"style"`
			Align  *string         `json:"align"`
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
		cs, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		return r.around(cs, sc.css, func() error {
			w.SetAlign(parseAlign(v.Align, cs.Align))
			w.Apply(cs.Style)
			b.Text(cs.Text(Clean(v.Prefix)))
			b.PrintCounter()
			b.LineFeed()
			return nil
		})

	case "datetime":
		var v struct {
			Type     string          `json:"type"`
			Format   string          `json:"format"`
			Timezone string          `json:"timezone"`
			Prefix   string          `json:"prefix"`
			Style    *css.Properties `json:"style"`
			Align    *string         `json:"align"`
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
		ds, err := r.resolve(v.Style, sc)
		if err != nil {
			return err
		}
		s := ds.Text(Clean(v.Prefix) + now.Format(format))
		return r.around(ds, sc.css, func() error {
			return r.text([]layout.Span{{Text: s, Style: ds.Style}}, "", "", parseAlign(v.Align, ds.Align), !ds.NoWrap)
		})

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
		for _, l := range t.Layout(cols, unifont.Cells) {
			lines = append(lines, spansText(l))
		}
		return unifont.Print(r.b, strings.Join(lines, "\n"), unifont.Options{Scale: 1.5, NoWrap: true})
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
