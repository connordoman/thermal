package render

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/connordoman/escpos"
)

// ErrUpsideDown is wrapped by errors for jobs that cannot be printed upside
// down.
var ErrUpsideDown = errors.New("cannot print upside down")

// UpsideDown rewrites a job's content (everything before its finish) so the
// receipt reads correctly when turned 180°, using the printer's own
// upside-down mode (ESC {) in standard mode.
//
// The printer turns each line it prints, but not the order of the lines, so
// the content is cut into units (a line, a feed, an image, a symbol) that
// are sent last first, each starting with ESC { 1. Text, bar codes, QR codes
// and ESC * images are turned by the printer, so alignment, spacing, size and
// emphasis all work as usual. Raster images (GS v 0) are not, so they are
// turned here, and their alignment mirrored. Cuts stay in place: each part
// between cuts is turned on its own.
//
// Left margins and print area widths (GS L, GS W) and 90° rotated characters
// (ESC V) are dropped, and page mode (ESC L) is refused, along with commands
// this package does not know.
func UpsideDown(body []byte) ([]byte, error) {
	s := &flipper{body: body, state: map[string][]byte{}}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.endSection(nil)
	return s.out, nil
}

// unit is a run of content that prints as one piece.
type unit struct {
	data    []byte
	prelude []byte // the printer state the unit starts with
	// upsideDown is whether the job itself had upside-down printing on.
	upsideDown bool
	// align is the justification the unit starts with.
	align  []byte
	raster bool
}

type flipper struct {
	body []byte
	out  []byte

	units []unit
	cur   []byte // the unit being collected
	start unit   // how cur starts
	text  bool   // cur has text on a line that is not ended yet

	// Printer state, as the last command that set each setting.
	state map[string][]byte
}

// stateOrder lists the settings carried into each unit, in the order they
// are restored. ESC a and ESC { are handled apart.
var stateOrder = []string{
	"ESC t", "ESC R", "ESC !", "ESC M", "GS !", "ESC E", "ESC G", "ESC -", "GS B", "ESC SP", "spacing", "ESC D",
	"GS h", "GS w", "GS H", "GS f", "GS x", "GS Z", "QR 65", "QR 67", "QR 69",
}

// mirror swaps left and right justification.
func mirror(align []byte) []byte {
	if len(align) != 3 {
		return []byte{escpos.ESC, 'a', 2} // the default, left
	}
	m := []byte{align[0], align[1], align[2]}
	switch align[2] {
	case 0, '0':
		m[2] = align[2] + 2
	case 2, '2':
		m[2] = align[2] - 2
	}
	return m
}

// beginUnit records the state a unit starts with, unless one has started.
func (s *flipper) beginUnit() {
	if s.cur != nil {
		return
	}
	s.cur = []byte{}
	var p []byte
	for _, k := range stateOrder {
		p = append(p, s.state[k]...)
	}
	s.start = unit{prelude: p, align: s.state["ESC a"], upsideDown: len(s.state["ESC {"]) == 3 && s.state["ESC {"][2]&1 == 1}
}

// add appends b to the current unit.
func (s *flipper) add(b []byte) {
	s.beginUnit()
	s.cur = append(s.cur, b...)
}

// addText appends text, or a command that prints within a line.
func (s *flipper) addText(b []byte) {
	s.add(b)
	s.text = true
}

// endUnit closes the current unit.
func (s *flipper) endUnit() {
	if s.cur != nil {
		u := s.start
		u.data = s.cur
		s.units = append(s.units, u)
	}
	s.cur, s.start, s.text = nil, unit{}, false
}

// endLine ends a line of text that nothing ended, without feeding more than
// the line: a symbol or image would otherwise share it, and the lines after
// it would be printed after it.
func (s *flipper) endLine() {
	if s.text {
		s.add([]byte{escpos.ESC, 'J', 0})
	}
	s.endUnit()
}

// setting records a command that sets key. Within a unit it also stays in
// the unit, since it may change what follows on the same line.
func (s *flipper) setting(key string, cmd []byte) {
	if s.cur != nil {
		s.cur = append(s.cur, cmd...)
	}
	s.state[key] = cmd
}

