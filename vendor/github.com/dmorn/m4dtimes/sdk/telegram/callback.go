package telegram

import "context"

// AnswerCallback acknowledges a button press (removes the loading spinner).
// Call this after receiving a callback_query update.
// text: optional notification text shown to user (empty = silent ack)
func (c *Client) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	payload := map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}
	return c.do(ctx, "answerCallbackQuery", payload, nil)
}
