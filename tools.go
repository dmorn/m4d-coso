package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/dmorn/m4dtimes/sdk/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HotelTools struct {
	registry  *UserRegistry
	botName   string // e.g. "cimon_hotel_bot"
	botToken  string // Telegram bot token for outbound messages
	adminPool *pgxpool.Pool
}

func newHotelTools(registry *UserRegistry, botName, botToken string, adminPool *pgxpool.Pool) *HotelTools {
	return &HotelTools{registry: registry, botName: botName, botToken: botToken, adminPool: adminPool}
}

func (h *HotelTools) Tools() []agent.Tool {
	return []agent.Tool{
		&executeSQLTool{},
		&generateInviteTool{registry: h.registry, botName: h.botName},
		&sendUserMessageTool{adminPool: h.adminPool, botToken: h.botToken},
	}
}

func poolFrom(ctx agent.ToolContext) (*pgxpool.Pool, error) {
	pool, ok := ctx.Extra.(*pgxpool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("no db pool in context")
	}
	return pool, nil
}

// ── generate_invite ──────────────────────────────────────────────────────────

type generateInviteTool struct {
	registry *UserRegistry
	botName  string
}

func (t *generateInviteTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "generate_invite",
		Description: "Genera un link di invito per un nuovo utente. Solo i manager possono usare questo tool. Restituisce un link Telegram da condividere con la persona.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "Nome della persona da invitare"
				},
				"role": {
					"type": "string",
					"enum": ["cleaner", "manager"],
					"description": "Ruolo da assegnare: 'cleaner' per le cameriere, 'manager' per i responsabili"
				}
			},
			"required": ["name", "role"]
		}`),
	}
}

func (t *generateInviteTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.Name == "" || in.Role == "" {
		return "", fmt.Errorf("name and role are required")
	}

	role := Role(in.Role)
	if role != RoleManager && role != RoleCleaner {
		return "", fmt.Errorf("invalid role: %s", in.Role)
	}

	token, err := t.registry.CreateInvite(context.Background(), ctx.UserID, role, in.Name)
	if err != nil {
		return "", fmt.Errorf("create invite: %w", err)
	}

	link := fmt.Sprintf("https://t.me/%s?start=%s", t.botName, token)
	return fmt.Sprintf(
		"✅ Invito creato per %s (%s):\n%s\n\n⚠️ Il link scade tra 7 giorni ed è monouso.",
		in.Name, in.Role, link,
	), nil
}

// ── execute_sql ──────────────────────────────────────────────────────────────

type executeSQLTool struct{}

func (t *executeSQLTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "execute_sql",
		Description: "Execute an arbitrary SQL query against the database. Returns rows as text for SELECT, or affected row count for INSERT/UPDATE/DELETE.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "The SQL query to execute"}
			},
			"required": ["query"]
		}`),
	}
}

func (t *executeSQLTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}

	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}

	q := strings.TrimSpace(in.Query)
	if q == "" {
		return "", fmt.Errorf("empty query")
	}

	// SELECT → return rows
	upper := strings.ToUpper(q)
	if strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") {
		rows, err := db.Query(context.Background(), q)
		if err != nil {
			return "", fmt.Errorf("query: %w", err)
		}
		defer rows.Close()

		fields := rows.FieldDescriptions()
		headers := make([]string, len(fields))
		for i, f := range fields {
			headers[i] = string(f.Name)
		}

		var sb strings.Builder
		sb.WriteString(strings.Join(headers, " | "))
		sb.WriteString("\n" + strings.Repeat("-", 40) + "\n")

		count := 0
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return "", err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%v", v)
			}
			sb.WriteString(strings.Join(parts, " | ") + "\n")
			count++
		}
		if count == 0 {
			sb.WriteString("(no rows)\n")
		}
		return sb.String(), nil
	}

	// INSERT / UPDATE / DELETE / DDL → exec
	tag, err := db.Exec(context.Background(), q)
	if err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	return fmt.Sprintf("OK — %d rows affected", tag.RowsAffected()), nil
}

// ── send_user_message ────────────────────────────────────────────────────────

type sendUserMessageTool struct {
	adminPool *pgxpool.Pool
	botToken  string
}

func (t *sendUserMessageTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "send_user_message",
		Description: "Invia un messaggio Telegram a uno o più utenti registrati. " +
			"Puoi specificare un nome utente specifico oppure un ruolo ('manager' o 'cleaner') per inviare a tutti gli utenti con quel ruolo. " +
			"Usa 'all' come destinatario per inviare a tutti gli utenti registrati.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {
					"type": "string",
					"description": "Nome dell'utente (es. 'Mario'), ruolo ('manager' o 'cleaner'), oppure 'all' per tutti"
				},
				"message": {
					"type": "string",
					"description": "Il testo del messaggio da inviare"
				}
			},
			"required": ["to", "message"]
		}`),
	}
}

func (t *sendUserMessageTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	var in struct {
		To      string `json:"to"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.To == "" || in.Message == "" {
		return "", fmt.Errorf("to and message are required")
	}

	// Resolve recipients from the DB
	type recipient struct {
		telegramID int64
		name       string
	}
	var recipients []recipient

	bg := context.Background()
	to := strings.ToLower(strings.TrimSpace(in.To))

	var query string
	var queryArgs []any

	switch to {
	case "all":
		query = `SELECT telegram_id, COALESCE(name, '') FROM users`
	case "manager", "cleaner":
		query = `SELECT telegram_id, COALESCE(name, '') FROM users WHERE role = $1`
		queryArgs = []any{to}
	default:
		// Match by name (case-insensitive)
		query = `SELECT telegram_id, COALESCE(name, '') FROM users WHERE lower(name) = lower($1)`
		queryArgs = []any{in.To}
	}

	dbRows, err := t.adminPool.Query(bg, query, queryArgs...)
	if err != nil {
		return "", fmt.Errorf("query recipients: %w", err)
	}
	defer dbRows.Close()

	for dbRows.Next() {
		var r recipient
		if err := dbRows.Scan(&r.telegramID, &r.name); err != nil {
			return "", fmt.Errorf("scan recipient: %w", err)
		}
		// Don't send to self
		if r.telegramID != ctx.UserID {
			recipients = append(recipients, r)
		}
	}

	if len(recipients) == 0 {
		return "⚠️ Nessun utente trovato per il destinatario specificato.", nil
	}

	tg := telegram.New(t.botToken)
	var sent, failed int
	var sentNames []string

	for _, r := range recipients {
		// In Telegram, the chat_id for a DM equals the user's telegram_id
		if err := tg.Send(bg, r.telegramID, in.Message); err != nil {
			failed++
		} else {
			sent++
			name := r.name
			if name == "" {
				name = fmt.Sprintf("utente %d", r.telegramID)
			}
			sentNames = append(sentNames, name)
		}
	}

	result := fmt.Sprintf("✅ Messaggio inviato a %d utente/i: %s", sent, strings.Join(sentNames, ", "))
	if failed > 0 {
		result += fmt.Sprintf("\n⚠️ %d invio/i fallito/i.", failed)
	}
	return result, nil
}
