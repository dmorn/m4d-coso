// m4d-coso: Hotel Cimon agent, m4dtimes SDK + pgx.
//
// Each Telegram user maps to a dedicated Postgres role with its own credentials.
// The agent opens a per-user connection pool and runs all queries under that role,
// so RLS and CURRENT_USER-based policies apply automatically.
//
// NOTE: native linux/arm64 build. pgx requires real TCP sockets not available in wasip1.
// Future path: host bridge (go:wasmimport "host" "fetch" → PostgREST). See m4dtimes/sdk/README.
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
	adminTelegramID := int64(7756297856) // Dani

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Admin pool (superuser — only for DDL and user management)
	adminPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer adminPool.Close()

	if err := adminPool.Ping(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	log.Printf("connected to postgres: %s", dbURL)

	// Schema
	if err := ensureSchema(ctx, adminPool); err != nil {
		log.Fatalf("schema: %v", err)
	}

	// User registry
	registry := newUserRegistry(adminPool, dbURL)

	// Bootstrap admin if not registered
	if !registry.IsRegistered(ctx, adminTelegramID) {
		log.Printf("bootstrapping admin user %d...", adminTelegramID)
		if err := registry.Register(ctx, adminTelegramID, true); err != nil {
			log.Fatalf("register admin: %v", err)
		}
	}

	// LLM (reads LLM_API_KEY from env)
	provider, err := llm.NewAnthropicProvider(nil)
	if err != nil {
		log.Fatalf("llm provider: %v", err)
	}

	// Tool registry
	toolRegistry := agent.NewToolRegistry()
	toolRegistry.RegisterToolSet(newHotelTools())

	// Agent
	a := agent.New(agent.Options{
		LLM:       llm.New(provider, llm.Options{Model: "claude-sonnet-4-5-20250514"}),
		Messenger: telegram.New(botToken),
		Registry:  toolRegistry,
		Prompt:    fmt.Sprintf(systemPrompt, hotelName),
		Logger:    agent.NewLogger("info"),

		// Inject per-user DB pool into ToolContext.Extra
		BuildExtra: func(userID, chatID int64) (any, error) {
			pool, err := registry.Pool(ctx, userID)
			if err != nil {
				// Auto-register unknown users as non-admin
				log.Printf("user %d not found, registering...", userID)
				if regErr := registry.Register(ctx, userID, false); regErr != nil {
					return nil, fmt.Errorf("register user %d: %w", userID, regErr)
				}
				pool, err = registry.Pool(ctx, userID)
				if err != nil {
					return nil, err
				}
			}
			return pool, nil
		},
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
