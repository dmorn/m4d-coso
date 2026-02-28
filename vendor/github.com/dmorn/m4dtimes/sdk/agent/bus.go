package agent

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EventBus is the interface for publishing and consuming AgentEvents.
type EventBus interface {
	Publish(event AgentEvent)
	Subscribe() <-chan AgentEvent
	Close()
}

// ── InMemoryBus ────────────────────────────────────────────────────────────────

// InMemoryBus is a simple buffered-channel event bus for single-process use.
type InMemoryBus struct {
	ch chan AgentEvent
}

// NewInMemoryBus creates an InMemoryBus with a buffer of 256 events.
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{ch: make(chan AgentEvent, 256)}
}

// Publish sends event to the bus without blocking. If the buffer is full the
// event is dropped and a warning is logged.
func (b *InMemoryBus) Publish(event AgentEvent) {
	select {
	case b.ch <- event:
	default:
		log.Printf("agent/bus: channel full — dropping event kind=%s target=%d", event.Kind, event.TargetID)
	}
}

// Subscribe returns the underlying channel. All calls return the same channel.
func (b *InMemoryBus) Subscribe() <-chan AgentEvent {
	return b.ch
}

// Close closes the internal channel, unblocking any receiver.
func (b *InMemoryBus) Close() {
	close(b.ch)
}

// ── PersistentBus ─────────────────────────────────────────────────────────────

// PersistentBus wraps InMemoryBus and persists every event to Postgres so that
// events survive process restarts. Call ReplayUnprocessed on startup to recover
// events that were published but not yet processed before the last crash.
//
// Required table (applied by the consumer, not by PersistentBus itself):
//
//	CREATE TABLE IF NOT EXISTS agent_events (
//	    id               BIGSERIAL PRIMARY KEY,
//	    event_id         UUID NOT NULL UNIQUE,
//	    target_user_id   BIGINT NOT NULL,
//	    chat_id          BIGINT NOT NULL,
//	    kind             TEXT NOT NULL,
//	    content          TEXT NOT NULL,
//	    source           TEXT,
//	    context_snapshot JSONB,
//	    created_at       TIMESTAMPTZ DEFAULT NOW(),
//	    processed_at     TIMESTAMPTZ
//	);
type PersistentBus struct {
	mem  *InMemoryBus
	pool *pgxpool.Pool
}

// NewPersistentBus creates a PersistentBus backed by the given pool.
func NewPersistentBus(pool *pgxpool.Pool) *PersistentBus {
	return &PersistentBus{
		mem:  NewInMemoryBus(),
		pool: pool,
	}
}

// Publish persists the event to Postgres (idempotent on event_id) then forwards
// it to the in-memory bus so the agent loop picks it up immediately.
func (b *PersistentBus) Publish(event AgentEvent) {
	_, err := b.pool.Exec(context.Background(),
		`INSERT INTO agent_events (event_id, target_user_id, chat_id, kind, content, source)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.TargetID, event.ChatID,
		string(event.Kind), event.Content, event.Source,
	)
	if err != nil {
		log.Printf("agent/bus: persist event %s: %v", event.EventID, err)
	}
	b.mem.Publish(event)
}

// ReplayUnprocessed fetches all rows where processed_at IS NULL (ordered by
// created_at) and republishes them to the in-memory bus. Call this once on
// startup after the table exists.
func (b *PersistentBus) ReplayUnprocessed(ctx context.Context) error {
	rows, err := b.pool.Query(ctx,
		`SELECT event_id, target_user_id, chat_id, kind, content, COALESCE(source, '')
		 FROM agent_events
		 WHERE processed_at IS NULL
		 ORDER BY created_at`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var ev AgentEvent
		var kind string
		if err := rows.Scan(&ev.EventID, &ev.TargetID, &ev.ChatID, &kind, &ev.Content, &ev.Source); err != nil {
			return err
		}
		ev.Kind = EventKind(kind)
		b.mem.Publish(ev)
		count++
	}
	if count > 0 {
		log.Printf("agent/bus: replayed %d unprocessed event(s)", count)
	}
	return nil
}

// MarkProcessed stamps processed_at on the given event so it won't be replayed
// after a restart. Call this after the LLM turn for that event completes.
func (b *PersistentBus) MarkProcessed(ctx context.Context, eventID string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE agent_events SET processed_at = NOW() WHERE event_id = $1`,
		eventID,
	)
	return err
}

// Subscribe delegates to the inner InMemoryBus.
func (b *PersistentBus) Subscribe() <-chan AgentEvent {
	return b.mem.Subscribe()
}

// Close delegates to the inner InMemoryBus.
func (b *PersistentBus) Close() {
	b.mem.Close()
}
