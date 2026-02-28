package agent

type EventKind string

const (
	EventUserMessage EventKind = "user_message"
	EventRelay       EventKind = "relay"
	EventHeartbeat   EventKind = "heartbeat"
	EventReminder    EventKind = "reminder"
)

type AgentEvent struct {
	Kind     EventKind
	TargetID int64  // which user context to run the LLM turn for
	ChatID   int64  // where to send the LLM response
	Content  string // synthesized as the incoming "user message"
	Source   string // human-readable sender: "Berni", "system", etc.
	EventID  string // UUID for idempotency
}
