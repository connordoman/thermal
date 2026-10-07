package render

import (
	"errors"
	"fmt"

	"github.com/connordoman/escpos"
)

// DefaultPageHeight is the tallest page, in dots, that upside-down jobs
// print at once unless the server sets its own.
const DefaultPageHeight = 1024

// ErrUpsideDown is wrapped by errors for jobs that cannot be printed upside
// down.
var ErrUpsideDown = errors.New("cannot print upside down")

// Heights the printer gives text and line feeds, in dots.
const (
	defaultLineSpacing = 30
	fontAHeight        = 24
	fontBHeight        = 17
)

// UpsideDown rewrites a job's content (everything before its finish) so the
// receipt reads correctly when turned 180°. The content is split into
// chunks no taller than pageHeight dots, at line, image and bar code
// boundaries, and the chunks are printed last first, each in page mode with
// ESC T 2 (right to left, from the lower right corner). Cuts stay in place:
// each part between cuts is turned on its own.
//
// Page mode ignores justification (ESC a), margins (GS L, GS W), and the
// rotation modes, so text prints left-aligned at full width. Images are
// padded to the paper width when aligned, so they keep their place. Chunk
// heights are estimates; a short estimate clips a chunk and a long one
// leaves a gap.
func UpsideDown(body []byte, paperWidth, pageHeight int) ([]byte, error) {
	if pageHeight <= 0 {
		pageHeight = DefaultPageHeight
	}
	s := &flipper{body: body, width: paperWidth, pageHeight: pageHeight, state: map[string][]byte{}}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.endSection(nil)
	return s.out, nil
}

// unit is a run of content that prints as one piece: a line of text, a
// feed, an image or a symbol.
type unit struct {
	data    []byte
	height  int
	prelude []byte // the printer state the unit starts with
}

type flipper struct {
	body       []byte
	width      int
	pageHeight int
	out        []byte

	units []unit
	cur   []byte // the unit being collected
	start []byte // prelude for cur

	// Printer state, as the last command that set each setting.
	state map[string][]byte

	// For estimating heights.
	text       bool // cur holds text
	textHeight int  // tallest text in cur
}

// stateOrder lists the settings carried into each chunk, in the order they
// are restored.
var stateOrder = []string{
	"ESC t", "ESC R", "ESC !", "ESC M", "GS !", "ESC E", "ESC G", "ESC -", "GS B", "ESC SP", "spacing",
	"GS h", "GS w", "GS H", "GS f", "GS x", "GS Z", "QR 65", "QR 67", "QR 69",
}

func (s *flipper) prelude() []byte {
	var p []byte
	for _, k := range stateOrder {
		p = append(p, s.state[k]...)
	}
	return p
}

func (s *flipper) arg(key string, def int) int {
	if c := s.state[key]; len(c) > 0 {
		return int(c[len(c)-1])
	}
	return def
}

func (s *flipper) lineSpacing() int {
	c := s.state["spacing"]
	if len(c) == 3 { // ESC 3 n
		return int(c[2])
	}
	return defaultLineSpacing
}

func (s *flipper) charHeight() int {
	h := fontAHeight
	mode := s.arg("ESC !", 0)
	if f := s.arg("ESC M", 0); f == 1 || f == 49 || (s.state["ESC M"] == nil && mode&1 != 0) {
		h = fontBHeight
	}
	mul := s.arg("GS !", 0)&0x0F + 1
	if mode&0x10 != 0 {
		mul = max(mul, 2)
	}
	return h * mul
}

// add appends b to the current unit.
func (s *flipper) add(b []byte) {
	if s.cur == nil {
		s.start = s.prelude()
	}
	s.cur = append(s.cur, b...)
}

// endUnit closes the current unit, which takes height dots.
func (s *flipper) endUnit(height int) {
	if s.cur != nil {
		s.units = append(s.units, unit{data: s.cur, height: height, prelude: s.start})
	}
	s.cur, s.start, s.text, s.textHeight = nil, nil, false, 0
}

// lineHeight is the height of the current line when it is printed and fed
// by feed dots.
func (s *flipper) lineHeight(feed int) int {
	if s.text {
		return max(feed, s.textHeight)
	}
	return feed
}

