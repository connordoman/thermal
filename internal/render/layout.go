package render

import (
	"strings"
	"unicode"

	"github.com/connordoman/escpos"
)

// Style is a combination of printer character attributes.
type Style struct {
	Bold         bool
	Underline    uint8 // 0, 1 or 2 dots
	DoubleStrike bool
	Invert       bool
	FontB        bool
	Width        uint8 // 1–8, 0 means 1
	Height       uint8 // 1–8, 0 means 1
	UpsideDown   bool
}

func (s Style) width() uint8  { return max(s.Width, 1) }
func (s Style) height() uint8 { return max(s.Height, 1) }

func (s Style) font() escpos.Font {
	if s.FontB {
		return escpos.FontB
	}
	return escpos.FontA
}

// CharWidth is the width of one character in dots.
func (s Style) CharWidth() int { return s.font().Width() * int(s.width()) }

// Span is text in one style.
type Span struct {
	Text  string
	Style Style
}

// Writer prints styled, word-wrapped text with an escpos.Builder, sending
// only the commands needed to change style.
type Writer struct {
	B     *escpos.Builder
	Width int // printable width in dots

	cur   Style
	align escpos.Align

	// Raw passes text to the printer without rewriting it for CP437. It is
	// set when a JSON job selects another code page.
	Raw bool
}

// NewWriter returns a writer for b. It assumes the printer has just been
// initialised (ESC @).
func NewWriter(b *escpos.Builder) *Writer {
	return &Writer{B: b, Width: b.PaperWidth()}
}

// Reset records that the printer was reinitialised.
func (w *Writer) Reset() {
	w.cur = Style{}
	w.align = escpos.AlignLeft
}

// Apply switches the printer to style s.
func (w *Writer) Apply(s Style) {
	c := w.cur
	b := w.B
	if s.FontB != c.FontB {
		b.SelectFont(s.font())
	}
	if s.Bold != c.Bold {
		b.SetEmphasis(s.Bold)
	}
	if s.Underline != c.Underline {
		b.SetUnderline(escpos.Underline(min(s.Underline, 2)))
	}
	if s.DoubleStrike != c.DoubleStrike {
		b.SetDoubleStrike(s.DoubleStrike)
	}
	if s.Invert != c.Invert {
		b.SetReverse(s.Invert)
	}
	if s.width() != c.width() || s.height() != c.height() {
		b.SetCharacterSize(min(s.width(), 8), min(s.height(), 8))
	}
	if s.UpsideDown != c.UpsideDown {
		b.SetUpsideDown(s.UpsideDown)
	}
	w.cur = s
}

// SetAlign sets the justification of following lines.
func (w *Writer) SetAlign(a escpos.Align) {
	if a != w.align {
		w.B.SetAlign(a)
		w.align = a
	}
}

// Align returns the current justification.
func (w *Writer) Align() escpos.Align { return w.align }

func (w *Writer) prepare(s string) string {
	if w.Raw {
		return s
	}
	return ForCodePage437(s)
}

// atom is one printed character.
type atom struct {
	r     rune
	style Style
}

func (w *Writer) atoms(spans []Span) []atom {
	var out []atom
	for _, sp := range spans {
		for _, r := range w.prepare(sp.Text) {
			out = append(out, atom{r, sp.Style})
		}
	}
	return out
}

func atomsWidth(a []atom) int {
	n := 0
	for _, x := range a {
		n += x.style.CharWidth()
	}
	return n
}

// Paragraph prints spans word-wrapped to the paper width. Explicit newlines
// in the text start new lines. first is printed before the first line and
// rest before each following line, for list bullets and quote bars.
func (w *Writer) Paragraph(spans []Span, first, rest []Span) {
	all := w.atoms(spans)
	pf, pr := w.atoms(first), w.atoms(rest)
	lineNo := 0
	start := 0
	for i := 0; i <= len(all); i++ {
		if i < len(all) && all[i].r != '\n' {
			continue
		}
		for _, line := range wrapAtoms(all[start:i], w.Width-atomsWidth(pf), w.Width-atomsWidth(pr)) {
			prefix := pr
			if lineNo == 0 {
				prefix = pf
			}
			w.printAtoms(append(append([]atom{}, prefix...), line...))
			lineNo++
			// After the first line, later segments use the rest prefix.
			pf = pr
		}
		start = i + 1
	}
}

