package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/escpostest"
)

func upsideDownEnv(pageHeight int) *Env {
	env := NewEnv(576, NewImageLoader(false, 1<<20, time.Second))
	env.Now = func() time.Time { return time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC) }
	env.UpsideDown = true
	env.PageHeight = pageHeight
	return env
}

// pages returns the commands of each page-mode page in out.
func pages(out []byte) [][]escpostest.Token {
	var all [][]escpostest.Token
	var cur []escpostest.Token
	in := false
	for _, t := range escpostest.Decode(out) {
		switch {
		case t.Cmd == "ESC L":
			in, cur = true, nil
		case t.Cmd == "FF" && in:
			all = append(all, cur)
			in = false
		case in:
			cur = append(cur, t)
		}
	}
	return all
}

func TestUpsideDownText(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	// 30-dot lines, so 4 fit a 120-dot page.
	out, err := RenderText(upsideDownEnv(120), strings.Join(lines, "\n"), TextOptions{Style: boldStyle}, Finish{Cut: CutPartial, Feed: 10})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + escpostest.Describe(out))
	}
	ps := pages(out)
	if len(ps) != 3 {
		t.Fatalf("got %d pages, want 3", len(ps))
	}
	// The last lines print first.
	got := escpostest.Lines(out)
	want := []string{"line 8", "line 9", "line 4", "line 5", "line 6", "line 7", "line 0", "line 1", "line 2", "line 3"}
	if !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	for i, p := range ps {
		if p[0].Cmd != "ESC T" || p[0].Args[0] != 2 {
			t.Errorf("page %d starts with %v, want ESC T 2", i, p[0])
		}
		if p[1].Cmd != "ESC W" {
			t.Fatalf("page %d: second command is %v, want ESC W", i, p[1])
		}
		// Every page restores the style, even though it is only set once.
		if !slices.ContainsFunc(p, func(t escpostest.Token) bool { return t.Cmd == "ESC E" && t.Args[0] == 1 }) {
			t.Errorf("page %d does not turn bold on", i)
		}
	}
	if h := escpostest.Commands(out, "ESC W")[0].Args; h[len(h)-2] != 60 {
		t.Errorf("first page area %v, want 60 dots tall", h)
	}
	// The finish follows the pages.
	cuts := escpostest.Commands(out, "GS V")
	if len(cuts) != 1 || !strings.HasSuffix(escpostest.Describe(out), "<GS V 66 10>") {
		t.Errorf("job does not end with the cut: %v", cuts)
	}
}

var boldStyle = func() (s TextOptions) { s.Style.Bold = true; return }().Style

func TestUpsideDownImageBands(t *testing.T) {
	b := escpos.NewBuilder(576)
	img := image.NewGray(image.Rect(0, 0, 576, 250))
	if err := b.PrintImage(img, escpos.ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := UpsideDown(b.Bytes(), 576, 100)
	if err != nil {
		t.Fatal(err)
	}
	var rows []int
	for _, c := range escpostest.Commands(out, "GS v 0") {
		rows = append(rows, c.Args[2])
	}
	// Bands print last first.
	if !slices.Equal(rows, []int{50, 100, 100}) {
		t.Errorf("bands = %v, want [50 100 100]", rows)
	}
}

func TestUpsideDownKeepsCuts(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Initialize()
	b.Textln("first")
	b.FeedAndCut(0)
	b.Textln("second")
	out, err := UpsideDown(b.Bytes(), 576, 1000)
	if err != nil {
		t.Fatal(err)
	}
	d := escpostest.Describe(out)
	if i, j, k := strings.Index(d, "first"), strings.Index(d, "<GS V"), strings.Index(d, "second"); i < 0 || !(i < j && j < k) {
		t.Errorf("cut moved:\n%s", d)
	}
	if n := len(pages(out)); n != 2 {
		t.Errorf("got %d pages, want 2", n)
	}
}

func TestUpsideDownQRCodeStaysWhole(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Textln("above")
	if err := b.PrintQRCode("https://example.com", escpos.QRErrorM, 4); err != nil {
		t.Fatal(err)
	}
	// One line per page, so the QR code's commands would be split if they
	// could be.
	out, err := UpsideDown(b.Bytes(), 576, 30)
	if err != nil {
		t.Fatal(err)
	}
	ps := pages(out)
	if len(ps) != 2 {
		t.Fatalf("got %d pages, want 2", len(ps))
	}
	var fns []int
	for _, c := range ps[0] {
		if c.Cmd == "GS ( k" {
			fns = append(fns, c.Args[1])
		}
	}
	if !slices.Equal(fns, []int{65, 67, 69, 80, 81}) {
		t.Errorf("first page QR functions = %v, want all of them", fns)
	}
	// Version 2 at 4 dots a module.
	if h := escpostest.Commands(out, "ESC W")[0].Args; h[len(h)-2]|h[len(h)-1]<<8 != 25*4 {
		t.Errorf("QR page area %v, want 100 dots tall", h)
	}
}

func TestUpsideDownExamples(t *testing.T) {
	for _, name := range []string{"receipt.json", "cut.json", "everything.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("../../examples/" + name)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := ParseDocument(body)
			if err != nil {
				t.Fatal(err)
			}
			// The scanner agrees with escpostest on every command.
			plain, err := RenderDocument(context.Background(), upsideDownEnv(0), &Document{Blocks: doc.Blocks, Options: doc.Options})
			if name == "everything.json" {
				if !errors.Is(err, ErrUpsideDown) {
					t.Fatalf("err = %v, want ErrUpsideDown (it has a page block)", err)
				}
				env := upsideDownEnv(0)
				env.UpsideDown = false
				if plain, err = RenderDocument(context.Background(), env, doc); err != nil {
					t.Fatal(err)
				}
				checkScanner(t, plain)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(pages(plain)) == 0 && len(doc.Blocks) > 0 {
				t.Error("no pages")
			}
			if os.Getenv("SHOW") != "" {
				t.Log("\n" + escpostest.Describe(plain))
			}
			checkScanner(t, plain)
		})
	}
}

// checkScanner checks that cmdLen splits b into the same commands as
// escpostest.
func checkScanner(t *testing.T, b []byte) {
	t.Helper()
	var want []string
	for _, tok := range escpostest.Decode(b) {
		if !tok.IsText() {
			want = append(want, tok.Cmd)
		}
	}
	got := 0
	for i := 0; i < len(b); {
		if c := b[i]; c >= 0x20 && c != 0x7F {
			i++
			continue
		}
		n := cmdLen(b[i:])
		if n == 0 {
			t.Fatalf("cmdLen does not know %s at byte %d", cmdName(b[i:]), i)
		}
		i += n
		got++
	}
	if got != len(want) {
		t.Errorf("cmdLen found %d commands, escpostest %d", got, len(want))
	}
}