// endSection prints the units collected since the last cut, then sep (the
// cut) as is.
func (s *flipper) endSection(sep []byte) {
	if s.cur != nil {
		s.endUnit(s.lineHeight(0))
	}
	// Group units into chunks that fit a page.
	type chunk struct {
		data   []byte
		height int
	}
	var chunks []chunk
	for _, u := range s.units {
		if n := len(chunks); n > 0 && (chunks[n-1].height == 0 || chunks[n-1].height+u.height <= s.pageHeight) {
			chunks[n-1].data = append(chunks[n-1].data, u.data...)
			chunks[n-1].height += u.height
			continue
		}
		chunks = append(chunks, chunk{data: append(u.prelude[:len(u.prelude):len(u.prelude)], u.data...), height: u.height})
	}
	total := 0
	for _, c := range chunks {
		total += c.height
	}
	if total == 0 {
		// Nothing to see (state changes, a drawer kick): send it as is.
		for _, c := range chunks {
			s.out = append(s.out, c.data...)
		}
	} else {
		b := escpos.NewBuilder(s.width)
		for i := len(chunks) - 1; i >= 0; i-- {
			c := chunks[i]
			if c.height == 0 {
				b.Raw(c.data...) // trailing settings or a drawer kick
				continue
			}
			b.Initialize()
			b.EnterPageMode()
			_ = b.SetPageDirection(escpos.PageRightToLeft)
			_ = b.SetPageArea(0, 0, uint16(s.width), uint16(min(c.height, 0xFFFF)))
			b.Raw(c.data...)
			b.PrintPageAndExit()
		}
		s.out = append(s.out, b.Bytes()...)
	}
	s.out = append(s.out, sep...)
	s.units = nil
}

func (s *flipper) scan() error {
	b := s.body
	for i := 0; i < len(b); {
		c := b[i]
		if c >= 0x20 && c != 0x7F {
			j := i
			for j < len(b) && b[j] >= 0x20 && b[j] != 0x7F {
				j++
			}
			s.add(b[i:j])
			s.text = true
			s.textHeight = max(s.textHeight, s.charHeight())
			i = j
			continue
		}
		n := cmdLen(b[i:])
		if n == 0 {
			return fmt.Errorf("%w: unsupported command %s at byte %d", ErrUpsideDown, cmdName(b[i:]), i)
		}
		cmd := b[i : i+n]
		i += n
		if err := s.command(cmd); err != nil {
			return err
		}
	}
	return nil
}

func (s *flipper) command(cmd []byte) error {
	// Settings are recorded after the command joins a unit, so a unit's
	// prelude does not repeat its own first command.
	key := ""
	set := func(k string) { key = k }
	defer func() {
		if key != "" {
			s.state[key] = cmd
		}
	}()
	switch cmd[0] {
	case escpos.LF:
		s.add(cmd)
		s.endUnit(s.lineHeight(s.lineSpacing()))
		return nil
	case escpos.FF:
		return fmt.Errorf("%w: the job uses page mode", ErrUpsideDown)
	case escpos.ESC:
		switch cmd[1] {
		case '@':
			clear(s.state)
			return nil // each chunk initialises the printer itself
		case 'L':
			return fmt.Errorf("%w: documents with page blocks cannot be printed upside down", ErrUpsideDown)
		case 'i', 'm':
			s.endSection(cmd)
			return nil
		case 'J':
			s.add(cmd)
			s.endUnit(s.lineHeight(int(cmd[2])))
			return nil
		case 'd':
			s.add(cmd)
			if n := int(cmd[2]); n > 0 {
				s.endUnit(s.lineHeight(s.lineSpacing()) + (n-1)*s.lineSpacing())
			} else {
				s.endUnit(s.lineHeight(0))
			}
			return nil
		case 't', 'R', '!', 'M', 'E', 'G', '-':
			set("ESC " + string(cmd[1]))
		case ' ':
			set("ESC SP")
		case '2', '3':
			set("spacing")
		case 'Z': // PDF417, after GS Z 0
			s.endPending()
			s.add(cmd)
			s.endUnit(pdf417Height(cmd))
			return nil
		}
	case escpos.GS:
		switch cmd[1] {
		case 'V':
			s.endSection(cmd)
			return nil
		case '!', 'B', 'h', 'w', 'H', 'f', 'x', 'Z':
			set("GS " + string(cmd[1]))
		case 'v': // raster image
			s.endPending()
			s.image(cmd)
			return nil
		case 'k': // bar code
			s.endPending()
			s.add(cmd)
			h := s.arg("GS h", 162)
			hri := fontAHeight
			if s.arg("GS f", 0)&1 != 0 {
				hri = fontBHeight
			}
			switch s.arg("GS H", 0) & 3 {
			case 1, 2:
				h += hri
			case 3:
				h += 2 * hri
			}
			s.endUnit(h)
			return nil
		case '(':
			if cmd[2] == 'k' && len(cmd) >= 7 && cmd[5] == 49 { // QR code
				switch fn := cmd[6]; fn {
				case 65, 67, 69:
					set(fmt.Sprintf("QR %d", fn))
				case 80:
					// Keep the data with the print command that follows.
					s.endPending()
					s.state["QR data"] = cmd
				case 81:
					s.endPending()
					s.add(cmd)
					s.endUnit(s.qrHeight())
					return nil
				}
			}
		}
	}
	s.add(cmd)
	return nil
}

