package render

import (
	"context"
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
	d := describe(out)
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + d)
	}
	for _, want := range []string{
		"<SIZE 11>Receipt Title",
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
	for _, line := range strings.Split(d, "\n") {
		plain := stripCommands(line)
		if n := len([]rune(strings.TrimSuffix(plain, "⏎"))); n > 64 {
			t.Errorf("line too long (%d): %q", n, plain)
		}
	}
}

func stripCommands(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
