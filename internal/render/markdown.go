package render

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/connordoman/escpos"
	"github.com/yuin/goldmark"
	emoji "github.com/yuin/goldmark-emoji"
	emojiast "github.com/yuin/goldmark-emoji/ast"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// LinkMode chooses how Markdown links are printed.
type LinkMode string

const (
	LinksInline    LinkMode = "inline"    // text (url), in the small font
	LinksFootnotes LinkMode = "footnotes" // text[1], with URLs listed at the end
	LinksQR        LinkMode = "qr"        // text[1], with a QR code per URL at the end
	LinksHide      LinkMode = "hide"      // text only
)

// UnicodeMode chooses how text the printer's fonts cannot show is printed.
type UnicodeMode string

const (
	// UnicodeImage draws blocks containing such text (emoji, CJK, ...) as
	// images with Unifont.
	UnicodeImage UnicodeMode = "image"
	// UnicodeTransliterate prints the closest ASCII, or '?'.
	UnicodeTransliterate UnicodeMode = "transliterate"
)

// MarkdownOptions controls Markdown rendering.
type MarkdownOptions struct {
	Links   LinkMode
	Unicode UnicodeMode
	// Images enables printing ![images](url). When false, alt text is
	// printed instead.
	Images bool
}

var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM, // tables, strikethrough, autolinks, task lists
		extension.Footnote,
		emoji.Emoji, // :shortcodes:
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// Paragraph spacing in dots (half a Font A line).
const blockGap = 12

// container is a block quote or list item, which prefixes the lines of the
// blocks inside it.
type container struct {
	first, rest string
	used        bool
}

type link struct {
	text, url string
}

type mdRenderer struct {
	ctx  context.Context
	env  *Env
	src  []byte
	w    *Writer
	opts MarkdownOptions

	containers []container
	links      []link
	pendingGap bool
	printed    bool
}

// WriteMarkdown renders GitHub-flavoured Markdown with w.
func WriteMarkdown(ctx context.Context, env *Env, w *Writer, src string, opts MarkdownOptions) error {
	if opts.Links == "" {
		opts.Links = LinksInline
	}
	if opts.Unicode == "" {
		opts.Unicode = UnicodeImage
	}
	source := []byte(Clean(src))
	r := &mdRenderer{ctx: ctx, env: env, src: source, w: w, opts: opts}
	doc := markdown.Parser().Parse(text.NewReader(source))
	if err := r.block(doc); err != nil {
		return err
	}
	return r.printLinks()
}

func (r *mdRenderer) gap() { r.pendingGap = r.printed }

func (r *mdRenderer) flushGap() {
	if r.pendingGap {
		r.pendingGap = false
		if r.inQuote() {
			// Keep the quote bar unbroken across paragraphs.
			first, _ := r.prefixes()
			r.w.Line(Span{Text: strings.TrimRight(first, " ")})
		} else {
			r.w.B.FeedUnits(blockGap)
		}
	}
	r.printed = true
}

const quoteBar = "│ "

func (r *mdRenderer) inQuote() bool {
	for _, c := range r.containers {
		if c.first == quoteBar {
			return true
		}
	}
	return false
}

func (r *mdRenderer) prefixes() (first, rest string) {
	var f, s strings.Builder
	for i := range r.containers {
		c := &r.containers[i]
		if c.used {
			f.WriteString(c.rest)
		} else {
			f.WriteString(c.first)
		}
		s.WriteString(c.rest)
		c.used = true
	}
	return f.String(), s.String()
}

