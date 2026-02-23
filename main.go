// m4d-coso: Hotel Cimon management agent — m4dtimes SDK + pgx.
//
// Each Telegram user maps to a dedicated Postgres role with its own credentials.
// RLS policies on all tables enforce what each role can see and modify.
// Managers have full access; cleaners can read everything but only update their own assignments.
//
// NOTE: native linux/arm64 build. pgx requires real TCP sockets not available in wasip1.
// Future: host bridge (go:wasmimport "host" "fetch" → PostgREST). See m4dtimes/sdk/README.
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

func main() {
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	dbURL := envOr("DATABASE_URL", "postgresql://postgres:devpassword@localhost:5432/m4dtimes")
	hotelName := envOr("HOTEL_NAME", "Hotel Cimon")
	adminTelegramID := int64(7756297856) // Dani

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Superuser pool — DDL only
	adminPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer adminPool.Close()

	if err := adminPool.Ping(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	log.Printf("connected to postgres: %s", dbURL)

	if err := ensureSchema(ctx, adminPool); err != nil {
		log.Fatalf("schema: %v", err)
	}

	registry := newUserRegistry(adminPool, dbURL)

	// Bootstrap admin/manager
	if !registry.IsRegistered(ctx, adminTelegramID) {
		log.Printf("bootstrapping manager %d...", adminTelegramID)
		if err := registry.Register(ctx, adminTelegramID, RoleManager, "Dani"); err != nil {
			log.Fatalf("register manager: %v", err)
		}
	}

	provider, err := llm.NewAnthropicProvider(nil)
	if err != nil {
		log.Fatalf("llm provider: %v", err)
	}

	toolRegistry := agent.NewToolRegistry()
	toolRegistry.RegisterToolSet(newHotelTools())

	a := agent.New(agent.Options{
		LLM:       llm.New(provider, llm.Options{Model: "claude-sonnet-4-5-20250514"}),
		Messenger: telegram.New(botToken),
		Registry:  toolRegistry,
		Logger:    agent.NewLogger("info"),

		BuildExtra: func(userID, _ int64) (any, error) {
			pool, err := registry.Pool(ctx, userID)
			if err != nil {
				// Auto-register unknown users as cleaners
				log.Printf("unknown user %d, registering as cleaner...", userID)
				if regErr := registry.Register(ctx, userID, RoleCleaner, ""); regErr != nil {
					return nil, fmt.Errorf("register user %d: %w", userID, regErr)
				}
				pool, err = registry.Pool(ctx, userID)
				if err != nil {
					return nil, err
				}
			}
			return pool, nil
		},

		BuildPrompt: func(userID, _ int64) string {
			var pgUser, name, roleStr string
			adminPool.QueryRow(ctx,
				`SELECT pg_user, COALESCE(name,''), role FROM users WHERE telegram_id = $1`, userID,
			).Scan(&pgUser, &name, &roleStr)
			if pgUser == "" {
				pgUser = fmt.Sprintf("tg_%d", userID)
			}
			role := Role(roleStr)
			if role == "" {
				role = RoleCleaner
			}
			return buildPrompt(hotelName, userID, pgUser, role, name)
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
