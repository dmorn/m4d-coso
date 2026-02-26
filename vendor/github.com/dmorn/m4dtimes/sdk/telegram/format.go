package telegram

import (
	"fmt"
	"regexp"
	"strings"
)

// markdownToTelegramHTML converts a subset of Markdown to Telegram-supported HTML.
//
// Supported conversions:
//   - **text** or __text__           → <b>text</b>
//   - *text* or _text_               → <i>text</i>
//   - ~~text~~                        → <s>text</s>
//   - `text`                          → <code>text</code>
//   - ```lang\ntext\n```              → <pre><code>text</code></pre>
//   - [text](url)                     → <a href="url">text</a>
//   - > text                          → <blockquote>text</blockquote>
//   - ||text||                        → <tg-spoiler>text</tg-spoiler>
//   - # Header                        → <b>Header</b>
//   - - item / * item                 → • item
//   - 1. item                         → 1. item  (preserved)
//   - ---  (HR)                       → skipped
//
// CRITICAL: inside code spans and code blocks, < > & are escaped to HTML entities
// before wrapping, preventing parse errors when code contains generics or XML.
// In all other plain text, < > & are also escaped.
func markdownToTelegramHTML(text string) string {
	// We work with a list of segments: protected ones (already valid HTML,
	// must not be further processed) and raw ones (to be converted).
	type segment struct {
		html      string
		protected bool
	}

	// --- Phase 1: extract fenced code blocks ---
	fencedRe := regexp.MustCompile("(?s)```([a-zA-Z0-9]*)\n?(.*?)```")
	var segs []segment

	addRaw := func(s string) {
		if s != "" {
			segs = append(segs, segment{html: s})
		}
	}
	addProtected := func(s string) {
		segs = append(segs, segment{html: s, protected: true})
	}

	last := 0
	for _, loc := range fencedRe.FindAllStringSubmatchIndex(text, -1) {
		addRaw(text[last:loc[0]])
		lang := text[loc[2]:loc[3]]
		code := text[loc[4]:loc[5]]
		escapedCode := htmlEscape(code)
		var rendered string
		if lang != "" {
			rendered = fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", lang, escapedCode)
		} else {
			rendered = fmt.Sprintf("<pre><code>%s</code></pre>", escapedCode)
		}
		addProtected(rendered)
		last = loc[1]
	}
	addRaw(text[last:])

	// --- Phase 2: within raw segments, extract inline code spans ---
	inlineCodeRe := regexp.MustCompile("`([^`]+)`")
	var segs2 []segment
	for _, seg := range segs {
		if seg.protected {
			segs2 = append(segs2, seg)
			continue
		}
		s := seg.html
		last2 := 0
		for _, loc := range inlineCodeRe.FindAllStringSubmatchIndex(s, -1) {
			if loc[0] > last2 {
				segs2 = append(segs2, segment{html: s[last2:loc[0]]})
			}
			code := s[loc[2]:loc[3]]
			segs2 = append(segs2, segment{
				html:      fmt.Sprintf("<code>%s</code>", htmlEscape(code)),
				protected: true,
			})
			last2 = loc[1]
		}
		if last2 < len(s) {
			segs2 = append(segs2, segment{html: s[last2:]})
		}
	}

	// --- Phase 3: process each raw segment ---
	// For raw segments we: escape HTML entities, then apply block/inline patterns.
	var out strings.Builder
	for _, seg := range segs2 {
		if seg.protected {
			out.WriteString(seg.html)
			continue
		}
		out.WriteString(convertRawMarkdown(seg.html))
	}

	return out.String()
}

