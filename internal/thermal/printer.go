package thermal

import (
	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/usb"
	"github.com/connordoman/thermal/internal/settings"
)

func GetPrinter() (printer *escpos.Printer, close func() error, err error) {

	conn, conErr := usb.Open(usb.Options{
		VendorID:  settings.Global.Printer.VendorId,
		ProductID: settings.Global.Printer.ProductId,
	})

	if conErr != nil {
		err = conErr
		return
	}

	printer = escpos.New(conn)
	close = conn.Close
	return
}
