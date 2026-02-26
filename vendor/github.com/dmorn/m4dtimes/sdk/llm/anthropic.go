package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const anthropicURL = "https://api.anthropic.com/v1/messages"
const anthropicVersion = "2023-06-01"

type AnthropicProvider struct {
	apiKey     string
	url        string
	httpClient *http.Client
	retry      RetryConfig
}

func NewAnthropicProvider(httpClient *http.Client) (*AnthropicProvider, error) {
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}
	if apiKey == "" {
		return nil, errors.New("missing API key: set LLM_API_KEY or ANTHROPIC_API_KEY")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AnthropicProvider{
		apiKey:     apiKey,
		url:        anthropicURL,
		httpClient: httpClient,
		retry:      DefaultRetryConfig,
	}, nil
}

// isOAuthToken returns true if the key is an Anthropic OAuth access token
// (sk-ant-oat* prefix). These require Bearer auth + specific beta headers
// instead of the standard x-api-key header.
func isOAuthToken(key string) bool {
	return strings.HasPrefix(key, "sk-ant-oat")
}

func (p *AnthropicProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	wireReq, err := toAnthropicRequest(req)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	resp, err := doWithRetry(ctx, p.retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		if isOAuthToken(p.apiKey) {
			// OAuth tokens require Bearer auth + oauth beta header
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
			httpReq.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
		} else {
			httpReq.Header.Set("x-api-key", p.apiKey)
		}
		return p.httpClient.Do(httpReq)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read anthropic response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("anthropic API error %d: %s", resp.StatusCode, string(respBody))
	}

	var wireResp anthropicResponse
	if err := json.Unmarshal(respBody, &wireResp); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	return fromAnthropicResponse(wireResp)
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicContentItem `json:"content"`
}

type anthropicContentItem struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicResponse struct {
	Content    []anthropicContentItem `json:"content"`
	StopReason string                 `json:"stop_reason"`
	Usage      anthropicUsage         `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func toAnthropicRequest(req Request) (anthropicRequest, error) {
	out := anthropicRequest{
		Model:     req.Options.Model,
		MaxTokens: req.Options.MaxTokens,
		System:    req.System,
	}
	for _, m := range req.Messages {
		wm := anthropicMessage{Role: m.Role}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				wm.Content = append(wm.Content, anthropicContentItem{Type: "text", Text: c.Text})
			case "tool_use":
				if c.ToolCall == nil {
					return anthropicRequest{}, errors.New("tool_use block missing tool_call")
				}
				wm.Content = append(wm.Content, anthropicContentItem{
					Type:  "tool_use",
					ID:    c.ToolCall.ID,
					Name:  c.ToolCall.Name,
					Input: c.ToolCall.Arguments,
				})
			case "tool_result":
				if c.ToolResult == nil {
					return anthropicRequest{}, errors.New("tool_result block missing tool_result")
				}
				wm.Role = "user" // Anthropic quirk
				wm.Content = append(wm.Content, anthropicContentItem{
					Type:      "tool_result",
					ToolUseID: c.ToolResult.ToolCallID,
					Content:   c.ToolResult.Content,
					IsError:   c.ToolResult.IsError,
				})
			default:
				return anthropicRequest{}, fmt.Errorf("unsupported content block type: %q", c.Type)
			}
		}
		out.Messages = append(out.Messages, wm)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	return out, nil
}

func fromAnthropicResponse(in anthropicResponse) (*Response, error) {
	resp := &Response{
		StopReason: in.StopReason,
		Usage: Usage{
			InputTokens:  in.Usage.InputTokens,
			OutputTokens: in.Usage.OutputTokens,
		},
	}

	switch in.StopReason {
	case "tool_use":
		resp.Type = "tool_use"
	case "end_turn":
		resp.Type = "text"
	default:
		resp.Type = "text"
	}

	for _, c := range in.Content {
		switch c.Type {
		case "text":
			if resp.Text == "" {
				resp.Text = c.Text
			} else {
				resp.Text += c.Text
			}
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        c.ID,
				Name:      c.Name,
				Arguments: c.Input,
			})
		}
	}

	if resp.Type == "tool_use" && len(resp.ToolCalls) == 0 {
		return nil, errors.New("anthropic response stop_reason=tool_use but no tool calls")
	}

	return resp, nil
}
