package main

import (
	"context"
	"log"
	"time"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/jackc/pgx/v5/pgxpool"
)

// startReminderProducer launches a background goroutine that polls the
// reminders table every minute and publishes EventReminder events for any due
// reminders. The agent loop picks them up and delivers them to the recipient.
func startReminderProducer(ctx context.Context, pool *pgxpool.Pool, bus agent.EventBus) {
	go func() {
		log.Printf("reminder producer started")
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		// Fire once immediately on startup to catch anything missed while down.
		fireReminders(ctx, pool, bus)

		for {
			select {
			case <-ctx.Done():
				log.Printf("reminder producer stopped")
				return
			case <-ticker.C:
				fireReminders(ctx, pool, bus)
			}
		}
	}()
}

type dueReminder struct {
	id      int64
	chatID  int64
	message string
}

func fireReminders(ctx context.Context, pool *pgxpool.Pool, bus agent.EventBus) {
	rows, err := pool.Query(ctx,
		`SELECT id, chat_id, message FROM reminders
		 WHERE fire_at <= now() AND fired_at IS NULL
		 ORDER BY fire_at`,
	)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("reminder query: %v", err)
		}
		return
	}

	var due []dueReminder
	for rows.Next() {
		var r dueReminder
		if err := rows.Scan(&r.id, &r.chatID, &r.message); err != nil {
			log.Printf("reminder scan: %v", err)
			continue
		}
		due = append(due, r)
	}
	rows.Close()

	for _, r := range due {
		bus.Publish(agent.AgentEvent{
			Kind:     agent.EventReminder,
			TargetID: r.chatID,
			ChatID:   r.chatID,
			Content:  r.message,
			Source:   "reminder",
			EventID:  generateUUID(),
		})

		// Mark as fired immediately — the bus guarantees delivery.
		if _, err := pool.Exec(ctx,
			`UPDATE reminders SET fired_at = now() WHERE id = $1`, r.id,
		); err != nil {
			log.Printf("reminder mark fired (id=%d): %v", r.id, err)
		} else {
			log.Printf("reminder published: id=%d chat=%d", r.id, r.chatID)
		}
	}
}