func spansText(spans []Span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// leaf prints one block of inline text.
func (r *mdRenderer) leaf(spans []Span, align escpos.Align, scale float64) error {
	r.flushGap()
	first, rest := r.prefixes()
	if r.opts.Unicode == UnicodeImage && NeedsUnicode(spansText(spans)) {
		bold := false
		for _, s := range spans {
			bold = bold || s.Style.Bold
		}
		o := UnicodeOptions{
			Scale: 1.5 * scale, Bold: bold, Align: align,
			FirstPrefix: first, RestPrefix: rest,
		}
		if r.inQuote() {
			zero := 0
			o.LineGap = &zero
		}
		return PrintUnicode(r.w.B, spansText(spans), o)
	}
	r.w.SetAlign(align)
	r.w.Paragraph(spans, []Span{{Text: first}}, []Span{{Text: rest}})
	r.w.Apply(Style{})
	r.w.SetAlign(escpos.AlignLeft)
	return nil
}

func headingStyle(level int) (Style, float64) {
	switch level {
	case 1:
		return Style{Bold: true, Width: 2, Height: 2}, 2
	case 2:
		return Style{Bold: true, Height: 2}, 1.5
	case 3:
		return Style{Bold: true, Underline: 1}, 1
	}
	return Style{Bold: true}, 1
}

func (r *mdRenderer) children(n ast.Node) error {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if err := r.block(c); err != nil {
			return err
		}
	}
	return nil
}

func (r *mdRenderer) block(n ast.Node) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	switch n := n.(type) {
	case *ast.Document:
		return r.children(n)

	case *ast.Heading:
		st, scale := headingStyle(n.Level)
		if err := r.leaf(r.inlines(n, st), escpos.AlignLeft, scale); err != nil {
			return err
		}
		r.gap()

	case *ast.Paragraph:
		if imgs := imageOnly(n, r.src); imgs != nil && r.opts.Images {
			if err := r.images(imgs); err != nil {
				return err
			}
		} else if err := r.leaf(r.inlines(n, Style{}), escpos.AlignLeft, 1); err != nil {
			return err
		}
		r.gap()

	case *ast.TextBlock: // paragraph in a tight list
		if err := r.leaf(r.inlines(n, Style{}), escpos.AlignLeft, 1); err != nil {
			return err
		}

	case *ast.ThematicBreak:
		r.flushGap()
		first, _ := r.prefixes()
		r.w.Line(Span{Text: first + strings.Repeat("─", max(r.w.Width/12-len([]rune(first)), 1))})
		r.gap()

	case *ast.CodeBlock, *ast.FencedCodeBlock:
		r.flushGap()
		var code strings.Builder
		lines := n.Lines()
		for i := range lines.Len() {
			seg := lines.At(i)
			code.Write(seg.Value(r.src))
		}
		body := strings.TrimRight(code.String(), "\n")
		first, rest := r.prefixes()
		if r.opts.Unicode == UnicodeImage && NeedsUnicode(body) {
			if err := PrintUnicode(r.w.B, body, UnicodeOptions{Scale: 1, FirstPrefix: first, RestPrefix: rest}); err != nil {
				return err
			}
		} else {
			for i, line := range strings.Split(body, "\n") {
				p := rest
				if i == 0 {
					p = first
				}
				r.w.Paragraph([]Span{{Text: line, Style: Style{FontB: true}}}, []Span{{Text: p}}, []Span{{Text: rest}})
			}
			r.w.Apply(Style{})
		}
		r.gap()

	case *ast.Blockquote:
		r.flushGap()
		outer := !r.inQuote()
		if outer {
			// At Font A's height the │ bars of consecutive lines join.
			r.w.B.SetLineSpacing(barLineSpacing(Style{}))
		}
		r.containers = append(r.containers, container{first: quoteBar, rest: quoteBar})
		err := r.children(n)
		r.containers = r.containers[:len(r.containers)-1]
		if outer {
			r.w.B.DefaultLineSpacing()
		}
		if err != nil {
			return err
		}
		r.gap()

	case *ast.List:
		r.flushGap()
		depth := 0
		for _, c := range r.containers {
			if c.first != quoteBar {
				depth++
			}
		}
		num := n.Start
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			marker := []string{"- ", "* ", "+ "}[depth%3]
			if n.IsOrdered() {
				marker = fmt.Sprintf("%d. ", num)
				num++
			}
			r.containers = append(r.containers, container{first: marker, rest: strings.Repeat(" ", len(marker))})
			err := r.children(item)
			r.containers = r.containers[:len(r.containers)-1]
			if err != nil {
				return err
			}
		}
		r.gap()

	case *east.Table:
		r.flushGap()
		if err := r.table(n); err != nil {
			return err
		}
		r.gap()

	case *east.FootnoteList:
		r.flushGap()
		r.w.Rule('─', Style{FontB: true})
		for fn := n.FirstChild(); fn != nil; fn = fn.NextSibling() {
			idx := 0
			if f, ok := fn.(*east.Footnote); ok {
				idx = f.Index
			}
			marker := fmt.Sprintf("[^%d] ", idx)
			r.containers = append(r.containers, container{first: marker, rest: strings.Repeat(" ", len(marker))})
			err := r.children(fn)
			r.containers = r.containers[:len(r.containers)-1]
			if err != nil {
				return err
			}
		}

	case *ast.HTMLBlock:
		// Raw HTML has no printed form.

	default:
		return r.children(n)
	}
	return nil
}

