package device

import "testing"

func TestParseConnection(t *testing.T) {
	for in, want := range map[string]Target{
		"":                               {Kind: KindUSB},
		"usb":                            {Kind: KindUSB},
		"libusb":                         {Kind: KindLibUSB},
		"discard":                        {Kind: KindDiscard},
		"file:/dev/usb/lp0":              {Kind: KindFile, Path: "/dev/usb/lp0"},
		"file:///dev/usb/lp0":            {Kind: KindFile, Path: "/dev/usb/lp0"},
		"tcp://10.0.0.5:9100":            {Kind: KindTCP, Addr: "10.0.0.5:9100"},
		"tcp://printer.local":            {Kind: KindTCP, Addr: "printer.local"},
		"serial:/dev/ttyUSB0":            {Kind: KindSerial, Path: "/dev/ttyUSB0", Baud: 9600},
		"serial:COM3?baud=19200":         {Kind: KindSerial, Path: "COM3", Baud: 19200},
		"serial:///dev/ttyS0?baud=38400": {Kind: KindSerial, Path: "/dev/ttyS0", Baud: 38400},
	} {
		got, err := ParseConnection(in)
		if err != nil || got != want {
			t.Errorf("ParseConnection(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://x", "file:", "serial:/dev/x?baud=fast", "tcp://"} {
		if _, err := ParseConnection(bad); err == nil {
			t.Errorf("ParseConnection(%q) accepted", bad)
		}
	}
}
