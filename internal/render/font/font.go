// Package font provides GNU Unifont, a bitmap font covering every assigned
// Unicode code point (emoji included) with 8×16 and 16×16 glyphs. Its blocky
// pixel lettering is a natural match for a thermal print head.
package font

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

//go:embed unifont_all-18.0.01.hex.gz
var hexGz []byte

//go:embed unifont-combining-18.0.01.txt
var combiningTxt string

// Height is the height of every glyph in pixels.
const Height = 16

// Font is a parsed Unifont.
type Font struct {
	// index maps a rune to its glyph's offset in data; the top bit marks a
	// 16-pixel-wide glyph.
	index     map[rune]uint32
	data      []byte
	combining map[rune]int
}

const wideBit = 1 << 31

// Load parses the embedded font. It takes a few hundred milliseconds the
// first time and is cached afterwards.
var Load = sync.OnceValues(load)

func load() (*Font, error) {
	zr, err := gzip.NewReader(bytes.NewReader(hexGz))
	if err != nil {
		return nil, err
	}
	f := &Font{
		index:     make(map[rune]uint32, 130000),
		data:      make([]byte, 0, 4<<20),
		combining: map[rune]int{},
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		cp, bits, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		r, err := strconv.ParseUint(cp, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("font: bad code point %q", cp)
		}
		off := uint32(len(f.data))
		switch len(bits) {
		case 32: // 8×16
		case 64: // 16×16
			off |= wideBit
		default:
			continue
		}
		b, err := hex.DecodeString(bits)
		if err != nil {
			return nil, fmt.Errorf("font: bad glyph for U+%04X", r)
		}
		f.data = append(f.data, b...)
		f.index[rune(r)] = off
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for line := range strings.Lines(combiningTxt) {
		cp, off, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		r, err1 := strconv.ParseUint(cp, 16, 32)
		o, err2 := strconv.Atoi(off)
		if err1 == nil && err2 == nil {
			f.combining[rune(r)] = o
		}
	}
	return f, nil
}

// Glyph is one character's bitmap.
type Glyph struct {
	Width int // 8 or 16
	bits  []byte
}

// Set reports whether pixel (x, y) is inked.
func (g Glyph) Set(x, y int) bool {
	if x < 0 || y < 0 || x >= g.Width || y >= Height {
		return false
	}
	bytesPerRow := g.Width / 8
	b := g.bits[y*bytesPerRow+x/8]
	return b&(0x80>>(x%8)) != 0
}

// Glyph returns the glyph for r.
func (f *Font) Glyph(r rune) (Glyph, bool) {
	off, ok := f.index[r]
	if !ok {
		return Glyph{}, false
	}
	w, n := 8, 16
	if off&wideBit != 0 {
		w, n = 16, 32
		off &^= wideBit
	}
	return Glyph{Width: w, bits: f.data[off : off+uint32(n)]}, true
}

// Combining returns the horizontal offset at which a combining mark is drawn
// relative to the pen position, and whether r is a combining mark. Combining
// marks do not advance the pen.
func (f *Font) Combining(r rune) (int, bool) {
	if o, ok := f.combining[r]; ok {
		return o, true
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return -8, true
	}
	return 0, false
}

// Invisible reports whether r should be skipped when drawing: joiners,
// variation selectors, emoji modifiers and other format characters that
// Unifont would otherwise show as hex boxes.
func Invisible(r rune) bool {
	switch {
	case r == 0x200B, r == 0x200C, r == 0x200D, r == 0x2060, r == 0xFEFF, r == 0x00AD:
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tone modifiers
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tags
		return true
	case r == 0x202A, r == 0x202B, r == 0x202C, r == 0x202D, r == 0x202E,
		r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F: // bidi controls
		return true
	}
	return unicode.IsControl(r)
}