// imageOnly returns the images in a paragraph that holds nothing else.
func imageOnly(p ast.Node, src []byte) []*ast.Image {
	var imgs []*ast.Image
	for c := p.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Image:
			imgs = append(imgs, c)
		case *ast.Text:
			if len(bytes.TrimSpace(c.Segment.Value(src))) != 0 || c.HardLineBreak() {
				return nil
			}
		default:
			return nil
		}
	}
	return imgs
}

func (r *mdRenderer) images(imgs []*ast.Image) error {
	for _, im := range imgs {
		img, err := r.env.loadImage(r.ctx, string(im.Destination))
		if err != nil {
			return err
		}
		r.flushGap()
		if err := PrintImage(r.w.B, img, ImagePlacement{Align: escpos.AlignCenter, Dither: true}); err != nil {
			return err
		}
	}
	return nil
}

func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(c.Value)
		case *ast.AutoLink:
			b.Write(c.Label(src))
			return ast.WalkSkipChildren, nil
		case *emojiast.Emoji:
			b.WriteString(string(c.Value.Unicode))
		case *east.TaskCheckBox:
			if c.IsChecked {
				b.WriteString("[x] ")
			} else {
				b.WriteString("[ ] ")
			}
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

func (r *mdRenderer) table(n *east.Table) error {
	t := Table{HeaderStyle: Style{Bold: true}}
	for _, a := range n.Alignments {
		switch a {
		case east.AlignCenter:
			t.Aligns = append(t.Aligns, escpos.AlignCenter)
		case east.AlignRight:
			t.Aligns = append(t.Aligns, escpos.AlignRight)
		default:
			t.Aligns = append(t.Aligns, escpos.AlignLeft)
		}
	}
	unicode := false
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			s := strings.TrimSpace(plainText(cell, r.src))
			unicode = unicode || NeedsUnicode(s)
			cells = append(cells, s)
		}
		if _, ok := row.(*east.TableHeader); ok {
			t.Headers = cells
		} else {
			t.Rows = append(t.Rows, cells)
		}
	}
	first, rest := r.prefixes()
	if r.opts.Unicode == UnicodeImage && unicode {
		// Lay the table out in Unifont cells; at scale 1.5 a cell is the
		// same 12 dots as a Font A character.
		cols := r.w.Width/12 - UnicodeCells(first)
		var lines []string
		for _, l := range t.layout(cols, UnicodeCells) {
			lines = append(lines, spansText(l))
		}
		return PrintUnicode(r.w.B, strings.Join(lines, "\n"), UnicodeOptions{
			Scale: 1.5, NoWrap: true, FirstPrefix: first, RestPrefix: rest,
		})
	}
	if first == "" && rest == "" {
		r.w.Table(t)
		return nil
	}
	// Inside a list or quote: lay out narrower and add the prefixes.
	cols := r.w.Width/12 - len([]rune(rest))
	measure := func(s string) int { return len([]rune(r.w.prepare(s))) }
	for i, l := range t.layout(cols, measure) {
		p := rest
		if i == 0 {
			p = first
		}
		r.w.printAtomsKeepSpaces(r.w.atoms(append([]Span{{Text: p}}, l...)))
	}
	r.w.Apply(Style{})
	return nil
}

// inlines flattens the inline content of n into styled spans.
func (r *mdRenderer) inlines(n ast.Node, base Style) []Span {
	var out []Span
	r.inline(n, base, &out)
	// Collapse runs of whitespace left by soft breaks.
	for i := range out {
		if out[i].Text != "\n" {
			out[i].Text = strings.Join(strings.FieldsFunc(out[i].Text, func(c rune) bool { return c == '\n' }), " ")
		}
	}
	return out
}

