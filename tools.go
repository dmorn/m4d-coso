package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dmorn/m4dtimes/sdk/agent"
	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ensure pgxpool is used (via poolFrom)

// HotelTools groups all hotel management tools.
// db may be nil at registration time — the actual pool is taken from ToolContext.Extra
// (injected by BuildExtra in main.go as a *pgxpool.Pool per user).
type HotelTools struct {
	db *pgxpool.Pool
}

func newHotelTools(db *pgxpool.Pool) *HotelTools {
	return &HotelTools{db: db}
}

func (h *HotelTools) Tools() []agent.Tool {
	return []agent.Tool{
		&listRoomsTool{},
		&setOccupiedTool{},
		&addRoomTool{},
		&addNoteTool{},
	}
}

// poolFrom extracts the per-user *pgxpool.Pool from ToolContext.Extra.
func poolFrom(ctx agent.ToolContext) (*pgxpool.Pool, error) {
	pool, ok := ctx.Extra.(*pgxpool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("no db pool in context")
	}
	return pool, nil
}

// ── list_rooms ──────────────────────────────────────────────────────────────

type listRoomsTool struct{}

func (t *listRoomsTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "list_rooms",
		Description: "List all hotel rooms with their current status (occupied/free) and notes.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *listRoomsTool) Execute(ctx agent.ToolContext, _ json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}
	rows, err := db.Query(context.Background(),
		`SELECT id, name, floor, occupied, COALESCE(notes,'') FROM rooms ORDER BY floor, name`)
	if err != nil {
		return "", fmt.Errorf("query rooms: %w", err)
	}
	defer rows.Close()

	var result string
	for rows.Next() {
		var id int
		var name, notes string
		var floor int
		var occupied bool
		if err := rows.Scan(&id, &name, &floor, &occupied, &notes); err != nil {
			return "", err
		}
		status := "free"
		if occupied {
			status = "OCCUPIED"
		}
		line := fmt.Sprintf("- Room %s (floor %d): %s", name, floor, status)
		if notes != "" {
			line += " — " + notes
		}
		result += line + "\n"
	}
	if result == "" {
		return "No rooms found.", nil
	}
	return result, nil
}

// ── set_occupied ─────────────────────────────────────────────────────────────

type setOccupiedTool struct{}

func (t *setOccupiedTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "set_occupied",
		Description: "Mark a room as occupied or free.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"room_name": {"type": "string", "description": "Room name, e.g. '101'"},
				"occupied":  {"type": "boolean", "description": "true = occupied, false = free"}
			},
			"required": ["room_name", "occupied"]
		}`),
	}
}

func (t *setOccupiedTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}
	var in struct {
		RoomName string `json:"room_name"`
		Occupied bool   `json:"occupied"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	tag, err := db.Exec(context.Background(),
		`UPDATE rooms SET occupied=$1 WHERE name=$2`, in.Occupied, in.RoomName)
	if err != nil {
		return "", fmt.Errorf("update room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Sprintf("Room '%s' not found.", in.RoomName), nil
	}
	status := "free"
	if in.Occupied {
		status = "occupied"
	}
	return fmt.Sprintf("Room %s marked as %s.", in.RoomName, status), nil
}

// ── add_room ─────────────────────────────────────────────────────────────────

type addRoomTool struct{}

func (t *addRoomTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "add_room",
		Description: "Add a new room to the hotel.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":  {"type": "string", "description": "Room name, e.g. '101'"},
				"floor": {"type": "integer", "description": "Floor number"}
			},
			"required": ["name", "floor"]
		}`),
	}
}

func (t *addRoomTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}
	var in struct {
		Name  string `json:"name"`
		Floor int    `json:"floor"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	_, err = db.Exec(context.Background(),
		`INSERT INTO rooms (name, floor) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`,
		in.Name, in.Floor)
	if err != nil {
		return "", fmt.Errorf("insert room: %w", err)
	}
	return fmt.Sprintf("Room %s (floor %d) added.", in.Name, in.Floor), nil
}

// ── add_note ─────────────────────────────────────────────────────────────────

type addNoteTool struct{}

func (t *addNoteTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        "add_note",
		Description: "Add or update a note on a room (e.g. maintenance, special requests).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"room_name": {"type": "string"},
				"note":      {"type": "string", "description": "The note to set. Empty string clears it."}
			},
			"required": ["room_name", "note"]
		}`),
	}
}

func (t *addNoteTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
	db, err := poolFrom(ctx)
	if err != nil {
		return "", err
	}
	var in struct {
		RoomName string `json:"room_name"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	_, err = db.Exec(context.Background(),
		`UPDATE rooms SET notes=$1 WHERE name=$2`, in.Note, in.RoomName)
	if err != nil {
		return "", fmt.Errorf("update note: %w", err)
	}
	return fmt.Sprintf("Note updated for room %s.", in.RoomName), nil
}
