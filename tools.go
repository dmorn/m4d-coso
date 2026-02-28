package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"strings"
	"time"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/dmorn/m4dtimes/sdk/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

// generateUUID returns a random UUID v4 string (8-4-4-4-12 hex).
func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type HotelTools struct {
	registry  *UserRegistry
	botName   string // e.g. "cimon_hotel_bot"
	botToken  string // Telegram bot token for outbound messages
	adminPool *pgxpool.Pool
	bus       agent.EventBus
}

func newHotelTools(registry *UserRegistry, botName, botToken string, adminPool *pgxpool.Pool, bus agent.EventBus) *HotelTools {
	return &HotelTools{registry: registry, botName: botName, botToken: botToken, adminPool: adminPool, bus: bus}
}

func (h *HotelTools) Tools() []agent.Tool {
	return []agent.Tool{
		&executeSQLTool{},
		&readSchemaTool{},
		&generateInviteTool{registry: h.registry, botName: h.botName, botToken: h.botToken},
		&sendUserMessageTool{adminPool: h.adminPool, botToken: h.botToken, bus: h.bus},
		&scheduleReminderTool{adminPool: h.adminPool},
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
	botToken string
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

	// Build HTML directly — the URL lives inside an href attribute, so underscores
	// are never interpreted as markdown italic markers by the SDK converter.
	htmlMsg := fmt.Sprintf(
		"🔗 <b>Invito per %s</b> (%s)\n\n<a href=\"%s\">%s</a>\n\n<i>Scade tra 7 giorni · monouso</i>",
		htmlpkg.EscapeString(in.Name), in.Role, link, link,
	)

	// Send the link directly to the manager's chat — bypasses LLM text generation,
	// so the URL is never accidentally modified by the model.
	if ctx.ChatID != 0 {
		tg := telegram.New(t.botToken)
		if err := tg.SendHTML(context.Background(), ctx.ChatID, htmlMsg); err != nil {
			// Don't fail the tool call — the LLM can still relay the link as fallback
			return fmt.Sprintf("✅ Invito creato per %s (%s), ma l'invio diretto è fallito.\nLink: %s\n⚠️ Il link scade tra 7 giorni ed è monouso.", in.Name, in.Role, link), nil
		}
	}

	return fmt.Sprintf("✅ Invito per %s (%s) inviato direttamente in chat. Non ripetere il link nella risposta — è già stato consegnato.", in.Name, in.Role), nil
}

// ── read_schema ───────────────────────────────────────────────────────────────

type readSchemaTool struct{}

func (t *readSchemaTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "read_schema",
		Description: "Inspect the live database schema: tables, columns, types, and foreign keys. " +
			"Use this when you need to discover what the database contains, or to debug a failed SQL query.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (t *readSchemaTool) Execute(ctx agent.ToolContext, _ json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}
	return dumpSchema(context.Background(), db)
}

// dumpSchema queries information_schema and returns a compact human-readable
// schema dump (tables, columns, types, FKs). Used both by readSchemaTool and
// injected directly into the system prompt at session start.
func dumpSchema(ctx context.Context, db *pgxpool.Pool) (string, error) {
	// Columns per table — exclude internal tables and implementation-detail columns
	colRows, err := db.Query(ctx, `
		SELECT table_name, column_name, data_type, column_default, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name NOT IN ('user_credentials')
		  AND NOT (table_name = 'users' AND column_name IN ('pg_user', 'is_admin'))
		ORDER BY table_name, ordinal_position
	`)
	if err != nil {
		return "", fmt.Errorf("schema query: %w", err)
	}
	defer colRows.Close()

	type colInfo struct {
		name, dataType, defaultVal, nullable string
	}
	tables := make(map[string][]colInfo)
	var tableOrder []string
	seen := make(map[string]bool)

	for colRows.Next() {
		var tbl, col, dtype, nullable string
		var def *string
		if err := colRows.Scan(&tbl, &col, &dtype, &def, &nullable); err != nil {
			return "", err
		}
		defStr := ""
		if def != nil {
			defStr = *def
		}
		if !seen[tbl] {
			tableOrder = append(tableOrder, tbl)
			seen[tbl] = true
		}
		tables[tbl] = append(tables[tbl], colInfo{col, dtype, defStr, nullable})
	}

	// Foreign keys
	fkRows, err := db.Query(ctx, `
		SELECT
			kcu.table_name, kcu.column_name,
			ccu.table_name AS ref_table, ccu.column_name AS ref_column
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		JOIN information_schema.constraint_column_usage ccu
			ON tc.constraint_name = ccu.constraint_name AND tc.table_schema = ccu.table_schema
		WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = 'public'
		  AND kcu.table_name NOT IN ('user_credentials')
		ORDER BY kcu.table_name, kcu.column_name
	`)
	if err != nil {
		return "", fmt.Errorf("fk query: %w", err)
	}
	defer fkRows.Close()

	type fkInfo struct{ col, refTable, refCol string }
	fks := make(map[string][]fkInfo)
	for fkRows.Next() {
		var tbl, col, refTbl, refCol string
		if err := fkRows.Scan(&tbl, &col, &refTbl, &refCol); err != nil {
			return "", err
		}
		fks[tbl] = append(fks[tbl], fkInfo{col, refTbl, refCol})
	}

	// Format output
	var sb strings.Builder
	for _, tbl := range tableOrder {
		sb.WriteString(fmt.Sprintf("## %s\n", tbl))
		for _, c := range tables[tbl] {
			null := ""
			if c.nullable == "YES" {
				null = " NULL"
			}
			def := ""
			if c.defaultVal != "" {
				def = fmt.Sprintf(" DEFAULT %s", c.defaultVal)
			}
			sb.WriteString(fmt.Sprintf("  %-20s %s%s%s\n", c.name, c.dataType, null, def))
		}
		if refs, ok := fks[tbl]; ok {
			for _, fk := range refs {
				sb.WriteString(fmt.Sprintf("  FK: %s → %s(%s)\n", fk.col, fk.refTable, fk.refCol))
			}
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
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
	bus       agent.EventBus
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
		// Look up recipient role to decide whether to publish a relay event.
		var recipientRole string
		_ = t.adminPool.QueryRow(bg,
			`SELECT role FROM users WHERE telegram_id = $1`, r.telegramID,
		).Scan(&recipientRole)

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

			// Inject the sent message into the recipient's conversation context
			// so their next LLM turn has full awareness of what was said to them.
			if ctx.ContextInjector != nil {
				ctx.ContextInjector.Inject(r.telegramID, llm.Message{
					Role: "assistant",
					Content: []llm.ContentBlock{{Type: "text", Text: in.Message}},
				})
			}

			// If the recipient is a manager and we have an event bus, publish a
			// relay event so the manager agent processes the message autonomously.
			if recipientRole == "manager" && t.bus != nil {
				senderName := "system"
				if ctx.UserID != 0 {
					var sName string
					_ = t.adminPool.QueryRow(bg,
						`SELECT COALESCE(name, '') FROM users WHERE telegram_id = $1`, ctx.UserID,
					).Scan(&sName)
					if sName != "" {
						senderName = sName
					}
				}
				t.bus.Publish(agent.AgentEvent{
					Kind:     agent.EventRelay,
					TargetID: r.telegramID,
					ChatID:   r.telegramID,
					Content:  in.Message,
					Source:   senderName,
					EventID:  generateUUID(),
				})
			}
		}
	}

	result := fmt.Sprintf("✅ Messaggio inviato a %d utente/i: %s", sent, strings.Join(sentNames, ", "))
	if failed > 0 {
		result += fmt.Sprintf("\n⚠️ %d invio/i fallito/i.", failed)
	}
	return result, nil
}

// ── schedule_reminder ────────────────────────────────────────────────────────

type scheduleReminderTool struct {
	adminPool *pgxpool.Pool
}

func (t *scheduleReminderTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "schedule_reminder",
		Description: "Programma un reminder che verrà inviato via Telegram a una data/ora precisa. " +
			"Usa questo tool PROATTIVAMENTE: ogni volta che l'utente menziona un orario, un evento futuro, " +
			"o dice 'ricordami', proponi o crea subito un reminder. " +
			"Il destinatario può essere l'utente stesso o un altro membro dello staff (per nome).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"fire_at": {
					"type": "string",
					"description": "Data e ora di invio in formato ISO 8601 con timezone, es. '2026-02-24T10:30:00+01:00'"
				},
				"message": {
					"type": "string",
					"description": "Testo del reminder da inviare"
				},
				"to": {
					"type": "string",
					"description": "Destinatario: 'me' per se stessi, oppure nome di un altro utente registrato. Default: 'me'."
				},
				"room_id": {
					"type": "integer",
					"description": "ID della stanza a cui si riferisce il reminder (opzionale, per contesto)"
				}
			},
			"required": ["fire_at", "message"]
		}`),
	}
}

func (t *scheduleReminderTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	var in struct {
		FireAt  string `json:"fire_at"`
		Message string `json:"message"`
		To      string `json:"to"`
		RoomID  *int64 `json:"room_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.FireAt == "" || in.Message == "" {
		return "", fmt.Errorf("fire_at and message are required")
	}

	fireAt, err := time.Parse(time.RFC3339, in.FireAt)
	if err != nil {
		return "", fmt.Errorf("invalid fire_at format, use ISO 8601 with timezone (e.g. 2026-02-24T10:30:00+01:00): %w", err)
	}
	if fireAt.Before(time.Now()) {
		return "", fmt.Errorf("fire_at must be in the future")
	}

	// Resolve destination chat_id
	chatID := ctx.ChatID // default: self
	toName := ""
	if in.To != "" && in.To != "me" && in.To != "io" {
		var recipientID int64
		err := t.adminPool.QueryRow(context.Background(),
			`SELECT telegram_id, name FROM users WHERE lower(name) = lower($1)`, in.To,
		).Scan(&recipientID, &toName)
		if err != nil {
			return "", fmt.Errorf("utente '%s' non trovato", in.To)
		}
		chatID = recipientID
	}

	_, err = t.adminPool.Exec(context.Background(),
		`INSERT INTO reminders (fire_at, chat_id, message, room_id, created_by)
		 VALUES ($1, $2, $3, $4, $5)`,
		fireAt, chatID, in.Message, in.RoomID, ctx.UserID,
	)
	if err != nil {
		return "", fmt.Errorf("insert reminder: %w", err)
	}

	dest := "te"
	if toName != "" {
		dest = toName
	}
	return fmt.Sprintf("⏰ Reminder programmato per %s alle %s (destinatario: %s).",
		fireAt.Format("02/01/2006"), fireAt.Format("15:04"), dest), nil
}

