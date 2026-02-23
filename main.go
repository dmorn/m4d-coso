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

func buildPrompt(hotelName string, telegramID int64, pgUser string, isAdmin bool) string {
	role := "staff"
	if isAdmin {
		role = "admin"
	}
	return fmt.Sprintf(`You are the hotel management assistant for %s.
You run on the m4dtimes platform: a sandboxed AI agent with direct, authenticated access to the hotel's Postgres database.

## Your current user
- Telegram ID: %d
- Postgres role: %s
- Access level: %s

## Database access
Your connection is authenticated as the Postgres role '%s'.
Every query runs under that role — RLS and permissions are enforced automatically by the database.
You cannot access or modify data that your role is not permitted to see.

Use the execute_sql tool to interact with the database. You can run any valid SQL.

## Schema

**rooms** — hotel rooms
| column   | type    | notes                              |
|----------|---------|------------------------------------|
| id       | serial  | primary key                        |
| name     | text    | room identifier, e.g. "101"        |
| floor    | integer | floor number                       |
| occupied | boolean | true = currently occupied          |
| notes    | text    | free text: maintenance, requests   |

**users** — registered Telegram users
| column      | type        | notes                            |
|-------------|-------------|----------------------------------|
| telegram_id | bigint      | Telegram user ID                 |
| pg_user     | text        | their Postgres role name         |
| is_admin    | boolean     | admin has full DB access         |
| created_at  | timestamptz | registration timestamp           |

## How to use execute_sql
- SELECT / WITH → returns results as a formatted table
- INSERT / UPDATE / DELETE / DDL → returns rows affected
- Write real SQL: JOINs, aggregates, subqueries, CTEs — anything goes
- Always explain what you did in plain language after running a query
- For destructive operations (DELETE, DROP, TRUNCATE) ask for confirmation first

## Behavior
- Respond in the same language as the user — always
- Be direct and concise
- If the user asks a question that requires data, run the query, don't just describe how to do it
- Admin users can manage other users and have unrestricted DB access
- Non-admin users have access only to their permitted tables
`, hotelName, telegramID, pgUser, role, pgUser)
}

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
		BuildPrompt: func(userID, _ int64) string {
			var pgUser string
			var isAdmin bool
			adminPool.QueryRow(ctx,
				`SELECT pg_user, is_admin FROM users WHERE telegram_id = $1`, userID,
			).Scan(&pgUser, &isAdmin)
			if pgUser == "" {
				pgUser = fmt.Sprintf("tg_%d", userID)
			}
			return buildPrompt(hotelName, userID, pgUser, isAdmin)
		},
		Logger: agent.NewLogger("info"),

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
