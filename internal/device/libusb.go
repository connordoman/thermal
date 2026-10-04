//go:build darwin || libusb

package device

// libusb needs cgo. It is built in on macOS, which has no other way to reach
// a USB printer, and elsewhere with -tags libusb. Importing the package
// registers the libusb connection scheme and the "usb" fallback to it.
import _ "github.com/connordoman/escpos/usb"
