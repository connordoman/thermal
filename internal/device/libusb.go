//go:build darwin || libusb

package device

import (
	"errors"
	"io"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/usb"
)

// libusb needs cgo. It is built in on macOS, which has no other way to reach
// a USB printer, and elsewhere with -tags libusb.
const libusbAvailable = true

var errNoLibusb = errors.New("this build has no libusb support; rebuild with -tags libusb")

func openLibusb(match func(escpos.USBPrinter) bool) (io.ReadWriteCloser, escpos.USBPrinter, error) {
	conn, err := usb.Open(usb.Options{Match: match})
	if err != nil {
		return nil, escpos.USBPrinter{}, err
	}
	return conn, conn.Info(), nil
}
