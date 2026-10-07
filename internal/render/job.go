package render

import (
	"context"
	"fmt"
	"github.com/connordoman/escpos/unifont"
	"image"
	"strings"
	"sync/atomic"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/escpos/layout"
)

// Env holds what renderers need besides the request itself.
type Env struct {
	PaperWidth int
	Images     *ImageLoader
	Now        func() time.Time
	// CutFeed is the paper, in dots, fed past the last line before a cut
	// that does not give its own feed.
	CutFeed uint8
	// UpsideDown prints the job turned 180°, in pages up to PageHeight
	// dots tall (see [UpsideDown]).
	UpsideDown bool
	PageHeight int

	images atomic.Int32
}

// NewEnv returns an environment for rendering one job.
func NewEnv(paperWidth int, images *ImageLoader) *Env {
	return &Env{PaperWidth: paperWidth, Images: images, Now: time.Now}
}

func (e *Env) loadImage(ctx context.Context, src string) (image.Image, error) {
	if e.Images == nil {
		return nil, &ImageError{"", fmt.Errorf("images are disabled")}
	}
	if e.images.Add(1) > MaxImagesPerJob {
		return nil, &ImageError{"", fmt.Errorf("a job may contain at most %d images", MaxImagesPerJob)}
	}
	return e.Images.Load(ctx, src)
}

// CutMode is how a job ends.
type CutMode string

const (
	CutNone    CutMode = "none"
	CutPartial CutMode = "partial"
	CutFull    CutMode = "full"
)

// ParseCutMode parses a cut mode, case-insensitively. Empty means partial.
func ParseCutMode(s string) (CutMode, error) {
	switch m := CutMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return CutPartial, nil
	case CutNone, CutPartial, CutFull:
		return m, nil
	}
	return "", fmt.Errorf("cut must be none, partial or full, not %q", s)
}

// Finish says what happens after a job's content.
type Finish struct {
	Cut CutMode
	// Feed is extra paper, in dots, fed past the last line before cutting
	// (0–255). The printer's cutter sits about 2 cm above the print head,
	// so this is on top of the paper needed to reach the cutter.
	Feed       uint8
	OpenDrawer bool
	Beep       bool
}

// NewJob returns a builder that starts by initialising the printer, so no
// state leaks between jobs.
func (e *Env) NewJob() (*escpos.Builder, *layout.Writer) {
	b := escpos.NewBuilder(e.PaperWidth)
	b.Initialize()
	return b, layout.New(b)
}

// Apply ends a job according to f.
func (f Finish) Apply(b *escpos.Builder) {
	switch f.Cut {
	case CutFull:
		b.FeedAndFullCut(f.Feed)
	case CutNone:
		if f.Feed > 0 {
			b.FeedUnits(f.Feed)
		}
	default:
		b.FeedAndCut(f.Feed)
	}
	if f.OpenDrawer {
		b.OpenCashDrawer(escpos.DrawerPin2)
	}
	if f.Beep {
		b.Beep(1, 2)
	}
}

// finish ends a job with f, after turning its content upside down if the
// environment asks for it.
func (e *Env) finish(b *escpos.Builder, f Finish) ([]byte, error) {
	if !e.UpsideDown {
		f.Apply(b)
		return b.Bytes(), nil
	}
	body, err := UpsideDown(b.Bytes(), e.PaperWidth, e.PageHeight)
	if err != nil {
		return nil, err
	}
	b.Reset()
	f.Apply(b)
	return append(body, b.Bytes()...), nil
}

// TextOptions controls plain-text jobs.
type TextOptions struct {
	Style layout.Style
	Align escpos.Align
	// NoWrap leaves wrapping to the printer, which breaks mid-word.
	NoWrap bool
}

// RenderText prints text transliterated to ASCII.
func RenderText(env *Env, text string, o TextOptions, f Finish) ([]byte, error) {
	b, w := env.NewJob()
	w.SetAlign(o.Align)
	s := strings.TrimRight(ASCII(text), "\n")
	if o.NoWrap {
		for line := range strings.SplitSeq(s, "\n") {
			w.Line(layout.Span{Text: line, Style: o.Style})
		}
	} else {
		w.Text(s, o.Style)
	}
	return env.finish(b, f)
}

// RenderUnicodeText draws text as an image so every character prints.
func RenderUnicodeText(env *Env, text string, o unifont.Options, f Finish) ([]byte, error) {
	b, _ := env.NewJob()
	if err := unifont.Print(b, strings.TrimRight(text, "\n"), o); err != nil {
		return nil, err
	}
	return env.finish(b, f)
}

// RenderMarkdown prints GitHub-flavoured Markdown.
func RenderMarkdown(ctx context.Context, env *Env, src string, o MarkdownOptions, f Finish) ([]byte, error) {
	b, w := env.NewJob()
	if err := WriteMarkdown(ctx, env, w, src, o); err != nil {
		return nil, err
	}
	return env.finish(b, f)
}