// endPending ends a line of text that a symbol or image would otherwise
// share. Those print on their own line in standard mode.
func (s *flipper) endPending() {
	if s.text {
		s.endUnit(s.lineHeight(0))
	}
}

// image adds a raster image (GS v 0), split into bands that fit a page.
func (s *flipper) image(cmd []byte) {
	m := cmd[3]
	wb := int(cmd[4]) | int(cmd[5])<<8
	rows := int(cmd[6]) | int(cmd[7])<<8
	data := cmd[8:]
	scale := 1
	if m == 2 || m == 3 || m == 50 || m == 51 {
		scale = 2
	}
	band := max(s.pageHeight/scale, 1)
	for r := 0; r < rows; r += band {
		n := min(band, rows-r)
		head := []byte{escpos.GS, 'v', '0', m, byte(wb), byte(wb >> 8), byte(n), byte(n >> 8)}
		s.add(append(head, data[r*wb:(r+n)*wb]...))
		s.endUnit(n * scale)
	}
}

// qrCapacity is the byte-mode capacity of QR versions 1–40 at error
// correction levels L, M, Q and H.
var qrCapacity = [4][40]int{
	{17, 32, 53, 78, 106, 134, 154, 192, 230, 271, 321, 367, 425, 458, 520, 586, 644, 718, 792, 858, 929, 1003, 1091, 1171, 1273, 1367, 1465, 1528, 1628, 1732, 1840, 1952, 2068, 2188, 2303, 2431, 2563, 2699, 2809, 2953},
	{14, 26, 42, 62, 84, 106, 122, 152, 180, 213, 251, 287, 331, 362, 412, 450, 504, 560, 624, 666, 711, 779, 857, 911, 997, 1059, 1125, 1190, 1264, 1370, 1452, 1538, 1628, 1722, 1809, 1911, 1989, 2099, 2213, 2331},
	{11, 20, 32, 46, 60, 74, 86, 108, 130, 151, 177, 203, 241, 258, 292, 322, 364, 394, 442, 482, 509, 565, 611, 661, 715, 751, 805, 868, 908, 982, 1030, 1112, 1168, 1228, 1283, 1351, 1423, 1499, 1579, 1663},
	{7, 14, 24, 34, 44, 58, 64, 84, 98, 119, 137, 155, 177, 194, 220, 250, 280, 310, 338, 382, 403, 439, 461, 511, 535, 593, 625, 658, 698, 742, 790, 842, 898, 958, 983, 1051, 1093, 1139, 1219, 1273},
}

// qrHeight estimates the height of the stored QR code, assuming the printer
// encodes it in byte mode.
func (s *flipper) qrHeight() int {
	size := s.arg("QR 67", 3)
	ec := min(max(s.arg("QR 69", 48)-48, 0), 3)
	n := 0
	if d := s.state["QR data"]; len(d) >= 5 {
		n = (int(d[3]) | int(d[4])<<8) - 3
	}
	version := 40
	for v, c := range qrCapacity[ec] {
		if n <= c {
			version = v + 1
			break
		}
	}
	return (17 + 4*version) * size
}

// pdf417Height roughly estimates the height of a PDF417 symbol from ESC Z
// columns security ratio dL dH data.
func pdf417Height(cmd []byte) int {
	cols := max(int(cmd[2]), 1)
	security := int(cmd[3])
	ratio := max(int(cmd[4]), 2)
	n := int(cmd[5]) | int(cmd[6])<<8
	codewords := (n*5+5)/6 + 2 + 1<<(security+1)
	rows := min(max((codewords+cols-1)/cols, 3), 90)
	return rows * ratio * 2
}

