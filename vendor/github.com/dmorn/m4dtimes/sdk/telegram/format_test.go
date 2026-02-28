package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// --- markdownToTelegramHTML unit tests ---

func TestFormat_Bold(t *testing.T) {
	got := markdownToTelegramHTML("**bold**")
	want := "<b>bold</b>"
	if got != want {
		t.Fatalf("bold: got %q, want %q", got, want)
	}
}

func TestFormat_BoldUnderscore(t *testing.T) {
	got := markdownToTelegramHTML("__bold__")
	want := "<b>bold</b>"
	if got != want {
		t.Fatalf("bold underscore: got %q, want %q", got, want)
	}
}

func TestFormat_Italic(t *testing.T) {
	got := markdownToTelegramHTML("*italic*")
	want := "<i>italic</i>"
	if got != want {
		t.Fatalf("italic: got %q, want %q", got, want)
	}
}

func TestFormat_ItalicUnderscore(t *testing.T) {
	got := markdownToTelegramHTML("_italic_")
	want := "<i>italic</i>"
	if got != want {
		t.Fatalf("italic underscore: got %q, want %q", got, want)
	}
}

func TestFormat_InlineCode(t *testing.T) {
	got := markdownToTelegramHTML("`code`")
	want := "<code>code</code>"
	if got != want {
		t.Fatalf("inline code: got %q, want %q", got, want)
	}
}

func TestFormat_InlineCodeEscapesHTML(t *testing.T) {
	// Inside a code span, < > & must be escaped so Telegram HTML doesn't break.
	got := markdownToTelegramHTML("`Result<T>`")
	want := "<code>Result&lt;T&gt;</code>"
	if got != want {
		t.Fatalf("inline code html escape: got %q, want %q", got, want)
	}
}

func TestFormat_CodeBlockWithGenerics(t *testing.T) {
	// Code block containing generic types — must escape < > & before wrapping.
	input := "```go\nfunc Foo[T any](v T) {}\nmap[string]int{}\n```"
	got := markdownToTelegramHTML(input)

	if !strings.Contains(got, "&lt;") || strings.Contains(got, "<T>") {
		// If there are no generics with < > in this input, check map syntax still works
	}
	// Core requirement: output must contain <pre><code> wrapper and no raw unescaped < > inside it.
	if !strings.Contains(got, "<pre><code") {
		t.Fatalf("code block: missing <pre><code> wrapper, got %q", got)
	}
	// The angle brackets in the Go code (if any) must be escaped.
	if strings.Contains(got, "<T>") {
		t.Fatalf("code block: unescaped generic type <T> found in %q", got)
	}
}

func TestFormat_CodeBlockEscapesAngles(t *testing.T) {
	// Explicit test: code block with angle brackets must be escaped.
	input := "```\nfunc Ptr[T any]() *T { return nil }\nvar x <string>\n```"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "<string>") {
		t.Fatalf("code block: unescaped <string> in output: %q", got)
	}
	if !strings.Contains(got, "&lt;string&gt;") {
		t.Fatalf("code block: expected &lt;string&gt; in output: %q", got)
	}
	if !strings.Contains(got, "<pre><code>") {
		t.Fatalf("code block: missing <pre><code> in output: %q", got)
	}
}

func TestFormat_Link(t *testing.T) {
	got := markdownToTelegramHTML("[text](https://example.com)")
	want := `<a href="https://example.com">text</a>`
	if got != want {
		t.Fatalf("link: got %q, want %q", got, want)
	}
}

func TestFormat_Header(t *testing.T) {
	got := markdownToTelegramHTML("# Header")
	want := "<b>Header</b>"
	if got != want {
		t.Fatalf("h1: got %q, want %q", got, want)
	}
}

func TestFormat_Header2(t *testing.T) {
	got := markdownToTelegramHTML("## Sub-header")
	want := "<b>Sub-header</b>"
	if got != want {
		t.Fatalf("h2: got %q, want %q", got, want)
	}
}

func TestFormat_Strikethrough(t *testing.T) {
	got := markdownToTelegramHTML("~~strike~~")
	want := "<s>strike</s>"
	if got != want {
		t.Fatalf("strike: got %q, want %q", got, want)
	}
}

func TestFormat_Spoiler(t *testing.T) {
	got := markdownToTelegramHTML("||secret||")
	want := "<tg-spoiler>secret</tg-spoiler>"
	if got != want {
		t.Fatalf("spoiler: got %q, want %q", got, want)
	}
}

