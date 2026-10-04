package thermal

import (
	"github.com/connordoman/escpos"
)

func RenderUTF8(printer *escpos.Printer, text string) error {
	printer.Textln(text)
	return nil
}
