package telegram

import (
	"context"
	"github.com/dmorn/m4dtimes/sdk/agent"
)

// TelegramUpdate is the raw Telegram update structure.
type TelegramUpdate struct {
	UpdateID      int64          `json:"update_id"`
	Message       *TelegramMsg   `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type TelegramMsg struct {
	MessageID int64         `json:"message_id"`
	From      *TelegramUser `json:"from,omitempty"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text,omitempty"`
	Date      int64         `json:"date"`
}

type TelegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type TelegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type CallbackQuery struct {
	ID      string       `json:"id"`
	From    TelegramUser `json:"from"`
	Message *TelegramMsg `json:"message,omitempty"`
	Data    string       `json:"data,omitempty"`
}

// Poll implements agent.Messenger.
// Uses getUpdates with long polling (timeout=timeoutSec).
// Returns only updates with a text message or callback query.
// Converts Telegram updates to agent.Update.
func (c *Client) Poll(ctx context.Context, offset int64, timeoutSec int) ([]agent.Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "callback_query"},
	}

	var raw []TelegramUpdate
	if err := c.do(ctx, "getUpdates", payload, &raw); err != nil {
		return nil, err
	}

	updates := make([]agent.Update, 0, len(raw))
	for _, u := range raw {
		if u.Message != nil {
			if u.Message.From == nil || u.Message.Text == "" {
				continue
			}
			updates = append(updates, agent.Update{
				UpdateID: u.UpdateID,
				UserID:   u.Message.From.ID,
				ChatID:   u.Message.Chat.ID,
				Text:     u.Message.Text,
			})
			continue
		}

		if u.CallbackQuery != nil {
			if u.CallbackQuery.Data == "" || u.CallbackQuery.Message == nil {
				continue
			}
			updates = append(updates, agent.Update{
				UpdateID: u.UpdateID,
				UserID:   u.CallbackQuery.From.ID,
				ChatID:   u.CallbackQuery.Message.Chat.ID,
				Text:     u.CallbackQuery.Data,
			})
		}
	}

	return updates, nil
}
