// Package device owns the connection to the printer. It connects lazily,
// reconnects after failures and serialises access, so the job worker and
// status requests can share one printer.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/serial"
)

// Config configures a Device.
type Config struct {
	Target     Target
	VendorID   uint16 // optional USB filter
	ProductID  uint16 // optional USB filter
	USBSerial  string // optional USB filter
	PaperWidth int
	Timeout    time.Duration
}

// Reported is what the printer says about itself (GS I).
type Reported struct {
	Firmware     string `json:"firmware,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Name         string `json:"name,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Fonts        string `json:"fonts,omitempty"`
	ModelID      *byte  `json:"model_id,omitempty"`
	TypeID       *byte  `json:"type_id,omitempty"`
	MultiByte    *bool  `json:"multi_byte,omitempty"`
	AutoCutter   *bool  `json:"auto_cutter,omitempty"`
}

// USBInfo describes the USB device, when connected over USB.
type USBInfo struct {
	Path         string `json:"path"`
	VendorID     string `json:"vendor_id"`
	ProductID    string `json:"product_id"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Serial       string `json:"serial,omitempty"`
	DeviceID     string `json:"ieee1284_id,omitempty"`
	Driver       string `json:"driver"` // "os" or "libusb"
}

// Info is a snapshot of the device state.
type Info struct {
	Connection      string     `json:"connection"`
	Kind            Kind       `json:"kind"`
	Connected       bool       `json:"connected"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	Readable        bool       `json:"readable"`
	PaperWidth      int        `json:"paper_width_dots"`
	LibUSBAvailable bool       `json:"libusb_available"`
	USB             *USBInfo   `json:"usb,omitempty"`
	Reported        *Reported  `json:"reported,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
}

// Device is a lazily connected printer.
type Device struct {
	cfg Config

	mu      sync.Mutex // serialises use of the printer
	printer *escpos.Printer
	closer  io.Closer

	infoMu sync.RWMutex
	info   Info
}

// New returns a device that connects on first use.
func New(cfg Config) *Device {
	if cfg.PaperWidth == 0 {
		cfg.PaperWidth = escpos.PaperWidth80mm
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = escpos.DefaultTimeout
	}
	return &Device{cfg: cfg, info: Info{
		Connection:      cfg.Target.String(),
		Kind:            cfg.Target.Kind,
		PaperWidth:      cfg.PaperWidth,
		LibUSBAvailable: libusbAvailable,
	}}
}

// PaperWidth returns the configured printable width in dots.
func (d *Device) PaperWidth() int { return d.cfg.PaperWidth }

// Info returns a snapshot of the device state.
func (d *Device) Info() Info {
	d.infoMu.RLock()
	defer d.infoMu.RUnlock()
	info := d.info
	return info
}

func (d *Device) setInfo(fn func(*Info)) {
	d.infoMu.Lock()
	defer d.infoMu.Unlock()
	fn(&d.info)
}

func (d *Device) recordError(err error) {
	now := time.Now().UTC()
	d.setInfo(func(i *Info) {
		i.LastError = err.Error()
		i.LastErrorAt = &now
	})
}

// discard accepts writes and drops them.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (d *Device) usbMatch() func(escpos.USBPrinter) bool {
	c := d.cfg
	if c.VendorID == 0 && c.ProductID == 0 && c.USBSerial == "" {
		return nil
	}
	return func(p escpos.USBPrinter) bool {
		return (c.VendorID == 0 || p.VendorID == c.VendorID) &&
			(c.ProductID == 0 || p.ProductID == c.ProductID) &&
			(c.USBSerial == "" || p.Serial == c.USBSerial)
	}
}

func usbInfo(p escpos.USBPrinter, driver string) *USBInfo {
	return &USBInfo{
		Path:         p.Path,
		VendorID:     fmt.Sprintf("%04x", p.VendorID),
		ProductID:    fmt.Sprintf("%04x", p.ProductID),
		Manufacturer: p.Manufacturer,
		Product:      p.Product,
		Serial:       p.Serial,
		DeviceID:     p.DeviceID,
		Driver:       driver,
	}
}

// open opens the configured connection.
func (d *Device) open(ctx context.Context) (io.Writer, *USBInfo, error) {
	t := d.cfg.Target
	switch t.Kind {
	case KindDiscard:
		return discard{}, nil, nil
	case KindFile:
		f, err := escpos.OpenFile(t.Path)
		return f, nil, err
	case KindTCP:
		c, err := escpos.DialTCP(ctx, t.Addr)
		return c, nil, err
	case KindSerial:
		c, err := serial.Open(t.Path, serial.Options{BaudRate: t.Baud})
		return c, nil, err
	case KindLibUSB:
		c, p, err := openLibusb(d.usbMatch())
		if err != nil {
			return nil, nil, err
		}
		return c, usbInfo(p, "libusb"), nil
	}

	// Auto-detect: the OS printer driver needs no cgo (Linux, Windows);
	// libusb covers macOS and printers not bound to a printer driver.
	match := d.usbMatch()
	f, p, err := escpos.OpenUSB(match)
	if err == nil {
		return f, usbInfo(p, "os"), nil
	}
	if !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, escpos.ErrNoUSBPrinter) {
		return nil, nil, err
	}
	c, lp, lerr := openLibusb(match)
	if errors.Is(lerr, errNoLibusb) {
		return nil, nil, err
	}
	if lerr != nil {
		return nil, nil, fmt.Errorf("%w; libusb: %w", err, lerr)
	}
	return c, usbInfo(lp, "libusb"), nil
}

// connect opens the connection if needed. d.mu must be held.
func (d *Device) connect(ctx context.Context) error {
	if d.printer != nil {
		return nil
	}
	w, usb, err := d.open(ctx)
	if err != nil {
		err = fmt.Errorf("connecting to printer (%s): %w", d.cfg.Target, err)
		d.recordError(err)
		return err
	}
	d.printer = escpos.New(w, escpos.WithPaperWidth(d.cfg.PaperWidth), escpos.WithTimeout(d.cfg.Timeout))
	d.closer, _ = w.(io.Closer)
	_, readable := w.(io.Reader)

	reported := d.queryReported(ctx, readable)
	now := time.Now().UTC()
	d.setInfo(func(i *Info) {
		i.Connected = true
		i.ConnectedAt = &now
		i.Readable = readable
		i.USB = usb
		i.Reported = reported
	})
	return nil
}

// queryReported asks the printer to identify itself. Failures are ignored:
// not every connection or printer answers.
func (d *Device) queryReported(ctx context.Context, readable bool) *Reported {
	if !readable {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p := d.printer
	r := &Reported{}
	model, err := p.PrinterID(ctx, escpos.PrinterIDModel)
	if err != nil {
		// Do not wait out every query if the printer does not answer.
		return nil
	}
	r.ModelID = &model
	if t, err := p.PrinterID(ctx, escpos.PrinterIDTypeID); err == nil {
		mb, ac := escpos.TypeID(t).MultiByte(), escpos.TypeID(t).AutoCutter()
		r.TypeID, r.MultiByte, r.AutoCutter = &t, &mb, &ac
	}
	for _, f := range []struct {
		t   escpos.PrinterIDType
		dst *string
	}{
		{escpos.PrinterInfoFirmware, &r.Firmware},
		{escpos.PrinterInfoManufacturer, &r.Manufacturer},
		{escpos.PrinterInfoName, &r.Name},
		{escpos.PrinterInfoSerial, &r.Serial},
		{escpos.PrinterInfoFonts, &r.Fonts},
	} {
		if s, err := p.PrinterInfo(ctx, f.t); err == nil {
			*f.dst = s
		}
	}
	return r
}

// disconnect closes the connection. d.mu must be held.
func (d *Device) disconnect() {
	if d.closer != nil {
		d.closer.Close()
	}
	d.printer, d.closer = nil, nil
	d.setInfo(func(i *Info) { i.Connected = false })
}

// Close closes the connection.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnect()
}

// Do connects if needed and runs fn with exclusive use of the printer. If fn
// fails with anything but an invalid argument, the connection is dropped so
// the next call reconnects; the printer may have been unplugged and come
// back under a different path.
func (d *Device) Do(ctx context.Context, fn func(p *escpos.Printer) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(ctx); err != nil {
		return err
	}
	err := fn(d.printer)
	if err != nil && !errors.Is(err, escpos.ErrInvalidArgument) {
		d.recordError(err)
		d.disconnect()
	}
	return err
}

// Readable reports whether the current connection can answer queries. It
// connects first if needed.
func (d *Device) Readable(ctx context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(ctx); err != nil {
		return false, err
	}
	return d.Info().Readable, nil
}

// Status reads the real-time status. Unlike Do, a timeout does not drop the
// connection, since some drivers simply never deliver responses.
func (d *Device) Status(ctx context.Context) (escpos.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(ctx); err != nil {
		return escpos.Status{}, err
	}
	s, err := d.printer.Status(ctx)
	if err != nil && !errors.Is(err, escpos.ErrNotReadable) && !errors.Is(err, context.DeadlineExceeded) {
		d.recordError(err)
		d.disconnect()
	}
	return s, err
}
