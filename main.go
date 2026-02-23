// m4d-coso: Hotel Cimon agent, m4dtimes SDK + pgx.
//
// NOTE: this builds for native linux/arm64 (not wasip1).
// pgx requires real TCP sockets. The wasip1 migration path is the host bridge
// pattern (go:wasmimport "host" "fetch" → PostgREST). See m4dtimes/sdk/README.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/dmorn/m4dtimes/sdk/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

const systemPrompt = `You are a hotel management assistant for %s.
You help staff manage rooms and reservations.
Always respond in the same language as the user.
Be concise and practical.`

func main() {
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	dbURL := envOr("DATABASE_URL", "postgresql://postgres:devpassword@localhost:5432/m4dtimes")
	hotelName := envOr("HOTEL_NAME", "Hotel Cimon")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DB
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	log.Printf("connected to postgres: %s", dbURL)

	// Ensure schema
	if err := ensureSchema(ctx, pool); err != nil {
		log.Fatalf("schema: %v", err)
	}

	// Tools
	tools := newHotelTools(pool)

	// Registry
	registry := agent.NewToolRegistry()
	registry.RegisterToolSet(tools)

	// LLM (reads LLM_API_KEY from env)
	provider, err := llm.NewAnthropicProvider(nil)
	if err != nil {
		log.Fatalf("llm provider: %v", err)
	}

	// Agent
	a := agent.New(agent.Options{
		LLM: llm.New(provider, llm.Options{Model: "claude-sonnet-4-5-20250514"}),
		Messenger: telegram.New(botToken),
		Registry:  registry,
		Prompt:    fmt.Sprintf(systemPrompt, hotelName),
		Logger:    agent.NewLogger("info"),
	})

	log.Printf("starting %s agent...", hotelName)
	if err := a.Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env: %s", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
