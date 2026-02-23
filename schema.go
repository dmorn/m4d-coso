package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ensureSchema creates all tables, functions, and RLS policies.
// Must run as superuser (adminPool).
func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{

		// ── Users ─────────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS users (
			telegram_id  BIGINT PRIMARY KEY,
			pg_user      TEXT NOT NULL UNIQUE,
			name         TEXT,
			role         TEXT NOT NULL DEFAULT 'cleaner'
			             CHECK (role IN ('manager', 'cleaner')),
			is_admin     BOOLEAN NOT NULL GENERATED ALWAYS AS (role = 'manager') STORED,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Migrations for existing tables
		`DO $$ BEGIN
			ALTER TABLE users ADD COLUMN IF NOT EXISTS name TEXT;
			ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'cleaner';
			ALTER TABLE users DROP COLUMN IF EXISTS is_admin;
			ALTER TABLE users ADD COLUMN IF NOT EXISTS is_admin BOOLEAN
				GENERATED ALWAYS AS (role = 'manager') STORED;
		EXCEPTION WHEN others THEN NULL; END $$`,

		// Credentials (kept server-side, never exposed to agents)
		`CREATE TABLE IF NOT EXISTS user_credentials (
			telegram_id  BIGINT PRIMARY KEY REFERENCES users(telegram_id) ON DELETE CASCADE,
			pg_password  TEXT NOT NULL
		)`,

		// ── Rooms ─────────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS rooms (
			id       SERIAL PRIMARY KEY,
			name     TEXT NOT NULL UNIQUE,
			floor    INT NOT NULL DEFAULT 1,
			notes    TEXT
		)`,

		// ── Assignments ───────────────────────────────────────────────────────
		// A cleaner is assigned to clean a room on a given date/shift.
		`CREATE TABLE IF NOT EXISTS assignments (
			id          SERIAL PRIMARY KEY,
			room_id     INT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			cleaner_id  BIGINT NOT NULL REFERENCES users(telegram_id),
			date        DATE NOT NULL DEFAULT CURRENT_DATE,
			shift       TEXT NOT NULL DEFAULT 'morning'
			            CHECK (shift IN ('morning', 'afternoon', 'evening')),
			status      TEXT NOT NULL DEFAULT 'pending'
			            CHECK (status IN ('pending', 'in_progress', 'done', 'skipped')),
			notes       TEXT,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,

		// ── Helper functions ─────────────────────────────────────────────────

		// current_telegram_id() — maps CURRENT_USER (pg role) → telegram_id
		`CREATE OR REPLACE FUNCTION current_telegram_id() RETURNS bigint AS $$
			SELECT telegram_id FROM users WHERE pg_user = current_user;
		$$ LANGUAGE sql STABLE SECURITY DEFINER`,

		// is_manager() — true if the current connection belongs to a manager
		`CREATE OR REPLACE FUNCTION is_manager() RETURNS boolean AS $$
			SELECT COALESCE(
				(SELECT role = 'manager' FROM users WHERE telegram_id = current_telegram_id()),
				false
			);
		$$ LANGUAGE sql STABLE SECURITY DEFINER`,

		// ── RLS ───────────────────────────────────────────────────────────────

		// rooms: managers full access, cleaners read-only
		`ALTER TABLE rooms ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS rooms_select  ON rooms;
			DROP POLICY IF EXISTS rooms_insert  ON rooms;
			DROP POLICY IF EXISTS rooms_update  ON rooms;
			DROP POLICY IF EXISTS rooms_delete  ON rooms;
		END $$`,
		`CREATE POLICY rooms_select ON rooms FOR SELECT USING (true)`,
		`CREATE POLICY rooms_insert ON rooms FOR INSERT WITH CHECK (is_manager())`,
		`CREATE POLICY rooms_update ON rooms FOR UPDATE USING (is_manager())`,
		`CREATE POLICY rooms_delete ON rooms FOR DELETE USING (is_manager())`,

		// assignments: all can read; managers full write; cleaners can only
		// update status/notes on their own assignments
		`ALTER TABLE assignments ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS assignments_select ON assignments;
			DROP POLICY IF EXISTS assignments_insert ON assignments;
			DROP POLICY IF EXISTS assignments_update ON assignments;
			DROP POLICY IF EXISTS assignments_delete ON assignments;
		END $$`,
		`CREATE POLICY assignments_select ON assignments FOR SELECT USING (true)`,
		`CREATE POLICY assignments_insert ON assignments FOR INSERT
			WITH CHECK (is_manager())`,
		`CREATE POLICY assignments_update ON assignments FOR UPDATE
			USING (is_manager() OR cleaner_id = current_telegram_id())`,
		`CREATE POLICY assignments_delete ON assignments FOR DELETE
			USING (is_manager())`,

		// users: all can read (to see colleagues); only managers can write
		`ALTER TABLE users ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS users_select ON users;
			DROP POLICY IF EXISTS users_write  ON users;
		END $$`,
		`CREATE POLICY users_select ON users FOR SELECT USING (true)`,
		`CREATE POLICY users_write  ON users FOR ALL USING (is_manager())`,
	}

	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema error: %w\nstmt: %.80s", err, s)
		}
	}
	return nil
}
