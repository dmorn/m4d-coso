package llm

import "context"

type Provider interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

type Request struct {
	System   string
	Messages []Message
	Tools    []ToolDef
	Options  Options
}

type Options struct {
	Model     string
	MaxTokens int
}

// Client wraps a provider with a default model.
type Client struct {
	provider Provider
	opts     Options
}

func New(provider Provider, opts Options) *Client {
	return &Client{provider: provider, opts: opts}
}

const defaultMaxTokens = 4096

func (c *Client) Chat(ctx context.Context, req Request) (*Response, error) {
	if req.Options.Model == "" {
		req.Options.Model = c.opts.Model
	}
	if req.Options.MaxTokens == 0 {
		req.Options.MaxTokens = c.opts.MaxTokens
	}
	if req.Options.MaxTokens == 0 {
		req.Options.MaxTokens = defaultMaxTokens
	}
	return c.provider.Chat(ctx, req)
}
