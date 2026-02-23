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

type HotelTools struct{}

func newHotelTools() *HotelTools { return &HotelTools{} }

func (h *HotelTools) Tools() []agent.Tool {
	return []agent.Tool{&executeSQLTool{}}
}

func poolFrom(ctx agent.ToolContext) (*pgxpool.Pool, error) {
	pool, ok := ctx.Extra.(*pgxpool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("no db pool in context")
	}
	return pool, nil
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
