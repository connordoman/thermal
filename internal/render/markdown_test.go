package render

import (
	"context"
	"github.com/connordoman/escpos/escpostest"
	"os"
	"strings"
	"testing"
)

const sampleMarkdown = "# Receipt Title\n\nSome **bold**, *italic*, `code`, ~~struck~~ and a [link](https://example.com). Visit https://go.dev too.\n\n## List\n\n- [x] Milk\n- [ ] Eggs\n  - nested item\n\n1. First\n2. Second\n\n> A quote with **emphasis**\n> spanning lines that go on for long enough to need wrapping onto another line.\n\n| Item | Qty | Price |\n|:-----|:---:|------:|\n| Coffee | 2 | $7.00 |\n| Bagel with a long name that wraps around | 1 | $3.50 |\n\n```go\nfmt.Println(\"hello\")\n```\n\n---\n\nfootnote[^1]\n\n[^1]: The footnote.\n"

func TestMarkdown(t *testing.T) {
	env := NewEnv(576, nil)
	out, err := RenderMarkdown(context.Background(), env, sampleMarkdown, MarkdownOptions{Links: LinksFootnotes}, Finish{Cut: CutPartial})
	if err != nil {
		t.Fatal(err)
	}
	d := escpostest.Describe(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + d)
	}
	for _, want := range []string{
		"<GS ! 17>Receipt Title",
		"<ESC E 1>bold",
		"<ESC - 1>italic",
		"<GS B 1>code",
		"~struck~",
		"link[1]",
		"- [x] Milk",
		"  * nested item",
		"1. First",
		"│ A quote with ",
		"Coffee",
		"<ESC M 1>fmt.Println(\"hello\")",
		"[1] https://example.com",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, line := range escpostest.Lines(out) {
		if n := len([]rune(line)); n > 64 {
			t.Errorf("line too long (%d): %q", n, line)
		}
	}
}

func TestAlerts(t *testing.T) {
	src := "> [!NOTE]\n> Useful information that users should know, even when skimming. It wraps onto a second line.\n>\n> - a list item\n\n" +
		"- item\n\n  > [!warning]\n  > Inside a list.\n\n" +
		"> [!TIP]\n\n" +
		"> [!NOPE]\n> Not an alert, just a quote.\n"
	out, err := RenderMarkdown(context.Background(), NewEnv(576, nil), src, MarkdownOptions{}, Finish{Cut: CutNone})
	if err != nil {
		t.Fatal(err)
	}
	lines := escpostest.Lines(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + strings.Join(lines, "\n"))
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"┌─ NOTE ─", "│ Useful information", "│ - a list item", "└───",
		"- item\n  ┌─ WARNING ─", "  │ Inside a list.", "  └──",
		"┌─ TIP ─", "│ [!NOPE] Not an alert",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " -"), "┌") && (!strings.HasSuffix(l, "┐") || len([]rune(l)) != 48) {
			t.Errorf("bad top border %q", l)
		}
	}
	// Box lines fill the paper exactly, so the right borders line up.
	for _, l := range lines {
		if strings.Contains(l, "│") && strings.HasSuffix(l, "│") && len([]rune(l)) != 48 {
			t.Errorf("box line is %d columns, want 48: %q", len([]rune(l)), l)
		}
	}
	d := escpostest.Describe(out)
	// Titles are underlined, and the boxes' lines join.
	if !strings.Contains(d, "┌─ <ESC - 1>NOTE<ESC - 0> ─") || !strings.Contains(d, "<ESC 3 24>") {
		t.Errorf("title not underlined or spacing not tight:\n%s", d)
	}
}
