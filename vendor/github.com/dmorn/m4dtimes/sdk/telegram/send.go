package telegram

import (
	"context"
	"log"
)

const maxChunkRunes = 4096

// Button is an inline keyboard button.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Send implements agent.Messenger.
// It converts text from Markdown to Telegram HTML, splits it into ≤4096-rune
// chunks at newline boundaries, and sends each chunk sequentially.
// If Telegram rejects a chunk with an HTML parse error the chunk is retried
// as plain text (parse_mode omitted).
func (c *Client) Send(ctx context.Context, chatID int64, text string) error {
	htmlText := markdownToTelegramHTML(text)
	chunks := splitAtNewlines(htmlText, maxChunkRunes)

	for _, chunk := range chunks {
		if err := c.sendChunk(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendChunk sends a single pre-formatted HTML chunk.
// On an HTML parse error it retries without parse_mode (plain text fallback).
func (c *Client) sendChunk(ctx context.Context, chatID int64, chunk string) error {
	err := c.do(ctx, "sendMessage", map[string]any{
		"chat_id":    chatID,
		"text":       chunk,
		"parse_mode": "HTML",
	}, nil)
	if err == nil {
		return nil
	}
	if !isTelegramHTMLParseError(err) {
		return err
	}

	// HTML parse error: retry as plain text so the message is never silently dropped.
	log.Printf("[telegram] HTML parse error, retrying chunk as plain text (chatID=%d): %v", chatID, err)
	return c.do(ctx, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    chunk,
	}, nil)
}

// splitAtNewlines splits text into chunks of at most maxRunes runes, breaking
// only at newline boundaries. If a single line exceeds maxRunes it is emitted
// as its own (oversized) chunk to avoid losing content.
func splitAtNewlines(text string, maxRunes int) []string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}

	var chunks []string
	start := 0

	for start < len(runes) {
		end := start + maxRunes
		if end >= len(runes) {
			chunks = append(chunks, string(runes[start:]))
			break
		}

		// Find the last newline within [start, end).
		splitAt := -1
		for i := end - 1; i >= start; i-- {
			if runes[i] == '\n' {
				splitAt = i
				break
			}
		}

		if splitAt < 0 {
			// No newline in this window — hard-split to avoid an infinite loop.
			chunks = append(chunks, string(runes[start:end]))
			start = end
		} else {
			// Include the newline in the current chunk.
			chunks = append(chunks, string(runes[start:splitAt+1]))
			start = splitAt + 1
		}
	}

	return chunks
}

// SendTyping sends a "typing" chat action. Telegram shows the indicator for ~5s.
// Implements agent.TypingNotifier.
func (c *Client) SendTyping(ctx context.Context, chatID int64) error {
	return c.do(ctx, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  "typing",
	}, nil)
}

// SendWithButtons sends text with an inline keyboard (single row of buttons).
func (c *Client) SendWithButtons(ctx context.Context, chatID int64, text string, buttons []Button) error {
	return c.do(ctx, "sendMessage", map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
		"reply_markup": map[string]any{
			"inline_keyboard": [][]Button{buttons},
		},
	}, nil)
}
