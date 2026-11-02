package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/escpostest"
)

func upsideDownEnv() *Env {
	env := NewEnv(576, NewImageLoader(false, 1<<20, time.Second))
	env.Now = func() time.Time { return time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC) }
	env.UpsideDown = true
	return env
}

var boldStyle = func() (s TextOptions) { s.Style.Bold = true; return }().Style

func TestUpsideDownText(t *testing.T) {
	var lines []string
	for i := range 4 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	out, err := RenderText(upsideDownEnv(), strings.Join(lines, "\n"), TextOptions{Style: boldStyle}, Finish{Cut: CutPartial, Feed: 10})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + escpostest.Describe(out))
	}
	// The last line prints first, in standard mode.
	if got, want := escpostest.Lines(out), []string{"line 3", "line 2", "line 1", "line 0"}; !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	for _, c := range []string{"ESC L", "ESC T", "ESC W", "FF"} {
		if n := len(escpostest.Commands(out, c)); n != 0 {
			t.Errorf("%d × %s: upside-down jobs stay in standard mode", n, c)
		}
	}
	// The printer turns the lines, and once is enough. Bold is restored once.
	if n := len(escpostest.Commands(out, "ESC {")); n != 2 {
		t.Errorf("%d × ESC {, want on and off", n)
	}
	if n := len(escpostest.Commands(out, "ESC E")); n != 1 {
		t.Errorf("%d × ESC E, want 1", n)
	}
	if !strings.HasSuffix(escpostest.Describe(out), "<GS V 66 10>") {
		t.Errorf("job does not end with the cut:\n%s", escpostest.Describe(out))
	}
}

func TestUpsideDownStateFollowsEachLine(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Initialize()
	b.Textln("plain")
	b.SetEmphasis(true)
	b.Textln("bold")
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// Reversed, "bold" prints first, and "plain" must not inherit its style.
	d := escpostest.Describe(out)
	i, j := strings.Index(d, "bold"), strings.Index(d, "plain")
	if i < 0 || j < i || !strings.Contains(d[i:j], "<ESC @>") {
		t.Errorf("plain is not reset after bold:\n%s", d)
	}
}

func TestUpsideDownFeeds(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Textln("a")
	b.FeedUnits(40)
	b.Textln("b")
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The gap stays between the lines.
	d := escpostest.Describe(out)
	if i, j, k := strings.Index(d, "b"), strings.Index(d, "<ESC J 40>"), strings.LastIndex(d, "a"); !(0 <= i && i < j && j < k) {
		t.Errorf("gap moved:\n%s", d)
	}
}

func TestUpsideDownImageIsTurned(t *testing.T) {
	b := escpos.NewBuilder(576)
	img := image.NewGray(image.Rect(0, 0, 16, 2))
	for x := range 16 { // top row black on the left, bottom row on the right
		img.SetGray(x, 0, grayOf(x < 8))
		img.SetGray(x, 1, grayOf(x >= 8))
	}
	if err := b.PrintImage(img, escpos.ImageOptions{Threshold: 128}); err != nil {
		t.Fatal(err)
	}
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var raster []byte
	for i := range out {
		if i+8 < len(out) && out[i] == 0x1D && out[i+1] == 'v' {
			raster = out[i+8 : i+12]
		}
	}
	// Rows swap and each is mirrored: the left half black, then the right.
	if want := []byte{0xFF, 0x00, 0x00, 0xFF}; !slices.Equal(raster, want) {
		t.Errorf("raster = %x, want %x", raster, want)
	}
	// The printer does not turn it, so left-aligned becomes right-aligned.
	if a := escpostest.Commands(out, "ESC a"); len(a) != 1 || a[0].Args[0] != 2 {
		t.Errorf("alignment = %v, want right", a)
	}
	if len(escpostest.Commands(out, "ESC {")) != 0 {
		t.Error("an image alone needs no ESC {")
	}
}

func grayOf(black bool) color.Gray {
	if black {
		return color.Gray{}
	}
	return color.Gray{Y: 255}
}

func TestUpsideDownKeepsCuts(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Initialize()
	b.Textln("first")
	b.FeedAndCut(0)
	b.Textln("second")
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	d := escpostest.Describe(out)
	if i, j, k := strings.Index(d, "first"), strings.Index(d, "<GS V"), strings.Index(d, "second"); i < 0 || !(i < j && j < k) {
		t.Errorf("cut moved:\n%s", d)
	}
}

func TestUpsideDownSymbols(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.Textln("above")
	if err := b.PrintQRCode("https://example.com", escpos.QRErrorM, 4); err != nil {
		t.Fatal(err)
	}
	b.Textln("below")
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The QR code stays whole between the lines, now in reverse.
	var fns []int
	for _, c := range escpostest.Commands(out, "GS ( k") {
		fns = append(fns, c.Args[1])
	}
	if !slices.Equal(fns, []int{65, 67, 69, 80, 81}) {
		t.Errorf("QR functions = %v, want all of them once", fns)
	}
	if got := escpostest.Lines(out); !slices.Equal(got, []string{"below", "above"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestUpsideDownDropsWhatItCannotTurn(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.SetLeftMargin(100)
	b.SetPrintAreaWidth(300)
	b.SetRotate90(true)
	b.Textln("x")
	out, err := UpsideDown(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"GS L", "GS W", "ESC V"} {
		if n := len(escpostest.Commands(out, c)); n != 0 {
			t.Errorf("%d × %s survived", n, c)
		}
	}
}

func TestUpsideDownRefusesPageMode(t *testing.T) {
	b := escpos.NewBuilder(576)
	b.EnterPageMode()
	b.Textln("x")
	b.PrintPageAndExit()
	if _, err := UpsideDown(b.Bytes()); !errors.Is(err, ErrUpsideDown) {
		t.Errorf("err = %v, want ErrUpsideDown", err)
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
			out, err := RenderDocument(context.Background(), upsideDownEnv(), doc)
			if name == "everything.json" {
				// It has a page block.
				if !errors.Is(err, ErrUpsideDown) {
					t.Fatalf("err = %v, want ErrUpsideDown", err)
				}
				env := upsideDownEnv()
				env.UpsideDown = false
				plain, err := RenderDocument(context.Background(), env, doc)
				if err != nil {
					t.Fatal(err)
				}
				checkScanner(t, plain)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if os.Getenv("SHOW") != "" {
				t.Log("\n" + escpostest.Describe(out))
			}
			if len(doc.Blocks) > 0 && len(escpostest.Commands(out, "ESC {")) == 0 {
				t.Error("nothing was turned")
			}
			checkScanner(t, out)
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

func TestUpsideDownIgnoresMargins(t *testing.T) {
	doc, err := ParseDocument([]byte(`{"version":1,"blocks":[
		{"type":"margins","left":100,"width":300},
		{"type":"text","content":"x","style":{"marginLeft":50,"width":200}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderDocument(context.Background(), upsideDownEnv(), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"GS L", "GS W"} {
		if n := len(escpostest.Commands(out, c)); n != 0 {
			t.Errorf("%d × %s", n, c)
		}
	}
	env := upsideDownEnv()
	env.UpsideDown = false
	if out, err = RenderDocument(context.Background(), env, doc); err != nil || len(escpostest.Commands(out, "GS L")) == 0 {
		t.Errorf("upright jobs keep their margins: %v", err)
	}
}
