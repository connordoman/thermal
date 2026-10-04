// Package render turns requests (plain text, Markdown, Unicode text, JSON
// block documents) into ESC/POS command streams for the printer.
package render

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// Clean normalises line endings, expands tabs and removes control
// characters, so text from a request can never smuggle printer commands
// (ESC, GS, ...) into a job. Only \n survives.
func Clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
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

// transliterations covers characters that Unicode decomposition does not
// reduce to ASCII.
var transliterations = map[rune]string{
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-", '−': "-",
	'‘': "'", '’': "'", '‚': ",", '‛': "'", '′': "'", '‹': "<", '›': ">",
	'“': "\"", '”': "\"", '„': "\"", '‟': "\"", '″': "\"", '«': "<<", '»': ">>",
	'•': "*", '·': ".", '‣': ">", '◦': "o", '…': "...", '⁄': "/",
	'×': "x", '÷': "/", '±': "+/-", '°': "deg", '©': "(c)", '®': "(R)", '™': "TM",
	'€': "EUR", '£': "GBP", '¥': "JPY", '¢': "c", '¡': "!", '¿': "?", '§': "S",
	'¶': "P", '†': "+", '‡': "++", '→': "->", '←': "<-", '↑': "^", '↓': "v",
	'⇒': "=>", '⇐': "<=", '≤': "<=", '≥': ">=", '≠': "!=", '≈': "~", '∞': "inf",
	'ß': "ss", 'æ': "ae", 'Æ': "AE", 'œ': "oe", 'Œ': "OE", 'ø': "o", 'Ø': "O",
	'ł': "l", 'Ł': "L", 'đ': "d", 'Đ': "D", 'ð': "d", 'Ð': "D", 'þ': "th", 'Þ': "Th",
	'ı': "i", 'ħ': "h", 'Ħ': "H", 'ŧ': "t", 'Ŧ': "T", 'ŋ': "ng", 'Ŋ': "NG",
	'✓': "v", '✔': "v", '✗': "x", '✘': "x", '☐': "[ ]", '☑': "[x]", '☒': "[x]",
	'─': "-", '━': "-", '│': "|", '┃': "|", '═': "=", '║': "|",
}

// transliterate returns an ASCII rendering of r, or "" if there is none.
func transliterate(r rune) string {
	if r < utf8.RuneSelf {
		return string(r)
	}
	if t, ok := transliterations[r]; ok {
		return t
	}
	if unicode.IsSpace(r) {
		return " "
	}
	var out strings.Builder
	for _, d := range norm.NFKD.String(string(r)) {
		switch {
		case d < utf8.RuneSelf:
			out.WriteRune(d)
		case unicode.Is(unicode.Mn, d):
		default:
			if t, ok := transliterations[d]; ok {
				out.WriteString(t)
			} else {
				return ""
			}
		}
	}
	return out.String()
}

// ASCII converts s to printable ASCII, transliterating where possible
// ("café — naïve" becomes "cafe - naive") and using '?' otherwise.
func ASCII(s string) string {
	var b strings.Builder
	for _, r := range Clean(s) {
		if zeroWidth(r) {
			continue
		}
		if t := transliterate(r); t != "" {
			b.WriteString(t)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// cp437 is the printer's default code page.
var cp437 = charmap.CodePage437

// printable returns how r prints with the default code page: r itself if
// CP437 has it, an ASCII transliteration, or "" if neither works.
func printable(r rune) string {
	if r >= 0x20 && r < 0x7F {
		return string(r)
	}
	if r >= 0xA0 {
		if _, ok := cp437.EncodeRune(r); ok {
			return string(r)
		}
	}
	return transliterate(r)
}

// zeroWidth reports whether r prints nothing on its own: combining marks
// left over after NFC normalisation and format characters such as joiners.
func zeroWidth(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf)
}

// ForCodePage437 rewrites s so every rune is printable with CP437, so its
// printed width is known. Runes with no equivalent become '?'.
func ForCodePage437(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' {
			b.WriteRune(r)
		} else if zeroWidth(r) {
			continue
		} else if p := printable(r); p != "" {
			b.WriteString(p)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// NeedsUnicode reports whether s has characters the printer's fonts cannot
// show even approximately, such as emoji or CJK, and so must be drawn as an
// image to print faithfully.
func NeedsUnicode(s string) bool {
	for _, r := range s {
		if r != '\n' && !unicode.IsControl(r) && !zeroWidth(r) && printable(r) == "" {
			return true
		}
	}
	return false
}
