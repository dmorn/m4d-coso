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

		// current_telegram_id() — maps the session's login role → telegram_id.
		//
		// We use session_user (not current_user) because this function is
		// SECURITY DEFINER: inside it current_user becomes the function owner
		// (postgres), while session_user always reflects the original login role.
		`CREATE OR REPLACE FUNCTION current_telegram_id() RETURNS bigint AS $$
			SELECT telegram_id FROM users WHERE pg_user = session_user;
		$$ LANGUAGE sql STABLE SECURITY DEFINER`,

		// is_manager() — true if the current connection belongs to a manager
		`CREATE OR REPLACE FUNCTION is_manager() RETURNS boolean AS $$
			SELECT COALESCE(
				(SELECT role = 'manager' FROM users WHERE telegram_id = current_telegram_id()),
				false
			);
		$$ LANGUAGE sql STABLE SECURITY DEFINER`,

		// ── Re-grant table access to all existing tg_* roles ─────────────────
		// Grants issued in Register() are order-dependent: if a role was created
		// before its tables existed (e.g. from a previous session), the grant
		// silently succeeded against a non-existent table and is now missing.
		// This loop repairs any missing grants idempotently on every startup.
		`DO $$
		DECLARE r TEXT;
		BEGIN
			FOR r IN
				SELECT rolname FROM pg_roles
				WHERE rolname LIKE 'tg_%' AND rolcanlogin
			LOOP
				EXECUTE format('GRANT CONNECT ON DATABASE m4dtimes TO %I', r);
				EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', r);
				EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON rooms TO %I', r);
				EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON assignments TO %I', r);
				EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON users TO %I', r);
				EXECUTE format('GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO %I', r);
			END LOOP;
		END $$`,

		// ── RLS ───────────────────────────────────────────────────────────────
		//
		// Pattern: ENABLE ROW LEVEL SECURITY on every table, then drop+recreate
		// all policies on each boot so schema changes are always applied.
		//
		// Identity functions used in policies:
		//   current_telegram_id() → bigint  (maps CURRENT_USER → telegram_id)
		//   is_manager()          → boolean (true if current user has role='manager')
		//
		// Superuser (postgres) has BYPASSRLS implicitly — only user pools (tg_*)
		// are subject to these policies. Admin pool (postgres) is used only for
		// schema setup and user registration, never for agent tool calls.

		// ── user_credentials ─────────────────────────────────────────────────
		// Defense-in-depth: even if a GRANT is accidentally added in the future,
		// RLS blocks all access from non-superuser roles.
		// The admin pool (postgres/superuser) bypasses RLS automatically.
		`ALTER TABLE user_credentials ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS credentials_deny ON user_credentials;
		END $$`,
		// USING(false) = no row is ever visible or writable to any non-superuser
		`CREATE POLICY credentials_deny ON user_credentials USING (false)`,

		// ── rooms ─────────────────────────────────────────────────────────────
		// SELECT: everyone (all cleaners need to know which rooms exist)
		// INSERT/UPDATE/DELETE: managers only
		`ALTER TABLE rooms ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS rooms_select ON rooms;
			DROP POLICY IF EXISTS rooms_insert ON rooms;
			DROP POLICY IF EXISTS rooms_update ON rooms;
			DROP POLICY IF EXISTS rooms_delete ON rooms;
		END $$`,
		`CREATE POLICY rooms_select ON rooms FOR SELECT USING (true)`,
		`CREATE POLICY rooms_insert ON rooms FOR INSERT WITH CHECK (is_manager())`,
		`CREATE POLICY rooms_update ON rooms FOR UPDATE
			USING     (is_manager())
			WITH CHECK (is_manager())`,
		`CREATE POLICY rooms_delete ON rooms FOR DELETE USING (is_manager())`,

		// ── assignments ───────────────────────────────────────────────────────
		// SELECT: everyone (cleaners need to see their schedule)
		// INSERT: managers only
		// UPDATE: managers can change anything; cleaners can only touch their own
		//         rows — AND the resulting row must still belong to them
		//         (WITH CHECK prevents re-assigning cleaner_id to someone else)
		// DELETE: managers only
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
			USING      (is_manager() OR cleaner_id = current_telegram_id())
			WITH CHECK (is_manager() OR cleaner_id = current_telegram_id())`,
		`CREATE POLICY assignments_delete ON assignments FOR DELETE
			USING (is_manager())`,

		// ── users ─────────────────────────────────────────────────────────────
		// SELECT: everyone (cleaners need to see colleagues' names/shifts)
		// INSERT: managers only (tg_* roles are created by the system, not by LLM)
		// UPDATE: managers can edit any user; a user can update their own name only
		//         (pg_user and role are system-controlled — the LLM prompt should
		//         make this clear; RLS allows the write, field choice is up to the LLM)
		// DELETE: managers only
		`ALTER TABLE users ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS users_select ON users;
			DROP POLICY IF EXISTS users_write  ON users; -- legacy: FOR ALL, replaced below
			DROP POLICY IF EXISTS users_insert ON users;
			DROP POLICY IF EXISTS users_update ON users;
			DROP POLICY IF EXISTS users_delete ON users;
		END $$`,
		`CREATE POLICY users_select ON users FOR SELECT USING (true)`,
		`CREATE POLICY users_insert ON users FOR INSERT WITH CHECK (is_manager())`,
		`CREATE POLICY users_update ON users FOR UPDATE
			USING      (is_manager() OR telegram_id = current_telegram_id())
			WITH CHECK (is_manager() OR telegram_id = current_telegram_id())`,
		`CREATE POLICY users_delete ON users FOR DELETE USING (is_manager())`,
	}

	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema error: %w\nstmt: %.80s", err, s)
		}
	}
	return nil
}