// convertRawMarkdown converts a raw (non-code) markdown segment to Telegram HTML.
// It escapes plain text entities and applies block/inline patterns.
func convertRawMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	var out strings.Builder
	for i, line := range lines {
		converted := convertLine(line)
		out.WriteString(converted)
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

var (
	reHeader        = regexp.MustCompile(`^#{1,6}\s+(.+)$`)
	reBlockquote    = regexp.MustCompile(`^>\s*(.*)$`)
	reUnorderedList = regexp.MustCompile(`^[-*+]\s+(.+)$`)
	reOrderedList   = regexp.MustCompile(`^(\d+)\.\s+(.+)$`)
	reHRule         = regexp.MustCompile(`^(?:---+|===+|\*\*\*+|-\s-\s-)$`)
)

func convertLine(line string) string {
	// Horizontal rule → skip (empty line)
	if reHRule.MatchString(strings.TrimSpace(line)) {
		return ""
	}
	// Header
	if m := reHeader.FindStringSubmatch(line); m != nil {
		return "<b>" + convertInline(m[1]) + "</b>"
	}
	// Blockquote
	if m := reBlockquote.FindStringSubmatch(line); m != nil {
		return "<blockquote>" + convertInline(m[1]) + "</blockquote>"
	}
	// Unordered list
	if m := reUnorderedList.FindStringSubmatch(line); m != nil {
		return "• " + convertInline(m[1])
	}
	// Ordered list
	if m := reOrderedList.FindStringSubmatch(line); m != nil {
		return m[1] + ". " + convertInline(m[2])
	}
	return convertInline(line)
}

// convertInline applies inline markdown patterns to a line of text.
// It first escapes HTML entities in plain-text spans, then applies patterns.
func convertInline(text string) string {
	// We apply replacements using a token approach. Process the string for
	// inline patterns in priority order, protecting already-replaced spans.
	// Strategy: build output by scanning left-to-right, matching patterns greedily.

	var buf strings.Builder
	i := 0
	runes := []rune(text)
	n := len(runes)

	for i < n {
		// Try each pattern at position i in priority order.

		// **bold** (double asterisk)
		if i+3 < n && runes[i] == '*' && runes[i+1] == '*' {
			if j := findClosing(runes, i+2, "**"); j >= 0 {
				buf.WriteString("<b>")
				buf.WriteString(convertInline(string(runes[i+2 : j])))
				buf.WriteString("</b>")
				i = j + 2
				continue
			}
		}
		// __bold__ (double underscore)
		if i+3 < n && runes[i] == '_' && runes[i+1] == '_' {
			if j := findClosing(runes, i+2, "__"); j >= 0 {
				buf.WriteString("<b>")
				buf.WriteString(convertInline(string(runes[i+2 : j])))
				buf.WriteString("</b>")
				i = j + 2
				continue
			}
		}
		// ~~strikethrough~~
		if i+3 < n && runes[i] == '~' && runes[i+1] == '~' {
			if j := findClosing(runes, i+2, "~~"); j >= 0 {
				buf.WriteString("<s>")
				buf.WriteString(convertInline(string(runes[i+2 : j])))
				buf.WriteString("</s>")
				i = j + 2
				continue
			}
		}
		// ||spoiler||
		if i+3 < n && runes[i] == '|' && runes[i+1] == '|' {
			if j := findClosing(runes, i+2, "||"); j >= 0 {
				buf.WriteString("<tg-spoiler>")
				buf.WriteString(convertInline(string(runes[i+2 : j])))
				buf.WriteString("</tg-spoiler>")
				i = j + 2
				continue
			}
		}
		// *italic* (single asterisk, not followed by another asterisk)
		if runes[i] == '*' && (i+1 >= n || runes[i+1] != '*') {
			if j := findClosingSingle(runes, i+1, '*'); j >= 0 {
				buf.WriteString("<i>")
				buf.WriteString(convertInline(string(runes[i+1 : j])))
				buf.WriteString("</i>")
				i = j + 1
				continue
			}
		}
		// _italic_ (single underscore, not followed by another underscore)
		if runes[i] == '_' && (i+1 >= n || runes[i+1] != '_') {
			if j := findClosingSingle(runes, i+1, '_'); j >= 0 {
				buf.WriteString("<i>")
				buf.WriteString(convertInline(string(runes[i+1 : j])))
				buf.WriteString("</i>")
				i = j + 1
				continue
			}
		}
		// [text](url) link
		if runes[i] == '[' {
			if text, url, end := parseLink(runes, i); end >= 0 {
				buf.WriteString(`<a href="`)
				buf.WriteString(htmlEscape(url))
				buf.WriteString(`">`)
				buf.WriteString(convertInline(text))
				buf.WriteString("</a>")
				i = end
				continue
			}
		}

		// Plain character: escape HTML entities
		switch runes[i] {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		default:
			buf.WriteRune(runes[i])
		}
		i++
	}
	return buf.String()
}

// findClosing finds the position of the closing delimiter (2-char) in runes[start:].
// Returns the index of the first char of the closing delimiter, or -1 if not found.
func findClosing(runes []rune, start int, delim string) int {
	d := []rune(delim)
	for i := start; i <= len(runes)-len(d); i++ {
		if runes[i] == d[0] && runes[i+1] == d[1] {
			return i
		}
	}
	return -1
}

// findClosingSingle finds the closing single-char delimiter in runes[start:].
// Returns the index of the closing char, or -1 if not found.
func findClosingSingle(runes []rune, start int, delim rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == delim {
			// Make sure it's not doubled (e.g. ** or __)
			if i+1 < len(runes) && runes[i+1] == delim {
				i++ // skip the pair
				continue
			}
			return i
		}
	}
	return -1
}

// parseLink parses [text](url) starting at runes[start] (which must be '[').
// Returns the text content, url, and end position (exclusive), or ("","", -1).
func parseLink(runes []rune, start int) (string, string, int) {
	// find closing ]
	depth := 0
	textEnd := -1
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == '[' {
			depth++
		} else if runes[i] == ']' {
			if depth == 0 {
				textEnd = i
				break
			}
			depth--
		}
	}
	if textEnd < 0 {
		return "", "", -1
	}
	// expect ( immediately after ]
	if textEnd+1 >= len(runes) || runes[textEnd+1] != '(' {
		return "", "", -1
	}
	// find closing )
	urlEnd := -1
	for i := textEnd + 2; i < len(runes); i++ {
		if runes[i] == ')' {
			urlEnd = i
			break
		}
	}
	if urlEnd < 0 {
		return "", "", -1
	}
	linkText := string(runes[start+1 : textEnd])
	url := string(runes[textEnd+2 : urlEnd])
	return linkText, url, urlEnd + 1
}

// htmlEscape escapes &, <, > to HTML entities.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// maxMessageRunes is Telegram's hard character limit for sendMessage.
const maxMessageRunes = 4096

// chunkText splits text into chunks of at most maxMessageRunes runes,
// breaking only at newline boundaries where possible.
func chunkText(text string) []string {
	runes := []rune(text)
	if len(runes) <= maxMessageRunes {
		return []string{text}
	}

	var chunks []string
	for len(runes) > maxMessageRunes {
		splitAt := -1
		for i := maxMessageRunes - 1; i >= 0; i-- {
			if runes[i] == '\n' {
				splitAt = i
				break
			}
		}
		if splitAt < 0 {
			splitAt = maxMessageRunes // hard break, no newline found
		}
		chunks = append(chunks, string(runes[:splitAt]))
		// Skip leading newlines in the remainder
		runes = runes[splitAt:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
