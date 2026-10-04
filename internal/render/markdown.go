package render

import (
	"bytes"
	"context"
	"fmt"
	"github.com/connordoman/escpos/unifont"
	"strings"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/layout"
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

// container is a block quote, alert or list item, which surrounds the
// lines of the blocks inside it: first before the first line, rest before
// the others, and right against the right edge (an alert's box).
type container struct {
	first, rest, right string
	style              layout.Style // for the container's own marks
	used               bool
	// bar marks containers drawn with vertical lines, which need tight
	// line spacing to join up.
	bar bool
}

type link struct {
	text, url string
}

type mdRenderer struct {
	ctx  context.Context
	env  *Env
	src  []byte
	w    *layout.Writer
	opts MarkdownOptions

	containers []container
	// base is the style of body text, inherited from an enclosing block.
	base       layout.Style
	links      []link
	pendingGap bool
	printed    bool
}

// WriteMarkdown renders GitHub-flavoured Markdown with w.
func WriteMarkdown(ctx context.Context, env *Env, w *layout.Writer, src string, opts MarkdownOptions) error {
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
		if r.inFrame() {
			// Keep quote bars and alert boxes unbroken across paragraphs.
			e := r.prefixes()
			r.w.LineFramed(trimSpans(e.first), e.right)
		} else {
			r.w.B.FeedUnits(blockGap)
		}
	}
	r.printed = true
}

const quoteBar = "│ "

// inFrame reports whether the current block is inside a quote or alert.
func (r *mdRenderer) inFrame() bool {
	for _, c := range r.containers {
		if c.bar {
			return true
		}
	}
	return false
}

// edges is what surrounds the lines of a block: first before its first
// line, rest before the others, right at the right edge of each.
type edges struct {
	first, rest, right []layout.Span
}

// text returns the edges without their styles.
func (e edges) text() (first, rest, right string) {
	return spansText(e.first), spansText(e.rest), spansText(e.right)
}

// prefixes returns the edges of the next block.
func (r *mdRenderer) prefixes() edges {
	var e edges
	for i := range r.containers {
		c := &r.containers[i]
		t := c.first
		if c.used {
			t = c.rest
		}
		e.first = append(e.first, layout.Span{Text: t, Style: c.style})
		e.rest = append(e.rest, layout.Span{Text: c.rest, Style: c.style})
		c.used = true
	}
	for i := len(r.containers) - 1; i >= 0; i-- {
		c := r.containers[i]
		e.right = append(e.right, layout.Span{Text: c.right, Style: c.style})
	}
	return e
}

