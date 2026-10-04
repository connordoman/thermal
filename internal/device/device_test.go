package device

import "testing"

func TestConnectionString(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, "usb"},
		{Config{Connection: "usb", VendorID: 0x0fe6, ProductID: 0x811e}, "usb?pid=811e&vid=0fe6"},
		{Config{Connection: "usb?vid=1234", VendorID: 0x0fe6}, "usb?vid=1234"},
		{Config{Connection: "tcp://10.0.0.5:9100", VendorID: 0x0fe6}, "tcp://10.0.0.5:9100"},
		{Config{Connection: "serial:/dev/ttyUSB0?baud=19200"}, "serial:/dev/ttyUSB0?baud=19200"},
		{Config{Connection: "file:/dev/usb/lp0"}, "file:/dev/usb/lp0"},
		{Config{Connection: "discard"}, "discard"},
	} {
		got, err := tc.cfg.ConnectionString()
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %q, %v; want %q", tc.cfg, got, err, tc.want)
		}
	}
	for _, bad := range []string{"ftp://x", "usb?vid=zz"} {
		if _, err := (Config{Connection: bad}).ConnectionString(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
