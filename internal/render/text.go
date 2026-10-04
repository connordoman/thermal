// Package render turns requests (plain text, Markdown, Unicode text, JSON
// block documents) into ESC/POS command streams for the printer.
package render

import (
	"strings"
	"unicode"

	"github.com/connordoman/escpos"
	"golang.org/x/text/unicode/norm"
)

// Clean normalises line endings, expands tabs and removes control
// characters, so text from a request can never smuggle printer commands
// (ESC, GS, ...) into a job. Only \n survives.
func Clean(s string) string {
	s = norm.NFC.String(strings.ToValidUTF8(s, "�"))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	col := 0
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
			col = 0
		case r == '\r':
			b.WriteRune('\n')
			col = 0
		case r == '\t':
			n := 4 - col%4
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// ASCII converts s to printable ASCII, transliterating where possible
// ("café — naïve" becomes "cafe - naive") and using '?' otherwise.
func ASCII(s string) string { return escpos.Transliterate(Clean(s)) }

// ForCodePage437 rewrites s so every rune is printable with CP437, the
// printer's default code page, so its printed width is known. Runes with no
// equivalent become '?'.
func ForCodePage437(s string) string {
	out, _ := escpos.CodePagePC437.Printable(s)
	return out
}

// NeedsUnicode reports whether s has characters the printer's fonts cannot
// show even approximately, such as emoji or CJK, and so must be drawn as an
// image to print faithfully.
func NeedsUnicode(s string) bool {
	_, complete := escpos.CodePagePC437.Printable(s)
	return !complete
}
