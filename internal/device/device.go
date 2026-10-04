// Package device owns the connection to the printer. It wraps
// escpos.Device, which connects lazily and reconnects after failures, and
// keeps the details /v1/printer reports.
package device

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/connordoman/escpos"
	_ "github.com/connordoman/escpos/serial" // registers serial: connections
)

// Config configures a Device.
type Config struct {
	// Connection is an escpos.Open connection string, such as "usb",
	// "tcp://192.168.1.50" or "serial:/dev/ttyUSB0?baud=9600".
	Connection string
	VendorID   uint16 // added to usb/libusb connection strings that lack one
	ProductID  uint16
	USBSerial  string
	PaperWidth int
	Timeout    time.Duration
}

// ConnectionString returns c.Connection with the USB filters merged in.
func (c Config) ConnectionString() (string, error) {
	conn := c.Connection
	if strings.TrimSpace(conn) == "" {
		conn = "usb"
	}
	spec, err := escpos.ParseSpec(conn)
	if err != nil {
		return "", err
	}
	if !slices.Contains(escpos.Schemes(), spec.Scheme) && spec.Scheme != "auto" && spec.Scheme != "none" {
		return "", fmt.Errorf("unsupported connection %q (want one of %s)", conn, strings.Join(escpos.Schemes(), ", "))
	}
	if spec.Scheme == "usb" || spec.Scheme == "auto" || spec.Scheme == "libusb" {
		set := func(k, v string) {
			if v != "" && spec.Query.Get(k) == "" {
				spec.Query.Set(k, v)
			}
		}
		hex := func(v uint16) string {
			if v == 0 {
				return ""
			}
			return fmt.Sprintf("%04x", v)
		}
		set("vid", hex(c.VendorID))
		set("pid", hex(c.ProductID))
		set("serial", c.USBSerial)
		if _, err := escpos.USBMatch(spec.Query); err != nil {
			return "", err
		}
	}
	return spec.String(), nil
}

// Reported is what the printer says about itself (GS I).
type Reported struct {
	Firmware     string `json:"firmware,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Name         string `json:"name,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Fonts        string `json:"fonts,omitempty"`
	ModelID      byte   `json:"model_id"`
	TypeID       byte   `json:"type_id"`
	MultiByte    bool   `json:"multi_byte"`
	AutoCutter   bool   `json:"auto_cutter"`
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
}

// Info is a snapshot of the device state.
type Info struct {
	Connection      string     `json:"connection"`
	Kind            string     `json:"kind,omitempty"`
	Target          string     `json:"target,omitempty"`
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

// Device is the printer.
type Device struct {
	dev        *escpos.Device
	conn       string
	paperWidth int

	mu          sync.Mutex
	connectedAt time.Time
	reported    *Reported
	lastErr     error
	lastErrAt   time.Time
}

// New returns a device that connects on first use.
func New(cfg Config) (*Device, error) {
	conn, err := cfg.ConnectionString()
	if err != nil {
		return nil, err
	}
	if cfg.PaperWidth == 0 {
		cfg.PaperWidth = escpos.PaperWidth80mm
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = escpos.DefaultTimeout
	}
	d := &Device{conn: conn, paperWidth: cfg.PaperWidth}
	d.dev = escpos.NewDevice(conn, escpos.WithPaperWidth(cfg.PaperWidth), escpos.WithTimeout(cfg.Timeout))
	d.dev.OnConnect = d.onConnect
	return d, nil
}

// ConnectionString returns the connection string in use.
func (d *Device) ConnectionString() string { return d.conn }

// PaperWidth returns the configured printable width in dots.
func (d *Device) PaperWidth() int { return d.paperWidth }

// onConnect asks the printer to identify itself. Not every connection or
// printer answers, which is fine.
func (d *Device) onConnect(ctx context.Context, p *escpos.Printer, c escpos.Connection) {
	var rep *Reported
	if c.Readable {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if id, err := p.Identify(ctx); err == nil {
			rep = &Reported{
				Firmware: id.Firmware, Manufacturer: id.Manufacturer, Name: id.Name,
				Serial: id.Serial, Fonts: id.Fonts, ModelID: id.ModelID, TypeID: byte(id.TypeID),
				MultiByte: id.TypeID.MultiByte(), AutoCutter: id.TypeID.AutoCutter(),
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connectedAt, d.reported = time.Now().UTC(), rep
}

func (d *Device) record(err error) error {
	if err != nil {
		d.mu.Lock()
		d.lastErr, d.lastErrAt = err, time.Now().UTC()
		d.mu.Unlock()
	}
	return err
}

// Info returns a snapshot of the device state.
func (d *Device) Info() Info {
	c, connected := d.dev.Connection()
	d.mu.Lock()
	defer d.mu.Unlock()
	info := Info{
		Connection:      d.conn,
		Connected:       connected,
		PaperWidth:      d.paperWidth,
		LibUSBAvailable: slices.Contains(escpos.Schemes(), "libusb"),
	}
	if connected {
		at := d.connectedAt
		info.Kind, info.Target, info.Readable, info.ConnectedAt, info.Reported = c.Kind, c.Target, c.Readable, &at, d.reported
		if u := c.USB; u != nil {
			info.USB = &USBInfo{
				Path: u.Path, VendorID: fmt.Sprintf("%04x", u.VendorID), ProductID: fmt.Sprintf("%04x", u.ProductID),
				Manufacturer: u.Manufacturer, Product: u.Product, Serial: u.Serial, DeviceID: u.DeviceID,
			}
		}
	}
	if d.lastErr != nil {
		at := d.lastErrAt
		info.LastError, info.LastErrorAt = d.lastErr.Error(), &at
	}
	return info
}

// Readable connects if needed and reports whether the connection can
// answer queries.
func (d *Device) Readable(ctx context.Context) (bool, error) {
	c, err := d.dev.Connect(ctx)
	return c.Readable, d.record(err)
}

// Do runs fn with exclusive use of the printer; see escpos.Device.Do.
func (d *Device) Do(ctx context.Context, fn func(p *escpos.Printer) error) error {
	return d.record(d.dev.Do(ctx, fn))
}

// Status reads the real-time status.
func (d *Device) Status(ctx context.Context) (escpos.Status, error) {
	var s escpos.Status
	err := d.Do(ctx, func(p *escpos.Printer) error {
		var err error
		s, err = p.Status(ctx)
		return err
	})
	return s, err
}

// Disconnect closes the connection so the next use reopens it.
func (d *Device) Disconnect() { d.dev.Disconnect() }

// Close closes the connection.
func (d *Device) Close() error { return d.dev.Close() }
