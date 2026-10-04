package render

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func renderExample(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("../../examples/" + name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(body)
	if err != nil {
		t.Fatal(err)
	}
	env := NewEnv(576, NewImageLoader(false, 1<<20, time.Second))
	env.Now = func() time.Time { return time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC) }
	out, err := RenderDocument(context.Background(), env, doc)
	if err != nil {
		t.Fatal(err)
	}
	d := describe(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + d)
	}
	return d
}

func TestReceiptExample(t *testing.T) {
	d := renderExample(t, "receipt.json")
	for _, want := range []string{
		"DOMAN COFFEE",
		"Sat 3 Oct 2026 02:30", // America/Vancouver
		"2 x Flat white",
		"Subtotal ...",
		"<BARCODE 73",
		"<GS ( k",
		"<RASTER 576x", // the Unicode line falls back to an image
		"<CUT 66 0>",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestEverythingExample(t *testing.T) {
	d := renderExample(t, "everything.json")
	for _, want := range []string{
		"╔", "┌", "│", "9. One", "<ESC t 16>", "<ESC R 3>", "Raw bytes", "<BEEP [1 2]>", "<PULSE [0 50 250]>",
		"<ESC 'L'>", "<0c>", "Ticket #",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestSchemaErrors(t *testing.T) {
	for _, tc := range []struct {
		body, path string
	}{
		{`{"blocks":[{"type":"text"}]}`, "/blocks/0"},
		{`[{"type":"qr_code","content":"x","size":40}]`, "/0/size"},
		{`{"blocks":[{"type":"group","blocks":[{"type":"cut","bogus":1}]}]}`, "/blocks/0/blocks/0"},
		{`{"blocks":[],"extra":true}`, "/"},
		{`[{"type":"image","src":"x","width":2000}]`, "/0/width"},
	} {
		_, err := ParseDocument([]byte(tc.body))
		var se *SchemaError
		if !errors.As(err, &se) {
			t.Errorf("%s: got %v, want a schema error", tc.body, err)
			continue
		}
		found := false
		for _, p := range se.Problems {
			found = found || p.Path == tc.path
		}
		if !found {
			t.Errorf("%s: problems %+v lack path %s", tc.body, se.Problems, tc.path)
		}
	}
}

func TestBareBlockArray(t *testing.T) {
	doc, err := ParseDocument([]byte(`[{"type":"text","content":"hello, world"},{"type":"qr_code","content":"https://example.com"},{"type":"cut","content":"PARTIAL"}]`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderDocument(context.Background(), NewEnv(576, nil), doc)
	if err != nil {
		t.Fatal(err)
	}
	if d := describe(out); !strings.Contains(d, "hello, world") || !strings.Contains(d, "<CUT 66 0>") {
		t.Errorf("unexpected output %q", d)
	}
}

func TestNestingLimit(t *testing.T) {
	body := strings.Repeat(`{"type":"group","blocks":[`, 12) + `{"type":"text","content":"deep"}` + strings.Repeat(`]}`, 12)
	doc, err := ParseDocument([]byte("[" + body + "]"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RenderDocument(context.Background(), NewEnv(576, nil), doc)
	var be *BlockError
	if !errors.As(err, &be) {
		t.Fatalf("got %v, want a block error", err)
	}
}
