package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ensureSchema creates all tables and helper functions.
// Must run as a superuser (adminPool).
func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		// User registry: maps Telegram user → Postgres role
		`CREATE TABLE IF NOT EXISTS users (
			telegram_id  BIGINT PRIMARY KEY,
			pg_user      TEXT NOT NULL UNIQUE,
			is_admin     BOOLEAN NOT NULL DEFAULT FALSE,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,

		// Helper function: current user's telegram_id (for RLS policies)
		`CREATE OR REPLACE FUNCTION current_telegram_id() RETURNS bigint AS $$
			SELECT telegram_id FROM users WHERE pg_user = current_user;
		$$ LANGUAGE sql STABLE SECURITY DEFINER`,

		// Credentials store (passwords kept server-side, never exposed)
		`CREATE TABLE IF NOT EXISTS user_credentials (
			telegram_id  BIGINT PRIMARY KEY REFERENCES users(telegram_id),
			pg_password  TEXT NOT NULL
		)`,

		// Rooms table
		`CREATE TABLE IF NOT EXISTS rooms (
			id        SERIAL PRIMARY KEY,
			name      TEXT NOT NULL UNIQUE,
			floor     INT NOT NULL DEFAULT 1,
			occupied  BOOLEAN NOT NULL DEFAULT FALSE,
			notes     TEXT
		)`,
	}

	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema: %w\nstmt: %.60s", err, s)
		}
	}
	return nil
}
