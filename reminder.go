package main

import (
	"context"
	"log"
	"time"

	"github.com/dmorn/m4dtimes/sdk/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

// startReminderLoop launches a background goroutine that polls the reminders
// table every minute and fires any due reminders via Telegram.
// It exits when ctx is cancelled.
func startReminderLoop(ctx context.Context, pool *pgxpool.Pool, botToken string) {
	tg := telegram.New(botToken)
	go func() {
		log.Printf("reminder loop started")
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		// Fire once immediately on startup to catch anything missed while down.
		fireReminders(ctx, pool, tg)

		for {
			select {
			case <-ctx.Done():
				log.Printf("reminder loop stopped")
				return
			case <-ticker.C:
				fireReminders(ctx, pool, tg)
			}
		}
	}()
}

type dueReminder struct {
	id      int64
	chatID  int64
	message string
}

func fireReminders(ctx context.Context, pool *pgxpool.Pool, tg *telegram.Client) {
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
		if err := tg.Send(ctx, r.chatID, r.message); err != nil {
			log.Printf("reminder send (id=%d chat=%d): %v", r.id, r.chatID, err)
			// Don't mark as fired — retry next tick.
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE reminders SET fired_at = now() WHERE id = $1`, r.id,
		); err != nil {
			log.Printf("reminder mark fired (id=%d): %v", r.id, err)
		} else {
			log.Printf("reminder fired: id=%d chat=%d", r.id, r.chatID)
		}
	}
}
