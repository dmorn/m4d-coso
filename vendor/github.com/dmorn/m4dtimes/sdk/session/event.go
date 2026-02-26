// Package session provides append-only JSONL session recording, compatible
// with the Pi/OpenClaw session format. Each user gets an isolated JSONL file;
// every LLM turn — user message, assistant reply, tool calls, tool results —
// is written as an Event node with a parentId chain for full replay.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/dmorn/m4dtimes/sdk/llm"
)

const Version = 1

// Event is a single append-only node in a session transcript.
// The parentId chain forms a linked list of turns (linear for single-user sessions).
type Event struct {
	Type      string       `json:"type"`
	Version   int          `json:"version,omitempty"` // only on session init
	ID        string       `json:"id"`
	ParentID  string       `json:"parentId,omitempty"`
	Timestamp time.Time    `json:"timestamp"`
	UserID    int64        `json:"userId,omitempty"` // only on session init
	Message   *llm.Message `json:"message,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// sessionInitEvent returns the first event written to a new JSONL file.
func sessionInitEvent(userID int64) Event {
	return Event{
		Type:      "session",
		Version:   Version,
		ID:        newID(),
		Timestamp: time.Now().UTC(),
		UserID:    userID,
	}
}

// messageEvent wraps a llm.Message as a recordable event.
func messageEvent(msg llm.Message, parentID string) Event {
	return Event{
		Type:      "message",
		ID:        newID(),
		ParentID:  parentID,
		Timestamp: time.Now().UTC(),
		Message:   &msg,
	}
}

// newID returns an 8-hex-byte random ID (matches Pi's short ID style).
func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