// trimSpans drops trailing spaces from spans.
func trimSpans(spans []layout.Span) []layout.Span {
	out := append([]layout.Span{}, spans...)
	for len(out) > 0 {
		last := &out[len(out)-1]
		last.Text = strings.TrimRight(last.Text, " ")
		if last.Text != "" {
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

func spansText(spans []layout.Span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// leaf prints one block of inline text.
func (r *mdRenderer) leaf(spans []layout.Span, align escpos.Align, scale float64) error {
	r.flushGap()
	e := r.prefixes()
	first, rest, right := e.text()
	if r.opts.Unicode == UnicodeImage && NeedsUnicode(spansText(spans)) {
		bold := false
		for _, s := range spans {
			bold = bold || s.Style.Bold
		}
		o := unifont.Options{
			Scale: 1.5 * scale, Bold: bold, Align: align,
			FirstPrefix: first, RestPrefix: rest, Suffix: right,
		}
		if r.inFrame() {
			zero := 0
			o.LineGap = &zero
		}
		return unifont.Print(r.w.B, spansText(spans), o)
	}
	r.w.SetAlign(align)
	if right != "" {
		align = escpos.AlignLeft // the right edge is placed by position
	}
	r.w.SetAlign(align)
	r.w.ParagraphFramed(spans, e.first, e.rest, e.right)
	r.w.Apply(layout.Style{})
	r.w.SetAlign(escpos.AlignLeft)
	return nil
}

func headingStyle(level int) (layout.Style, float64) {
	switch level {
	case 1:
		return layout.Style{Bold: true, Width: 2, Height: 2}, 2
	case 2:
		return layout.Style{Bold: true, Height: 2}, 1.5
	case 3:
		return layout.Style{Bold: true, Underline: 1}, 1
	}
	return layout.Style{Bold: true}, 1
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
		} else if err := r.leaf(r.inlines(n, r.base), escpos.AlignLeft, 1); err != nil {
			return err
		}
		r.gap()

	case *ast.TextBlock: // paragraph in a tight list
		if err := r.leaf(r.inlines(n, r.base), escpos.AlignLeft, 1); err != nil {
			return err
		}

	case *ast.ThematicBreak:
		r.flushGap()
		e := r.prefixes()
		first, _, right := e.text()
		cells := r.w.Width/12 - r.w.Measure(first) - r.w.Measure(right)
		r.w.LineFramed(append(e.first, layout.Span{Text: strings.Repeat("─", max(cells, 1))}), e.right)
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
		e := r.prefixes()
		first, rest, right := e.text()
		if r.opts.Unicode == UnicodeImage && NeedsUnicode(body) {
			if err := unifont.Print(r.w.B, body, unifont.Options{Scale: 1, FirstPrefix: first, RestPrefix: rest, Suffix: right}); err != nil {
				return err
			}
		} else {
			for i, line := range strings.Split(body, "\n") {
				p := e.rest
				if i == 0 {
					p = e.first
				}
				r.w.ParagraphFramed([]layout.Span{{Text: line, Style: layout.Style{FontB: true}}}, p, e.rest, e.right)
			}
			r.w.Apply(layout.Style{})
		}
		r.gap()

	case *ast.Blockquote:
		r.flushGap()
		if kind := r.alertKind(n); kind != "" {
			if err := r.box(kind, singleLines, layout.Style{}, func() error { return r.children(n) }); err != nil {
				return err
			}
			r.gap()
			break
		}
		outer := !r.inFrame()
		if outer {
			// At Font A's height the │ bars of consecutive lines join.
			r.w.B.SetLineSpacing(layout.BarLineSpacing(layout.Style{}))
		}
		r.containers = append(r.containers, container{first: quoteBar, rest: quoteBar, bar: true})
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
			if !c.bar {
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
		r.w.Rule('─', layout.Style{FontB: true})
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

// alertKinds are GitHub's alert types: > [!NOTE] and so on.
var alertKinds = map[string]bool{"NOTE": true, "TIP": true, "IMPORTANT": true, "WARNING": true, "CAUTION": true}

// alertKind reports whether q is a GitHub alert, a quote whose first line
// is [!NOTE], [!TIP], [!IMPORTANT], [!WARNING] or [!CAUTION], and if so
// removes that marker from it.
func (r *mdRenderer) alertKind(q *ast.Blockquote) string {
	p, ok := q.FirstChild().(*ast.Paragraph)
	if !ok {
		return ""
	}
	var marker []ast.Node
	var line strings.Builder
	for c := p.FirstChild(); c != nil; c = c.NextSibling() {
		t, ok := c.(*ast.Text)
		if !ok {
			return ""
		}
		line.Write(t.Segment.Value(r.src))
		marker = append(marker, c)
		if t.SoftLineBreak() || t.HardLineBreak() {
			break
		}
	}
	m := strings.TrimSpace(line.String())
	if !strings.HasPrefix(m, "[!") || !strings.HasSuffix(m, "]") {
		return ""
	}
	kind := strings.ToUpper(m[2 : len(m)-1])
	if !alertKinds[kind] {
		return ""
	}
	for _, n := range marker {
		p.RemoveChild(p, n)
	}
	if p.ChildCount() == 0 {
		q.RemoveChild(q, p)
	}
	return kind
}

// lines are the characters of a box.
type lines struct{ tl, tr, bl, br, h, v string }

var (
	singleLines = lines{"┌", "┐", "└", "┘", "─", "│"}
	doubleLines = lines{"╔", "╗", "╚", "╝", "═", "║"}
)

// box prints what body prints inside a box with title set into its top
// border, like an HTML fieldset, as for GitHub alerts:
//
//	┌─ NOTE ─────────┐
//	│ Contents.      │
//	└────────────────┘
//
// border styles the box's lines (bold for heavier lines); the title is
// also underlined.
func (r *mdRenderer) box(title string, ln lines, border layout.Style, body func() error) error {
	border.Underline, border.Invert = 0, false
	border.Width, border.Height = 1, 1 // the box is drawn on Font A's grid
	border.FontB = false
	outer := !r.inFrame()
	if outer {
		// At Font A's height the vertical lines of consecutive rows join.
		r.w.B.SetLineSpacing(layout.BarLineSpacing(layout.Style{}))
	}
	e := r.prefixes()
	first, _, right := e.text()
	cells := r.w.Width/12 - r.w.Measure(first) - r.w.Measure(right)
	titleStyle := border
	titleStyle.Underline = 1
	r.w.LineFramed(append(append([]layout.Span{}, e.first...),
		layout.Span{Text: ln.tl + ln.h + " ", Style: border},
		layout.Span{Text: title, Style: titleStyle},
		layout.Span{Text: " " + strings.Repeat(ln.h, max(cells-len([]rune(title))-5, 1)) + ln.tr, Style: border},
	), e.right)
	r.containers = append(r.containers, container{
		first: ln.v + " ", rest: ln.v + " ", right: " " + ln.v, style: border, bar: true,
	})
	err := body()
	r.containers = r.containers[:len(r.containers)-1]
	r.pendingGap = false // no blank line before the bottom border
	r.w.Apply(layout.Style{})
	r.w.LineFramed(append(append([]layout.Span{}, e.rest...),
		layout.Span{Text: ln.bl + strings.Repeat(ln.h, max(cells-2, 1)) + ln.br, Style: border},
	), e.right)
	r.w.Apply(layout.Style{})
	if outer {
		r.w.B.DefaultLineSpacing()
	}
	r.printed = true
	return err
}

// Alert is a boxed note for WriteAlert.
type Alert struct {
	Title    string // set into the top border, in capitals
	Double   bool   // double lines
	Border   layout.Style
	Base     layout.Style // body text
	Text     string       // plain text body
	Markdown string       // Markdown body, after Text
}

// WriteAlert prints a GitHub-style alert box with w.
func WriteAlert(ctx context.Context, env *Env, w *layout.Writer, a Alert, opts MarkdownOptions) error {
	if opts.Links == "" {
		opts.Links = LinksInline
	}
	if opts.Unicode == "" {
		opts.Unicode = UnicodeImage
	}
	r := &mdRenderer{ctx: ctx, env: env, w: w, opts: opts, base: a.Base}
	ln := singleLines
	if a.Double {
		ln = doubleLines
	}
	return r.box(strings.ToUpper(Clean(a.Title)), ln, a.Border, func() error {
		if a.Text != "" {
			if err := r.leaf([]layout.Span{{Text: Clean(a.Text), Style: a.Base}}, escpos.AlignLeft, 1); err != nil {
				return err
			}
			r.gap()
		}
		if a.Markdown != "" {
			r.src = []byte(Clean(a.Markdown))
			if err := r.block(markdown.Parser().Parse(text.NewReader(r.src))); err != nil {
				return err
			}
		}
		return nil
	})
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
	t := layout.Table{HeaderStyle: layout.Style{Bold: true}}
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
	e := r.prefixes()
	first, rest, right := e.text()
	if r.opts.Unicode == UnicodeImage && unicode {
		// Lay the table out in Unifont cells; at scale 1.5 a cell is the
		// same 12 dots as a Font A character.
		cols := r.w.Width/12 - unifont.Cells(first) - unifont.Cells(right)
		var lines []string
		for _, l := range t.Layout(cols, unifont.Cells) {
			lines = append(lines, spansText(l))
		}
		return unifont.Print(r.w.B, strings.Join(lines, "\n"), unifont.Options{
			Scale: 1.5, NoWrap: true, FirstPrefix: first, RestPrefix: rest, Suffix: right,
		})
	}
	if first == "" && rest == "" && right == "" {
		r.w.Table(t)
		return nil
	}
	// Inside a list, quote or alert: lay out narrower and add the prefixes.
	cols := r.w.Width/12 - r.w.Measure(rest) - r.w.Measure(right)
	for i, l := range t.Layout(cols, r.w.Measure) {
		p := e.rest
		if i == 0 {
			p = e.first
		}
		r.w.LineFramed(append(append([]layout.Span{}, p...), l...), e.right)
	}
	r.w.Apply(layout.Style{})
	return nil
}

// inlines flattens the inline content of n into styled spans.
func (r *mdRenderer) inlines(n ast.Node, base layout.Style) []layout.Span {
	var out []layout.Span
	r.inline(n, base, &out)
	// Collapse runs of whitespace left by soft breaks.
	for i := range out {
		if out[i].Text != "\n" {
			out[i].Text = strings.Join(strings.FieldsFunc(out[i].Text, func(c rune) bool { return c == '\n' }), " ")
		}
	}
	return out
}

func (r *mdRenderer) inline(n ast.Node, st layout.Style, out *[]layout.Span) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Text:
			*out = append(*out, layout.Span{Text: string(c.Segment.Value(r.src)), Style: st})
			if c.HardLineBreak() {
				*out = append(*out, layout.Span{Text: "\n", Style: st})
			} else if c.SoftLineBreak() {
				*out = append(*out, layout.Span{Text: " ", Style: st})
			}
		case *ast.String:
			*out = append(*out, layout.Span{Text: string(c.Value), Style: st})
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
			*out = append(*out, layout.Span{Text: "~", Style: st})
			r.inline(c, st, out)
			*out = append(*out, layout.Span{Text: "~", Style: st})
		case *ast.Link:
			label := plainText(c, r.src)
			r.inline(c, st, out)
			r.addLink(label, string(c.Destination), st, out)
		case *ast.AutoLink:
			label := string(c.Label(r.src))
			*out = append(*out, layout.Span{Text: label, Style: st})
			r.addLink(label, string(c.URL(r.src)), st, out)
		case *ast.Image:
			*out = append(*out, layout.Span{Text: "[image: " + plainText(c, r.src) + "]", Style: st})
		case *ast.RawHTML:
			var raw strings.Builder
			for i := range c.Segments.Len() {
				seg := c.Segments.At(i)
				raw.Write(seg.Value(r.src))
			}
			tag := strings.ToLower(strings.ReplaceAll(raw.String(), " ", ""))
			if tag == "<br>" || tag == "<br/>" {
				*out = append(*out, layout.Span{Text: "\n", Style: st})
			}
		case *east.TaskCheckBox:
			if c.IsChecked {
				*out = append(*out, layout.Span{Text: "[x] ", Style: st})
			} else {
				*out = append(*out, layout.Span{Text: "[ ] ", Style: st})
			}
		case *emojiast.Emoji:
			*out = append(*out, layout.Span{Text: string(c.Value.Unicode), Style: st})
		case *east.FootnoteLink:
			*out = append(*out, layout.Span{Text: fmt.Sprintf("[^%d]", c.Index), Style: st})
		case *east.FootnoteBacklink:
		default:
			r.inline(c, st, out)
		}
	}
}

func (r *mdRenderer) addLink(label, url string, st layout.Style, out *[]layout.Span) {
	if url == "" || url == label || strings.TrimPrefix(url, "mailto:") == label {
		if r.opts.Links != LinksQR {
			return
		}
	}
	switch r.opts.Links {
	case LinksInline:
		*out = append(*out, layout.Span{Text: " (" + url + ")", Style: layout.Style{FontB: true, Bold: st.Bold}})
	case LinksFootnotes, LinksQR:
		r.links = append(r.links, link{label, url})
		*out = append(*out, layout.Span{Text: fmt.Sprintf("[%d]", len(r.links)), Style: st})
	}
}

func (r *mdRenderer) printLinks() error {
	if len(r.links) == 0 {
		return nil
	}
	r.pendingGap = true
	r.flushGap()
	r.w.Rule('─', layout.Style{FontB: true})
	for i, l := range r.links {
		switch r.opts.Links {
		case LinksFootnotes:
			r.w.Paragraph([]layout.Span{{Text: l.url, Style: layout.Style{FontB: true}}}, []layout.Span{{Text: fmt.Sprintf("[%d] ", i+1), Style: layout.Style{FontB: true}}}, []layout.Span{{Text: "    ", Style: layout.Style{FontB: true}}})
		case LinksQR:
			r.w.Paragraph([]layout.Span{{Text: l.text, Style: layout.Style{}}}, []layout.Span{{Text: fmt.Sprintf("[%d] ", i+1), Style: layout.Style{Bold: true}}}, []layout.Span{{Text: "    ", Style: layout.Style{}}})
			r.w.Apply(layout.Style{})
			r.w.SetAlign(escpos.AlignCenter)
			if err := r.w.B.PrintQRCode(l.url, escpos.QRErrorM, 4); err != nil {
				return fmt.Errorf("link %d: %w", i+1, err)
			}
			r.w.B.LineFeed()
			r.w.Line(layout.Span{Text: l.url, Style: layout.Style{FontB: true}})
			r.w.SetAlign(escpos.AlignLeft)
		}
	}
	r.w.Apply(layout.Style{})
	return nil
}
