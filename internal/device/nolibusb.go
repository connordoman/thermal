//go:build !darwin && !libusb

package device

import (
	"errors"
	"io"

	"github.com/connordoman/escpos"
)

const libusbAvailable = false

var errNoLibusb = errors.New("this build has no libusb support; rebuild with -tags libusb")

func openLibusb(func(escpos.USBPrinter) bool) (io.ReadWriteCloser, escpos.USBPrinter, error) {
	return nil, escpos.USBPrinter{}, errNoLibusb
}