// Line prints spans on one line without wrapping (the printer wraps text
// that is too long).
func (w *Writer) Line(spans ...Span) {
	w.printAtoms(w.atoms(spans))
}

// Text prints s in style st, word-wrapped.
func (w *Writer) Text(s string, st Style) {
	w.Paragraph([]Span{{s, st}}, nil, nil)
}

func (w *Writer) printAtoms(line []atom) {
	// Drop trailing spaces, which would show when underlined or inverted.
	for len(line) > 0 && line[len(line)-1].r == ' ' {
		line = line[:len(line)-1]
	}
	w.printAtomsKeepSpaces(line)
}

// wrapAtoms breaks a line of atoms into lines no wider than firstWidth for
// the first line and restWidth after that. It breaks at spaces where it can
// and inside words that are too long to fit on a line.
func wrapAtoms(line []atom, firstWidth, restWidth int) [][]atom {
	if len(line) == 0 {
		return [][]atom{nil}
	}
	var lines [][]atom
	limit := firstWidth
	var cur []atom
	curW := 0
	lastBreak := -1 // index in cur just after the last space
	for _, a := range line {
		aw := a.style.CharWidth()
		if curW+aw > limit && len(cur) > 0 {
			if a.r == ' ' {
				// Break here; the space is dropped.
				lines = append(lines, cur)
				cur, curW, lastBreak = nil, 0, -1
				limit = restWidth
				continue
			}
			if lastBreak > 0 {
				lines = append(lines, cur[:lastBreak])
				cur = append([]atom{}, cur[lastBreak:]...)
			} else {
				lines = append(lines, cur)
				cur = nil
			}
			curW = atomsWidth(cur)
			lastBreak = -1
			limit = restWidth
		}
		if a.r == ' ' && len(cur) == 0 && len(lines) > 0 {
			continue // no leading spaces on wrapped lines
		}
		cur = append(cur, a)
		curW += aw
		if unicode.IsSpace(a.r) || a.r == '-' || a.r == '/' {
			lastBreak = len(cur)
		}
	}
	return append(lines, cur)
}

// Rule prints a full-width horizontal line of r.
func (w *Writer) Rule(r rune, st Style) {
	n := w.Width / st.CharWidth()
	w.Line(Span{strings.Repeat(string(r), max(n, 1)), st})
}

// KeyValue prints key on the left and value on the right, joined by leader
// characters, wrapping the key if both do not fit on one line.
func (w *Writer) KeyValue(key, value string, leader rune, keyStyle, valueStyle Style) {
	k, v := w.atoms([]Span{{key, keyStyle}}), w.atoms([]Span{{value, valueStyle}})
	vw := atomsWidth(v)
	cw := keyStyle.CharWidth()
	if vw+cw > w.Width {
		w.Paragraph([]Span{{key, keyStyle}}, nil, nil)
		w.SetAlign(escpos.AlignRight)
		w.printAtoms(v)
		w.SetAlign(escpos.AlignLeft)
		return
	}
	lines := wrapAtoms(k, w.Width-vw-cw, w.Width-vw-cw)
	for i, l := range lines {
		if i < len(lines)-1 {
			w.printAtoms(l)
			continue
		}
		pad := (w.Width - atomsWidth(l) - vw) / cw
		fill := make([]atom, 0, pad)
		if pad > 0 {
			fill = append(fill, atom{' ', keyStyle})
			for range pad - 2 {
				fill = append(fill, atom{leader, keyStyle})
			}
			if pad > 1 {
				fill = append(fill, atom{' ', keyStyle})
			}
		}
		out := append(append(append([]atom{}, l...), fill...), v...)
		w.printAtomsKeepSpaces(out)
	}
}

func (w *Writer) printAtomsKeepSpaces(line []atom) {
	var run strings.Builder
	var st Style
	flush := func() {
		if run.Len() > 0 {
			w.Apply(st)
			w.B.Text(run.String())
			run.Reset()
		}
	}
	for i, a := range line {
		if i == 0 || a.style != st {
			flush()
			st = a.style
		}
		run.WriteRune(a.r)
	}
	flush()
	w.B.LineFeed()
}

// barLineSpacing is the line spacing, in dots, at which box-drawing
// characters such as │ in style st touch the lines above and below. The
// default spacing (ESC 2) leaves a gap, so vertical bars print as dashes.
func barLineSpacing(st Style) uint8 {
	h := 24
	if st.FontB {
		h = 17
	}
	return uint8(min(h*int(st.height()), 255))
}
