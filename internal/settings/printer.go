package settings

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/connordoman/escpos"
)

const (
	// EnvEscposConnection selects how to reach the printer:
	//
	//	usb                        auto-detect (default)
	//	libusb                     auto-detect using libusb only
	//	file:/dev/usb/lp0          a device node or file
	//	tcp://192.168.1.50:9100    Ethernet
	//	serial:/dev/ttyUSB0?baud=9600
	//	discard                    accept jobs without printing (testing)
	EnvEscposConnection = "ESCPOS_CONNECTION"

	// EnvEscposVendorId and EnvEscposProductId narrow USB auto-detection to
	// one model. Both are optional.
	EnvEscposVendorId  = "ESCPOS_VENDOR_ID"
	EnvEscposProductId = "ESCPOS_PRODUCT_ID"

	// EnvEscposUSBSerial narrows USB auto-detection to one printer.
	EnvEscposUSBSerial = "ESCPOS_USB_SERIAL"

	// EnvEscposPaperWidth is the paper width: 80mm (or 80), 82mm, 60mm,
	// 58mm, or a printable width in dots such as 576.
	EnvEscposPaperWidth = "ESCPOS_PAPER_WIDTH"

	// EnvEscposTimeout bounds each status query.
	EnvEscposTimeout = "ESCPOS_TIMEOUT"
)

type PrinterConfig struct {
	Connection string
	VendorId   uint16
	ProductId  uint16
	USBSerial  string
	PaperWidth int
	Timeout    time.Duration
}

func NewPrinterConfig() *PrinterConfig {
	return &PrinterConfig{}
}

var paperWidths = map[string]int{
	"80mm": escpos.PaperWidth80mm,
	"82mm": escpos.PaperWidth82mm,
	"60mm": escpos.PaperWidth60mm,
	"58mm": escpos.PaperWidth58mm,
}

func (p *PrinterConfig) Load() error {
	var errs []error
	var err error
	p.Connection = envString(EnvEscposConnection, "usb")
	if p.VendorId, err = parseHexId(EnvEscposVendorId); err != nil {
		errs = append(errs, err)
	}
	if p.ProductId, err = parseHexId(EnvEscposProductId); err != nil {
		errs = append(errs, err)
	}
	p.USBSerial = envString(EnvEscposUSBSerial, "")

	w := strings.ToLower(envString(EnvEscposPaperWidth, "80mm"))
	if dots, ok := paperWidths[w]; ok {
		p.PaperWidth = dots
	} else if dots, ok := paperWidths[w+"mm"]; ok {
		// A bare 58/60/80/82 means millimetres; no printer is that few dots wide.
		p.PaperWidth = dots
	} else if n, err := strconv.Atoi(w); err == nil {
		p.PaperWidth = n
	} else {
		errs = append(errs, configError(EnvEscposPaperWidth, "want 80mm, 82mm, 60mm, 58mm or a width in dots"))
	}
	if p.Timeout, err = envDuration(EnvEscposTimeout, escpos.DefaultTimeout); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *PrinterConfig) Validate() error {
	if p.PaperWidth < 256 || p.PaperWidth > 1024 || p.PaperWidth%8 != 0 {
		return configError(EnvEscposPaperWidth, "width must be 58mm–82mm, or a multiple of 8 dots from 256 to 1024")
	}
	if p.Timeout <= 0 {
		return configError(EnvEscposTimeout, "must be positive")
	}
	return nil
}
