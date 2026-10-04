package render

import (
	"context"
	"errors"
	"github.com/connordoman/escpos/escpostest"
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
	d := escpostest.Describe(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + d)
	}
	return d
}

func TestReceiptExample(t *testing.T) {
	d := renderExample(t, "receipt.json")
	for _, want := range []string{
		"YUMMY COFFEE",
		"Sat 3 Oct 2026 02:30", // America/Vancouver
		"2 x Flat white",
		"Subtotal ...",
		"<GS k 73",
		"<GS ( k",
		"(576×", // the Unicode line falls back to an image
		"<GS V 66 0>",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestEverythingExample(t *testing.T) {
	d := renderExample(t, "everything.json")
	for _, want := range []string{
		"╔", "┌", "│", "9. One", "<ESC t 16>", "<ESC R 3>", "Raw bytes", "<ESC B 1 2>", "<ESC p 0 50 250>",
		"<ESC L>", "<FF>", "Ticket #",
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
	if d := escpostest.Describe(out); !strings.Contains(d, "hello, world") || !strings.Contains(d, "<GS V 66 0>") {
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

func renderJSON(t *testing.T, env *Env, body string) (string, error) {
	t.Helper()
	doc, err := ParseDocument([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderDocument(context.Background(), env, doc)
	return escpostest.Describe(out), err
}

func TestCSSStyles(t *testing.T) {
	d, err := renderJSON(t, NewEnv(576, nil), `[
		{"type": "group", "style": {"lineHeight": 1.5, "marginLeft": 48, "textAlign": "center"}, "blocks": [
			{"type": "text", "content": "inside", "style": {"fontWeight": 700, "fontSize": 48, "textTransform": "uppercase"}}
		]},
		{"type": "text", "content": "after"}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<ESC 3 36><GS L 48 0><GS W 16 2>", // line height and margins for the group
		"<ESC a 1><ESC E 1><GS ! 17>INSIDE",
		"<GS L 0 0><GS W 64 2><ESC 2>", // restored afterwards
	} {
		if !strings.Contains(d, want) {
			t.Errorf("output lacks %q:\n%s", want, d)
		}
	}
}

func TestCSSErrors(t *testing.T) {
	_, err := renderJSON(t, NewEnv(576, nil), `[{"type":"text","content":"x"},{"type":"text","content":"x","style":{"color":"red"}}]`)
	var be *BlockError
	if !errors.As(err, &be) || be.Path != "/blocks/1" || !strings.Contains(err.Error(), "color") {
		t.Errorf("got %v", err)
	}
	if _, err := ParseDocument([]byte(`[{"type":"text","content":"x","style":{"bold":true}}]`)); err == nil {
		t.Error("old-style property accepted")
	}
}

func TestDefaultCutFeed(t *testing.T) {
	env := NewEnv(576, nil)
	env.CutFeed = 48
	for body, want := range map[string]string{
		`[{"type":"text","content":"x"}]`:                                     "<GS V 66 48>",
		`{"options":{"feed":10},"blocks":[{"type":"text","content":"x"}]}`:    "<GS V 66 10>",
		`[{"type":"text","content":"x"},{"type":"cut"}]`:                      "<GS V 66 48>",
		`{"options":{"cut":"none"},"blocks":[{"type":"text","content":"x"}]}`: "x⏎\n",
	} {
		d, err := renderJSON(t, env, body)
		if err != nil || !strings.HasSuffix(d, want) {
			t.Errorf("%s: got %q, %v; want suffix %q", body, d, err, want)
		}
	}
}

func TestAlertBlock(t *testing.T) {
	doc := `[
		{"type": "alert", "variant": "note", "content": "Plain text inside the box."},
		{"type": "alert", "variant": "WARNING", "title": "Heads up", "style": {"borderStyle": "double", "fontWeight": "bold"},
		 "content": "First line.", "markdown": "- a list\n- in the box"},
		{"type": "alert", "variant": "TIP", "style": {"marginLeft": 96}, "content": "Narrow"}
	]`
	parsed, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderDocument(context.Background(), NewEnv(576, nil), parsed)
	if err != nil {
		t.Fatal(err)
	}
	lines := escpostest.Lines(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + strings.Join(lines, "\n"))
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"┌─ NOTE ─", "│ Plain text inside the box.", "╔═ HEADS UP ═", "║ First line.", "║ - a list", "╚═══",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, l := range lines {
		if n := len([]rune(l)); strings.ContainsAny(l, "│║") && n != 48 && n != 40 {
			t.Errorf("box line is %d columns: %q", n, l)
		}
	}
	d := escpostest.Describe(out)
	if !strings.Contains(d, "<ESC E 1>╔═ <ESC - 1>HEADS UP") || !strings.Contains(d, "<GS L 96 0>") {
		t.Errorf("bold double box or margin missing:\n%s", d)
	}
	if _, err := ParseDocument([]byte(`[{"type":"alert","variant":"DANGER"}]`)); err == nil {
		t.Error("unknown variant accepted")
	}
}