// cmdLen returns the length of the command at the start of b, or 0 if it
// is truncated or not one that rendered jobs contain.
func cmdLen(b []byte) int {
	need := func(n int) int {
		if n > len(b) {
			return 0
		}
		return n
	}
	le := func(i int) int {
		if i+1 >= len(b) {
			return -1 << 20
		}
		return int(b[i]) | int(b[i+1])<<8
	}
	switch b[0] {
	case escpos.LF, escpos.CR, escpos.HT, escpos.FF, escpos.NUL:
		return 1
	case escpos.DC2:
		return need(2)
	case escpos.DLE:
		if len(b) < 2 {
			return 0
		}
		switch b[1] {
		case 4, 5: // EOT, ENQ
			return need(3)
		case 20: // DC4
			return need(5)
		}
	case escpos.FS:
		if len(b) < 2 {
			return 0
		}
		switch b[1] {
		case '&', '.':
			return 2
		case '!', '-', 'W':
			return need(3)
		case 'S', 'p':
			return need(4)
		}
	case escpos.ESC:
		if len(b) < 2 {
			return 0
		}
		switch b[1] {
		case '@', '2', 'L', 'S', 'i', 'm', escpos.FF:
			return 2
		case 'a', 'E', 'G', '-', '!', 'M', '{', 'V', ' ', '3', 'J', 'd', 't', 'R', 'T', '%', '?', '=', '9':
			return need(3)
		case '$', '\\', 'B':
			return need(4)
		case 'c':
			return need(4)
		case 'p':
			return need(5)
		case 'W':
			return need(10)
		case '*':
			if len(b) < 5 {
				return 0
			}
			k := le(3)
			if b[2] == 32 || b[2] == 33 {
				k *= 3
			}
			return need(5 + k)
		case 'Z':
			if len(b) < 7 {
				return 0
			}
			return need(7 + le(5))
		case 'D':
			for i := 2; i < len(b); i++ {
				if b[i] == escpos.NUL {
					return i + 1
				}
			}
		}
	case escpos.GS:
		if len(b) < 2 {
			return 0
		}
		switch b[1] {
		case escpos.FF, ':', 'c':
			return 2
		case '!', 'B', 'h', 'w', 'H', 'f', 'x', 'r', 'a', 'I', '/', 'Z':
			return need(3)
		case 'L', 'W', '$', '\\', 'P':
			return need(4)
		case '^':
			return need(5)
		case 'V':
			if len(b) < 3 {
				return 0
			}
			if b[2] == 65 || b[2] == 66 {
				return need(4)
			}
			return 3
		case 'v':
			if len(b) < 8 || b[2] != '0' {
				return 0
			}
			return need(8 + le(4)*le(6))
		case '(':
			if len(b) < 5 {
				return 0
			}
			return need(5 + le(3))
		case 'k':
			if len(b) < 3 {
				return 0
			}
			if b[2] <= 6 {
				for i := 3; i < len(b); i++ {
					if b[i] == escpos.NUL {
						return i + 1
					}
				}
				return 0
			}
			if len(b) < 4 {
				return 0
			}
			return need(4 + int(b[3]))
		case '*':
			if len(b) < 4 {
				return 0
			}
			return need(4 + int(b[2])*int(b[3])*8)
		case 'C':
			if len(b) < 3 {
				return 0
			}
			switch b[2] {
			case '0', '2':
				return need(5)
			case '1':
				return need(9)
			}
		}
	}
	return 0
}

// cmdName names the command at the start of b for error messages.
func cmdName(b []byte) string {
	names := map[byte]string{escpos.ESC: "ESC", escpos.GS: "GS", escpos.FS: "FS", escpos.DLE: "DLE", escpos.DC2: "DC2"}
	if n, ok := names[b[0]]; ok && len(b) > 1 {
		if b[1] > 0x20 && b[1] < 0x7F {
			return fmt.Sprintf("%s %c", n, b[1])
		}
		return fmt.Sprintf("%s 0x%02X", n, b[1])
	}
	return fmt.Sprintf("0x%02X", b[0])
}