// endSection prints the units collected since the last cut, last first, then
// sep (the cut) as is.
func (s *flipper) endSection(sep []byte) {
	s.endLine()
	var last []byte
	started, on := false, false // whether a unit was sent, and ESC { is on
	for i := len(s.units) - 1; i >= 0; i-- {
		u := s.units[i]
		align := u.align
		if u.raster {
			align = mirror(align)
		}
		prelude := append(u.prelude[:len(u.prelude):len(u.prelude)], align...)
		if !started || string(prelude) != string(last) {
			s.out = append(s.out, escpos.ESC, '@')
			s.out = append(s.out, prelude...)
			last, started, on = prelude, true, false
		}
		// The printer turns what it prints itself; text the job already
		// turned is turned back.
		if want := !u.upsideDown; !u.raster && want != on {
			s.out = append(s.out, escpos.ESC, '{', b2i(want))
			on = want
		}
		s.out = append(s.out, u.data...)
	}
	if on {
		s.out = append(s.out, escpos.ESC, '{', 0)
	}
	s.out = append(s.out, sep...)
	s.units = nil
}

func b2i(b bool) byte {
	if b {
		return 1
	}
	return 0
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
			s.addText(b[i:j])
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
	switch cmd[0] {
	case escpos.LF:
		s.add(cmd)
		s.endUnit()
		return nil
	case escpos.FF:
		return fmt.Errorf("%w: the job uses page mode", ErrUpsideDown)
	case escpos.ESC:
		switch cmd[1] {
		case '@':
			clear(s.state)
			return nil // each unit initialises the printer itself
		case 'L':
			return fmt.Errorf("%w: page mode is not available", ErrUpsideDown)
		case 'i', 'm':
			s.endSection(cmd)
			return nil
		case 'V':
			return nil // not combined with upside-down printing
		case 'J', 'd':
			s.endLine()
			s.add(cmd)
			s.endUnit()
			return nil
		case 'a':
			s.state["ESC a"] = cmd
			return nil
		case '{':
			s.state["ESC {"] = cmd
			return nil
		case 't', 'R', '!', 'M', 'E', 'G', '-', 'D':
			s.setting("ESC "+string(cmd[1]), cmd)
			return nil
		case ' ':
			s.setting("ESC SP", cmd)
			return nil
		case '2', '3':
			s.setting("spacing", cmd)
			return nil
		case '*':
			s.addText(cmd)
			return nil
		case 'Z': // PDF417, after GS Z 0
			s.endLine()
			s.add(cmd)
			s.endUnit()
			return nil
		}
	case escpos.GS:
		switch cmd[1] {
		case 'V':
			s.endSection(cmd)
			return nil
		case 'L', 'W':
			return nil // margins and print area widths are dropped
		case '!', 'B', 'h', 'w', 'H', 'f', 'x', 'Z':
			s.setting("GS "+string(cmd[1]), cmd)
			return nil
		case 'v': // raster image
			s.endLine()
			s.image(cmd)
			return nil
		case 'k': // bar code
			s.endLine()
			s.add(cmd)
			s.endUnit()
			return nil
		case '(':
			if cmd[2] == 'k' && len(cmd) >= 7 && cmd[5] == 49 { // QR code
				switch fn := cmd[6]; fn {
				case 65, 67, 69:
					s.setting(fmt.Sprintf("QR %d", fn), cmd)
					return nil
				case 80:
					// Keeps the data with the print command that follows.
					s.endLine()
				case 81:
					s.add(cmd)
					s.endUnit()
					return nil
				}
			}
		}
	}
	s.add(cmd)
	return nil
}

// image adds a raster image (GS v 0), turned 180°: the printer does not
// turn those itself.
func (s *flipper) image(cmd []byte) {
	wb := int(cmd[4]) | int(cmd[5])<<8
	rows := int(cmd[6]) | int(cmd[7])<<8
	data := cmd[8:]
	out := append(make([]byte, 0, len(cmd)), cmd[:8]...)
	for r := rows - 1; r >= 0; r-- {
		row := data[r*wb : (r+1)*wb]
		for x := wb - 1; x >= 0; x-- {
			out = append(out, bits.Reverse8(row[x]))
		}
	}
	s.add(out)
	s.start.raster = true
	s.endUnit()
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
