package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
		&scheduleReminderTool{adminPool: h.adminPool},
		&setRoomStatusTool{},
		&addReservationTool{adminPool: h.adminPool},
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

// ── set_room_status ──────────────────────────────────────────────────────────

type setRoomStatusTool struct{}

func (t *setRoomStatusTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "set_room_status",
		Description: "Aggiorna lo stato di una stanza nel ciclo di vita dell'hotel. " +
			"Usare quando lo stato cambia: ospiti arrivano/partono, pulizia inizia/finisce, stanza messa fuori servizio, ecc.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"room_id": {
					"type": "integer",
					"description": "ID della stanza"
				},
				"status": {
					"type": "string",
					"enum": ["available", "occupied", "stayover_due", "checkout_due", "cleaning", "ready", "out_of_service"],
					"description": "Nuovo stato: available=libera, occupied=ospiti presenti, stayover_due=riassetto da fare, checkout_due=pulizia completa da fare, cleaning=in pulizia, ready=pronta, out_of_service=fuori servizio"
				},
				"guest_name": {
					"type": "string",
					"description": "Nome ospite (opzionale, rilevante per occupied/checkout_due)"
				},
				"checkin_at": {
					"type": "string",
					"description": "Data/ora check-in ospiti, ISO 8601 (opzionale)"
				},
				"checkout_at": {
					"type": "string",
					"description": "Data/ora checkout ospiti, ISO 8601 (opzionale)"
				}
			},
			"required": ["room_id", "status"]
		}`),
	}
}

func (t *setRoomStatusTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}

	var in struct {
		RoomID    int64   `json:"room_id"`
		Status    string  `json:"status"`
		GuestName *string `json:"guest_name"`
		CheckinAt *string `json:"checkin_at"`
		CheckoutAt *string `json:"checkout_at"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}

	validStatuses := map[string]bool{
		"available": true, "occupied": true, "stayover_due": true,
		"checkout_due": true, "cleaning": true, "ready": true, "out_of_service": true,
	}
	if !validStatuses[in.Status] {
		return "", fmt.Errorf("invalid status: %s", in.Status)
	}

	q := `UPDATE rooms SET status = $1, guest_name = COALESCE($2, guest_name)`
	qArgs := []any{in.Status, in.GuestName}

	if in.CheckinAt != nil {
		ts, err := time.Parse(time.RFC3339, *in.CheckinAt)
		if err != nil {
			return "", fmt.Errorf("invalid checkin_at: %w", err)
		}
		q += fmt.Sprintf(", checkin_at = '%s'", ts.UTC().Format(time.RFC3339))
	}
	if in.CheckoutAt != nil {
		ts, err := time.Parse(time.RFC3339, *in.CheckoutAt)
		if err != nil {
			return "", fmt.Errorf("invalid checkout_at: %w", err)
		}
		q += fmt.Sprintf(", checkout_at = '%s'", ts.UTC().Format(time.RFC3339))
	}

	q += " WHERE id = $3"
	qArgs = append(qArgs, in.RoomID)

	tag, err := db.Exec(context.Background(), q, qArgs...)
	if err != nil {
		return "", fmt.Errorf("update room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("room %d not found", in.RoomID)
	}

	statusLabel := map[string]string{
		"available": "libera ✅", "occupied": "occupata 🛏️",
		"stayover_due": "riassetto da fare 🧹", "checkout_due": "pulizia completa da fare 🧹",
		"cleaning": "in pulizia 🫧", "ready": "pronta ✨", "out_of_service": "fuori servizio 🔧",
	}
	return fmt.Sprintf("✅ Stanza %d → %s", in.RoomID, statusLabel[in.Status]), nil
}

// ── add_reservation ──────────────────────────────────────────────────────────

type addReservationTool struct {
	adminPool *pgxpool.Pool
}

func (t *addReservationTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "add_reservation",
		Description: "Inserisce una nuova prenotazione per una stanza e aggiorna automaticamente lo stato della stanza. " +
			"Dopo l'inserimento, proponi all'utente di impostare reminder (es. 45 min prima del checkout per avvisare i cleaners).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"room_id": {
					"type": "integer",
					"description": "ID della stanza"
				},
				"guest_name": {
					"type": "string",
					"description": "Nome dell'ospite"
				},
				"checkin_at": {
					"type": "string",
					"description": "Data/ora check-in, ISO 8601 con timezone"
				},
				"checkout_at": {
					"type": "string",
					"description": "Data/ora checkout, ISO 8601 con timezone"
				},
				"notes": {
					"type": "string",
					"description": "Note aggiuntive sulla prenotazione"
				}
			},
			"required": ["room_id", "checkin_at", "checkout_at"]
		}`),
	}
}

func (t *addReservationTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	var in struct {
		RoomID    int64   `json:"room_id"`
		GuestName *string `json:"guest_name"`
		CheckinAt string  `json:"checkin_at"`
		CheckoutAt string `json:"checkout_at"`
		Notes     *string `json:"notes"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}

	checkin, err := time.Parse(time.RFC3339, in.CheckinAt)
	if err != nil {
		return "", fmt.Errorf("invalid checkin_at: %w", err)
	}
	checkout, err := time.Parse(time.RFC3339, in.CheckoutAt)
	if err != nil {
		return "", fmt.Errorf("invalid checkout_at: %w", err)
	}
	if !checkout.After(checkin) {
		return "", fmt.Errorf("checkout_at must be after checkin_at")
	}

	bg := context.Background()

	// Insert reservation
	var resID int64
	err = t.adminPool.QueryRow(bg,
		`INSERT INTO reservations (room_id, guest_name, checkin_at, checkout_at, notes, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		in.RoomID, in.GuestName, checkin, checkout, in.Notes, ctx.UserID,
	).Scan(&resID)
	if err != nil {
		return "", fmt.Errorf("insert reservation: %w", err)
	}

	// Update room status and guest info
	_, err = t.adminPool.Exec(bg,
		`UPDATE rooms SET
			guest_name  = $1,
			checkin_at  = $2,
			checkout_at = $3,
			status      = CASE
				WHEN now() >= $2 AND now() < $3 THEN 'occupied'
				ELSE 'available'
			END
		 WHERE id = $4`,
		in.GuestName, checkin, checkout, in.RoomID,
	)
	if err != nil {
		return "", fmt.Errorf("update room: %w", err)
	}

	guestStr := ""
	if in.GuestName != nil {
		guestStr = " per " + *in.GuestName
	}
	nights := int(checkout.Sub(checkin).Hours() / 24)
	nightStr := "notte"
	if nights != 1 {
		nightStr = "notti"
	}

	return fmt.Sprintf(
		"✅ Prenotazione #%d aggiunta: stanza %d%s\n📅 Check-in: %s\n📅 Checkout: %s\n🌙 %d %s\n\n"+
			"💡 Vuoi che programmi un reminder per i cleaners? (es. 45 min prima del checkout)",
		resID, in.RoomID, guestStr,
		checkin.Format("02/01/2006 15:04"),
		checkout.Format("02/01/2006 15:04"),
		nights, nightStr,
	), nil
}
