package main

import (
	"log"
	"strconv"
	"strings"
	"time"

	"context"

	"github.com/dmorn/m4dtimes/sdk/agent"
)

// startHeartbeatProducer launches a background goroutine that publishes
// EventHeartbeat events on a configurable schedule. The agent loop picks them
// up and runs the LLM turn, so the producer itself has no LLM dependency.
//
// Configure via env (mutually exclusive, HEARTBEAT_TIME takes precedence):
//
//	HEARTBEAT_TIME=17:00              fire daily at this time (Europe/Rome)
//	HEARTBEAT_INTERVAL_MINUTES=60    fire every N minutes (default; set to 0 to disable)
func startHeartbeatProducer(ctx context.Context, bus agent.EventBus, managerID int64) {
	loc, _ := time.LoadLocation("Europe/Rome")

	heartbeatContent := "🕐 Heartbeat check. Check the database for upcoming checkouts, check-ins, stale assignments, and any issues in the next 24 hours. Use execute_sql to investigate. If you find issues, use send_user_message to notify me with a summary. If everything looks fine, just reply OK."

	publish := func() {
		bus.Publish(agent.AgentEvent{
			Kind:     agent.EventHeartbeat,
			TargetID: managerID,
			ChatID:   managerID,
			Content:  heartbeatContent,
			Source:   "system",
			EventID:  generateUUID(),
		})
		log.Printf("heartbeat: event published for manager %d", managerID)
	}

	// HEARTBEAT_TIME=HH:MM → daily fire at exact time
	if timeStr := envOr("HEARTBEAT_TIME", ""); timeStr != "" {
		parts := strings.SplitN(timeStr, ":", 2)
		if len(parts) != 2 {
			log.Printf("heartbeat: invalid HEARTBEAT_TIME=%q (expected HH:MM), disabling", timeStr)
			return
		}
		hour, errH := strconv.Atoi(parts[0])
		min, errM := strconv.Atoi(parts[1])
		if errH != nil || errM != nil || hour < 0 || hour > 23 || min < 0 || min > 59 {
			log.Printf("heartbeat: invalid HEARTBEAT_TIME=%q, disabling", timeStr)
			return
		}
		log.Printf("heartbeat: daily mode, fires at %02d:%02d Europe/Rome for manager %d", hour, min, managerID)
		go func() {
			for {
				now := time.Now().In(loc)
				next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
				if !next.After(now) {
					next = next.Add(24 * time.Hour)
				}
				delay := time.Until(next)
				log.Printf("heartbeat: next run in %v (at %s)", delay.Round(time.Second), next.Format("2006-01-02 15:04 MST"))
				select {
				case <-ctx.Done():
					log.Printf("heartbeat: stopped")
					return
				case <-time.After(delay):
				}
				publish()
			}
		}()
		return
	}

	// Fallback: interval mode (legacy behaviour)
	intervalStr := envOr("HEARTBEAT_INTERVAL_MINUTES", "60")
	minutes, err := strconv.Atoi(intervalStr)
	if err != nil || minutes <= 0 {
		log.Printf("heartbeat: disabled (HEARTBEAT_INTERVAL_MINUTES=%q)", intervalStr)
		return
	}
	interval := time.Duration(minutes) * time.Minute
	log.Printf("heartbeat: interval mode, every %v for manager %d", interval, managerID)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("heartbeat: stopped")
				return
			case <-ticker.C:
				publish()
			}
		}
	}()
}
