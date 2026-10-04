package render

import (
	"strings"

	"github.com/connordoman/escpos"
)

// Table is a grid of plain-text cells.
type Table struct {
	Headers []string       // optional
	Aligns  []escpos.Align // per column, default left
	Rows    [][]string
	Border  bool // separate columns with │ instead of a space
	// Weights, if set, divides the width among columns in proportion
	// instead of sizing columns to their content.
	Weights []int

	HeaderStyle Style
	Style       Style
}

func (t Table) columns() int {
	n := len(t.Headers)
	for _, r := range t.Rows {
		n = max(n, len(r))
	}
	return n
}

// layout arranges the table into lines of at most width cells, where
// measure gives the number of cells a string occupies.
func (t Table) layout(width int, measure func(string) int) [][]Span {
	n := t.columns()
	if n == 0 {
		return nil
	}
	gap := 1
	sep := " "
	if t.Border {
		gap, sep = 3, " │ "
	}
	avail := max(width-gap*(n-1), n)

	natural := make([]int, n)
	consider := func(row []string) {
		for i, c := range row {
			natural[i] = max(natural[i], measure(c))
		}
	}
	consider(t.Headers)
	for _, r := range t.Rows {
		consider(r)
	}
	widths := fitColumns(natural, avail)
	if len(t.Weights) == n {
		widths = weighColumns(t.Weights, avail)
	}

	var out [][]Span
	addRow := func(row []string, st Style) {
		wrapped := make([][]string, n)
		height := 1
		for i := range n {
			c := ""
			if i < len(row) {
				c = row[i]
			}
			wrapped[i] = wrapCells(c, widths[i], measure)
			height = max(height, len(wrapped[i]))
		}
		for l := range height {
			var line []Span
			for i := range n {
				if i > 0 {
					line = append(line, Span{sep, t.Style})
				}
				c := ""
				if l < len(wrapped[i]) {
					c = wrapped[i][l]
				}
				a := escpos.AlignLeft
				if i < len(t.Aligns) {
					a = t.Aligns[i]
				}
				line = append(line, Span{pad(c, widths[i], a, measure), st})
			}
			out = append(out, line)
		}
	}
	if len(t.Headers) > 0 {
		addRow(t.Headers, t.HeaderStyle)
		var rule strings.Builder
		for i, w := range widths {
			if i > 0 {
				if t.Border {
					rule.WriteString("─┼─")
				} else {
					rule.WriteString(" ")
				}
			}
			rule.WriteString(strings.Repeat("─", w))
		}
		out = append(out, []Span{{rule.String(), t.Style}})
	}
	for _, r := range t.Rows {
		addRow(r, t.Style)
	}
	return out
}

// fitColumns shrinks natural column widths to fit avail, giving narrow
// columns what they need and sharing the rest among wide ones.
func fitColumns(natural []int, avail int) []int {
	widths := make([]int, len(natural))
	total := 0
	for i, w := range natural {
		widths[i] = max(w, 1)
		total += widths[i]
	}
	if total <= avail {
		return widths
	}
	fixed := make([]bool, len(natural))
	remaining, open := avail, len(natural)
	for changed := true; changed && open > 0; {
		changed = false
		share := remaining / open
		for i := range widths {
			if !fixed[i] && widths[i] <= share {
				fixed[i] = true
				remaining -= widths[i]
				open--
				changed = true
			}
		}
	}
	if open > 0 {
		share, extra := remaining/open, remaining%open
		for i := range widths {
			if !fixed[i] {
				widths[i] = max(share, 1)
				if extra > 0 {
					widths[i]++
					extra--
				}
			}
		}
	}
	return widths
}

func weighColumns(weights []int, avail int) []int {
	total := 0
	for _, w := range weights {
		total += max(w, 1)
	}
	widths := make([]int, len(weights))
	used := 0
	for i, w := range weights {
		widths[i] = max(avail*max(w, 1)/total, 1)
		used += widths[i]
	}
	for i := 0; used < avail; i = (i + 1) % len(widths) {
		widths[i]++
		used++
	}
	return widths
}

// wrapCells word-wraps s into lines of at most width cells.
func wrapCells(s string, width int, measure func(string) int) []string {
	var lines []string
	for para := range strings.SplitSeq(s, "\n") {
		var cur string
		for _, word := range strings.Fields(para) {
			for measure(word) > width {
				// Split a word that cannot fit on any line.
				if cur != "" {
					lines = append(lines, cur)
					cur = ""
				}
				head, tail := splitCells(word, width, measure)
				lines = append(lines, head)
				word = tail
			}
			switch {
			case cur == "":
				cur = word
			case measure(cur)+1+measure(word) <= width:
				cur += " " + word
			default:
				lines = append(lines, cur)
				cur = word
			}
		}
		lines = append(lines, cur)
	}
	return lines
}

func splitCells(s string, width int, measure func(string) int) (string, string) {
	n := 0
	for i, r := range s {
		n += measure(string(r))
		if n > width {
			if i == 0 {
				// Always make progress, even if one character is too wide.
				_, size := firstRune(s)
				return s[:size], s[size:]
			}
			return s[:i], s[i:]
		}
	}
	return s, ""
}

func firstRune(s string) (rune, int) {
	for i, r := range s {
		if i > 0 {
			return r, i
		}
	}
	return 0, len(s)
}

func pad(s string, width int, a escpos.Align, measure func(string) int) string {
	gap := max(width-measure(s), 0)
	switch a {
	case escpos.AlignRight:
		return strings.Repeat(" ", gap) + s
	case escpos.AlignCenter:
		return strings.Repeat(" ", gap/2) + s + strings.Repeat(" ", gap-gap/2)
	}
	return s + strings.Repeat(" ", gap)
}

// Table prints t with printer fonts.
func (w *Writer) Table(t Table) {
	cells := w.Width / t.Style.CharWidth()
	measure := func(s string) int { return len([]rune(w.prepare(s))) }
	for _, line := range t.layout(cells, measure) {
		w.printAtoms(w.atoms(line))
	}
}
