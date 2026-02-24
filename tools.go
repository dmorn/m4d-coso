package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HotelTools struct {
	registry *UserRegistry
	botName  string // e.g. "cimon_hotel_bot"
}

func newHotelTools(registry *UserRegistry, botName string) *HotelTools {
	return &HotelTools{registry: registry, botName: botName}
}

func (h *HotelTools) Tools() []agent.Tool {
	return []agent.Tool{
		&executeSQLTool{},
		&generateInviteTool{registry: h.registry, botName: h.botName},
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
