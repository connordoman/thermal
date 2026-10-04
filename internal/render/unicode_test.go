package render

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/connordoman/escpos"
)

func TestRenderUnicode(t *testing.T) {
	text := "Hello, 世界! 😀👍🏽🎉🍕★♥ café naïve é\nこんにちは、プリンターです。日本語の折り返しを確認するための長い文章です。\nΣωκράτης Привет مرحبا ✓ ★ ♥ →"
	img, err := RenderUnicode(text, escpos.PaperWidth80mm, UnicodeOptions{Scale: 2, Align: escpos.AlignLeft})
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != escpos.PaperWidth80mm || img.Bounds().Dy() < 32 {
		t.Fatalf("bounds %v", img.Bounds())
	}
	if dir := os.Getenv("RENDER_OUT"); dir != "" {
		f, _ := os.Create(filepath.Join(dir, "unicode.png"))
		png.Encode(f, img)
		f.Close()
	}
}
