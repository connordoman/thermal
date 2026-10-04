package device

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Kind is a type of printer connection.
type Kind string

const (
	KindUSB     Kind = "usb"     // auto-detected; OS printer driver, then libusb
	KindLibUSB  Kind = "libusb"  // auto-detected with libusb only
	KindFile    Kind = "file"    // a device node such as /dev/usb/lp0
	KindTCP     Kind = "tcp"     // Ethernet, raw port 9100
	KindSerial  Kind = "serial"  // RS-232
	KindDiscard Kind = "discard" // jobs are accepted and dropped (testing)
)

// Target is a parsed connection setting.
type Target struct {
	Kind Kind
	Path string // file and serial
	Addr string // tcp
	Baud int    // serial
}

func (t Target) String() string {
	switch t.Kind {
	case KindFile:
		return "file:" + t.Path
	case KindTCP:
		return "tcp://" + t.Addr
	case KindSerial:
		return fmt.Sprintf("serial:%s?baud=%d", t.Path, t.Baud)
	}
	return string(t.Kind)
}

// ParseConnection parses a connection setting such as "usb",
// "file:/dev/usb/lp0", "tcp://10.0.0.5:9100" or "serial:COM3?baud=19200".
func ParseConnection(s string) (Target, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "usb", "auto":
		return Target{Kind: KindUSB}, nil
	case "libusb":
		return Target{Kind: KindLibUSB}, nil
	case "discard", "none":
		return Target{Kind: KindDiscard}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, fmt.Errorf("connection %q: %w", s, err)
	}
	path := u.Path
	if u.Opaque != "" {
		path = u.Opaque
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		if path == "" {
			return Target{}, fmt.Errorf("connection %q: missing path", s)
		}
		return Target{Kind: KindFile, Path: path}, nil
	case "tcp":
		addr := u.Host
		if addr == "" {
			addr = path
		}
		if addr == "" {
			return Target{}, fmt.Errorf("connection %q: missing host", s)
		}
		return Target{Kind: KindTCP, Addr: addr}, nil
	case "serial":
		if path == "" {
			return Target{}, fmt.Errorf("connection %q: missing port", s)
		}
		baud := 9600
		if b := u.Query().Get("baud"); b != "" {
			if baud, err = strconv.Atoi(b); err != nil || baud <= 0 {
				return Target{}, fmt.Errorf("connection %q: invalid baud rate %q", s, b)
			}
		}
		return Target{Kind: KindSerial, Path: path, Baud: baud}, nil
	}
	return Target{}, fmt.Errorf("connection %q: want usb, libusb, discard, file:, tcp:// or serial:", s)
}
