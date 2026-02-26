package agent

import (
	"encoding/json"
	"os"
	"time"
)

type Logger struct {
	level string // "debug", "info", "error"
}

func NewLogger(level string) *Logger {
	if level == "" {
		level = "info"
	}
	return &Logger{level: level}
}

func (l *Logger) emit(event string, fields map[string]any) {
	payload := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"event": event,
	}
	for k, v := range fields {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func (l *Logger) Inbound(userID, chatID int64, text string) {
	l.emit("inbound", map[string]any{"user_id": userID, "chat_id": chatID, "text": text})
}

func (l *Logger) LLMCall(model string, tokensIn, tokensOut int, durationMs int64) {
	l.emit("llm_call", map[string]any{"model": model, "tokens_in": tokensIn, "tokens_out": tokensOut, "duration_ms": durationMs})
}

func (l *Logger) ToolExec(tool string, durationMs int64, success bool, errMsg string) {
	l.emit("tool_exec", map[string]any{"tool": tool, "duration_ms": durationMs, "success": success, "error": errMsg})
}

func (l *Logger) Outbound(chatID int64, text string) {
	l.emit("outbound", map[string]any{"chat_id": chatID, "text": text})
}

func (l *Logger) Error(context string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	l.emit("error", map[string]any{"context": context, "error": msg})
}
