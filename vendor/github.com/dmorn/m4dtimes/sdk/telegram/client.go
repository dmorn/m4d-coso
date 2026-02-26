package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"github.com/dmorn/m4dtimes/sdk/agent"
	"net/http"
	"time"
)

const baseURL = "https://api.telegram.org/bot%s/%s"

type Client struct {
	token      string
	httpClient *http.Client
}

func New(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

var _ agent.Messenger = (*Client)(nil)

// do sends a Telegram Bot API request.
// method: e.g. "getUpdates", "sendMessage"
// payload: JSON-serializable params (or nil)
// result: pointer to struct to decode into (or nil to ignore)
func (c *Client) do(ctx context.Context, method string, payload any, result any) error {
	url := fmt.Sprintf(baseURL, c.token, method)

	var req *http.Request
	var err error
	if payload == nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build telegram request: %w", err)
		}
	} else {
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal telegram request: %w", err)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build telegram request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s request failed: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}

	if !envelope.OK {
		if envelope.Description == "" {
			envelope.Description = "unknown error"
		}
		return fmt.Errorf("telegram %s API error: %s", method, envelope.Description)
	}

	if result != nil && envelope.Result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode telegram result for %s: %w", method, err)
		}
	}

	return nil
}
