# sdk/telegram

Minimal Telegram Bot API client. Polling-based, no webhooks, no frameworks.

## Operations

| Method | Telegram API | Description |
|--------|-------------|-------------|
| `Poll()` | getUpdates | Long-poll for new messages (30s timeout) |
| `Send()` | sendMessage | Send text message |
| `SendWithButtons()` | sendMessage + inline_keyboard | Send with inline buttons |
| `AnswerCallback()` | answerCallbackQuery | Acknowledge button press |

## Why polling (not webhooks)

- No inbound port needed → simpler WASI config, no TLS cert for the bot
- Works behind NAT/Tailscale without port forwarding
- One long HTTP connection, efficient enough for our scale
- Simpler error handling (just retry the poll)

## Status

🔴 **Not started** — interface defined in sdk/README.md.
