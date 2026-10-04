package render

import (
	"fmt"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// describe renders an ESC/POS stream as readable text for tests: printable
// text as is, commands as <NAME args>.
func describe(b []byte) string {
	var out strings.Builder
	arg := func(i, n int) []byte {
		if i+n > len(b) {
			return b[i:]
		}
		return b[i : i+n]
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x0A:
			out.WriteString("⏎\n")
			i++
		case c == 0x1B && i+1 < len(b):
			cmd := b[i+1]
			switch cmd {
			case '@':
				out.WriteString("<INIT>")
				i += 2
			case 'E', 'G', '-', 'M', 'a', '{', 'J', 'd', 't', 'R', '3', 'V':
				fmt.Fprintf(&out, "<ESC %c %d>", cmd, arg(i+2, 1)[0])
				i += 3
			case '2':
				out.WriteString("<ESC 2>")
				i += 2
			case 'B':
				fmt.Fprintf(&out, "<BEEP %v>", arg(i+2, 2))
				i += 4
			case 'p':
				fmt.Fprintf(&out, "<PULSE %v>", arg(i+2, 3))
				i += 5
			default:
				fmt.Fprintf(&out, "<ESC %q>", cmd)
				i += 2
			}
		case c == 0x1D && i+1 < len(b):
			cmd := b[i+1]
			switch cmd {
			case '!':
				fmt.Fprintf(&out, "<SIZE %02x>", arg(i+2, 1)[0])
				i += 3
			case 'B', 'H', 'f', 'h', 'w':
				fmt.Fprintf(&out, "<GS %c %d>", cmd, arg(i+2, 1)[0])
				i += 3
			case 'V':
				if arg(i+2, 1)[0] >= 65 {
					fmt.Fprintf(&out, "<CUT %d %d>", b[i+2], arg(i+3, 1)[0])
					i += 4
				} else {
					fmt.Fprintf(&out, "<CUT %d>", b[i+2])
					i += 3
				}
			case 'v':
				w := int(b[i+4]) | int(b[i+5])<<8
				h := int(b[i+6]) | int(b[i+7])<<8
				fmt.Fprintf(&out, "<RASTER %dx%d>", w*8, h)
				i += 8 + w*h
			case '(':
				n := int(b[i+3]) | int(b[i+4])<<8
				fmt.Fprintf(&out, "<GS ( %c %d bytes>", b[i+2], n)
				i += 5 + n
			case 'k':
				m, n := b[i+2], int(b[i+3])
				fmt.Fprintf(&out, "<BARCODE %d %q>", m, b[i+4:i+4+n])
				i += 4 + n
			default:
				fmt.Fprintf(&out, "<GS %q>", cmd)
				i += 2
			}
		case c < 0x20:
			fmt.Fprintf(&out, "<%02x>", c)
			i++
		default:
			r := charmap.CodePage437.DecodeByte(c)
			out.WriteRune(r)
			i++
		}
	}
	return out.String()
}
