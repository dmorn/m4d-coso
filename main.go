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
	"strings"
	"syscall"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/dmorn/m4dtimes/sdk/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	botName := envOr("BOT_NAME", "cimon_hotel_bot")
	dbURL := envOr("DATABASE_URL", "postgresql://postgres:devpassword@localhost:5432/m4dtimes")
	hotelName := envOr("HOTEL_NAME", "Hotel Cimon")
	llmModel := envOr("LLM_MODEL", "claude-3-5-sonnet-20241022")
	adminTelegramID := int64(7756297856) // Dani

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Superuser pool — DDL and invite management only
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

	// Bootstrap admin/manager on first run
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
	toolRegistry.RegisterToolSet(newHotelTools(registry, botName, botToken, adminPool))

	a := agent.New(agent.Options{
		LLM:       llm.New(provider, llm.Options{Model: llmModel}),
		Messenger: telegram.New(botToken),
		Registry:  toolRegistry,
		Logger:    agent.NewLogger("info"),

		// HandleStart — deep-link invite redemption via /start <token>.
		// Runs BEFORE Authorize so unregistered users can onboard themselves.
		HandleStart: func(hCtx context.Context, userID, chatID int64, payload string) (string, error) {
			token := strings.TrimSpace(payload)
			if token == "" {
				// Bare /start with no token — fall through to Authorize
				return "", nil
			}

			info, err := registry.UseInvite(hCtx, token, userID)
			if err != nil {
				log.Printf("invite redemption failed for user %d token %s: %v", userID, token, err)
				return "❌ Il link di invito non è valido o è scaduto. Chiedi un nuovo link all'amministratore.", nil
			}

			roleLabel := map[Role]string{
				RoleManager: "manager",
				RoleCleaner: "addetto/a alle pulizie",
			}[info.Role]

			return fmt.Sprintf(
				"✅ Benvenuto/a, %s! Sei stato registrato come %s. Puoi iniziare a usare il bot. 🏨",
				info.Name, roleLabel,
			), nil
		},

		// Authorize — gate every inbound message; rejects unregistered users
		// before the LLM is ever called (zero tokens consumed for strangers).
		Authorize: func(aCtx context.Context, userID, chatID int64) (string, error) {
			if registry.IsRegistered(aCtx, userID) {
				return "", nil
			}
			return "Ciao! Non sei ancora registrato. Chiedi un link di invito all'amministratore. 🔒", nil
		},

		BuildExtra: func(userID, _ int64) (any, error) {
			pool, err := registry.Pool(ctx, userID)
			if err != nil {
				return nil, fmt.Errorf("user %d: %w", userID, err)
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

	startReminderLoop(ctx, adminPool, botToken)

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