func TestFormat_Blockquote(t *testing.T) {
	got := markdownToTelegramHTML("> quote me")
	want := "<blockquote>quote me</blockquote>"
	if got != want {
		t.Fatalf("blockquote: got %q, want %q", got, want)
	}
}

func TestFormat_UnorderedList(t *testing.T) {
	got := markdownToTelegramHTML("- item")
	want := "• item"
	if got != want {
		t.Fatalf("unordered list: got %q, want %q", got, want)
	}
}

func TestFormat_OrderedList(t *testing.T) {
	got := markdownToTelegramHTML("1. first")
	want := "1. first"
	if got != want {
		t.Fatalf("ordered list: got %q, want %q", got, want)
	}
}

func TestFormat_PlainTextEscapesEntities(t *testing.T) {
	got := markdownToTelegramHTML("a < b && c > d")
	want := "a &lt; b &amp;&amp; c &gt; d"
	if got != want {
		t.Fatalf("entity escape: got %q, want %q", got, want)
	}
}

func TestFormat_NoMarkdown(t *testing.T) {
	got := markdownToTelegramHTML("hi")
	if got != "hi" {
		t.Fatalf("plain text: got %q, want %q", got, "hi")
	}
}

// --- splitAtNewlines unit tests ---

func TestSplitAtNewlines_ShortText(t *testing.T) {
	chunks := splitAtNewlines("hello", 4096)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("short text: %v", chunks)
	}
}

func TestSplitAtNewlines_SplitsAtNewline(t *testing.T) {
	// Build a text with a newline before the 4096-rune boundary.
	line1 := strings.Repeat("a", 3000) + "\n"
	line2 := strings.Repeat("b", 2000)
	text := line1 + line2

	chunks := splitAtNewlines(text, 4096)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != line1 {
		t.Fatalf("chunk[0] wrong: len=%d", len([]rune(chunks[0])))
	}
	if chunks[1] != line2 {
		t.Fatalf("chunk[1] wrong: len=%d", len([]rune(chunks[1])))
	}
}

func TestSplitAtNewlines_MultipleChunks(t *testing.T) {
	// Three lines each about 2000 chars, total > 4096.
	line := strings.Repeat("x", 1999) + "\n"
	text := line + line + line

	chunks := splitAtNewlines(text, 4096)
	// line is 2000 runes; two lines = 4000 ≤ 4096, three = 6000 > 4096.
	// So first chunk = line+line, second chunk = line.
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c)) > 4096 {
			t.Fatalf("chunk %d exceeds 4096 runes: %d", i, len([]rune(c)))
		}
	}
}

// --- Integration: Send() splits and sends two chunks ---

func TestSend_LongMessageSplits(t *testing.T) {
	var requestCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		requestCount.Add(1)
		b, _ := io.ReadAll(r.Body)
		defer r.Body.Close()
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if body["parse_mode"] != "HTML" {
			t.Errorf("expected parse_mode=HTML, got %v", body["parse_mode"])
		}
		// Verify each chunk is within limit.
		text, _ := body["text"].(string)
		if len([]rune(text)) > maxChunkRunes {
			t.Errorf("chunk exceeds %d runes: %d", maxChunkRunes, len([]rune(text)))
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)

	// Build a message with two logical lines, total > 4096 runes.
	line1 := strings.Repeat("a", 3000) + "\n"
	line2 := strings.Repeat("b", 2000)
	longText := line1 + line2

	if err := c.Send(context.Background(), 42, longText); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("expected 2 sendMessage requests, got %d", got)
	}
}

// --- Integration: HTML parse error triggers plain-text fallback ---

func TestSend_HTMLParseErrorFallback(t *testing.T) {
	var callCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		b, _ := io.ReadAll(r.Body)
		defer r.Body.Close()
		var body map[string]any
		_ = json.Unmarshal(b, &body)

		if n == 1 {
			// First call: simulate Telegram HTML parse error.
			if body["parse_mode"] != "HTML" {
				t.Errorf("first attempt must use parse_mode=HTML, got %v", body["parse_mode"])
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities: Unsupported start tag <T>"}`))
			return
		}
		// Second call: plain text retry.
		if pm, ok := body["parse_mode"]; ok && pm != "" {
			t.Errorf("retry must omit parse_mode, got %v", pm)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":2}}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	if err := c.Send(context.Background(), 7, "some `code<T>` here"); err != nil {
		t.Fatalf("send with fallback: %v", err)
	}
	if got := callCount.Load(); got != 2 {
		t.Fatalf("expected 2 requests (HTML + plain), got %d", got)
	}
}
