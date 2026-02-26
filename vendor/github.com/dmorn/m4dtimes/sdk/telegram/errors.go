package telegram

import "strings"

// isTelegramHTMLParseError reports whether err is a Telegram "can't parse
// entities" error, which happens when the HTML payload contains malformed
// markup (e.g. unescaped < > from generic types in code blocks).
func isTelegramHTMLParseError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "can't parse entities") ||
		strings.Contains(msg, "Bad Request: can't parse")
}