func (r *mdRenderer) inline(n ast.Node, st Style, out *[]Span) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Text:
			*out = append(*out, Span{string(c.Segment.Value(r.src)), st})
			if c.HardLineBreak() {
				*out = append(*out, Span{"\n", st})
			} else if c.SoftLineBreak() {
				*out = append(*out, Span{" ", st})
			}
		case *ast.String:
			*out = append(*out, Span{string(c.Value), st})
		case *ast.CodeSpan:
			s := st
			s.Invert = true
			r.inline(c, s, out)
		case *ast.Emphasis:
			s := st
			if c.Level >= 2 {
				s.Bold = true
			} else {
				s.Underline = 1
			}
			r.inline(c, s, out)
		case *east.Strikethrough:
			// Printers cannot strike text through; keep the markers.
			*out = append(*out, Span{"~", st})
			r.inline(c, st, out)
			*out = append(*out, Span{"~", st})
		case *ast.Link:
			label := plainText(c, r.src)
			r.inline(c, st, out)
			r.addLink(label, string(c.Destination), st, out)
		case *ast.AutoLink:
			label := string(c.Label(r.src))
			*out = append(*out, Span{label, st})
			r.addLink(label, string(c.URL(r.src)), st, out)
		case *ast.Image:
			*out = append(*out, Span{"[image: " + plainText(c, r.src) + "]", st})
		case *ast.RawHTML:
			var raw strings.Builder
			for i := range c.Segments.Len() {
				seg := c.Segments.At(i)
				raw.Write(seg.Value(r.src))
			}
			tag := strings.ToLower(strings.ReplaceAll(raw.String(), " ", ""))
			if tag == "<br>" || tag == "<br/>" {
				*out = append(*out, Span{"\n", st})
			}
		case *east.TaskCheckBox:
			if c.IsChecked {
				*out = append(*out, Span{"[x] ", st})
			} else {
				*out = append(*out, Span{"[ ] ", st})
			}
		case *emojiast.Emoji:
			*out = append(*out, Span{string(c.Value.Unicode), st})
		case *east.FootnoteLink:
			*out = append(*out, Span{fmt.Sprintf("[^%d]", c.Index), st})
		case *east.FootnoteBacklink:
		default:
			r.inline(c, st, out)
		}
	}
}

func (r *mdRenderer) addLink(label, url string, st Style, out *[]Span) {
	if url == "" || url == label || strings.TrimPrefix(url, "mailto:") == label {
		if r.opts.Links != LinksQR {
			return
		}
	}
	switch r.opts.Links {
	case LinksInline:
		*out = append(*out, Span{" (" + url + ")", Style{FontB: true, Bold: st.Bold}})
	case LinksFootnotes, LinksQR:
		r.links = append(r.links, link{label, url})
		*out = append(*out, Span{fmt.Sprintf("[%d]", len(r.links)), st})
	}
}

func (r *mdRenderer) printLinks() error {
	if len(r.links) == 0 {
		return nil
	}
	r.pendingGap = true
	r.flushGap()
	r.w.Rule('─', Style{FontB: true})
	for i, l := range r.links {
		switch r.opts.Links {
		case LinksFootnotes:
			r.w.Paragraph([]Span{{l.url, Style{FontB: true}}}, []Span{{fmt.Sprintf("[%d] ", i+1), Style{FontB: true}}}, []Span{{"    ", Style{FontB: true}}})
		case LinksQR:
			r.w.Paragraph([]Span{{l.text, Style{}}}, []Span{{fmt.Sprintf("[%d] ", i+1), Style{Bold: true}}}, []Span{{"    ", Style{}}})
			r.w.Apply(Style{})
			r.w.SetAlign(escpos.AlignCenter)
			if err := r.w.B.PrintQRCode(l.url, escpos.QRErrorM, 4); err != nil {
				return fmt.Errorf("link %d: %w", i+1, err)
			}
			r.w.B.LineFeed()
			r.w.Line(Span{l.url, Style{FontB: true}})
			r.w.SetAlign(escpos.AlignLeft)
		}
	}
	r.w.Apply(Style{})
	return nil
}
