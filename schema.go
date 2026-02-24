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
			id          SERIAL PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			floor       INT NOT NULL DEFAULT 1,
			notes       TEXT,
			status      TEXT NOT NULL DEFAULT 'available'
			            CHECK (status IN (
			            	'available',      -- libera e pulita
			            	'occupied',       -- ospiti presenti
			            	'stayover_due',   -- ospiti rimangono, riassetto da fare oggi
			            	'checkout_due',   -- checkout oggi, pulizia completa
			            	'cleaning',       -- cameriera al lavoro
			            	'ready',          -- pulita, pronta per check-in
			            	'out_of_service'  -- manutenzione
			            )),
			guest_name  TEXT,
			checkin_at  TIMESTAMPTZ,
			checkout_at TIMESTAMPTZ
		)`,
		// Migrations for rooms (idempotent)
		`DO $$ BEGIN
			ALTER TABLE rooms ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'available';
			ALTER TABLE rooms ADD COLUMN IF NOT EXISTS guest_name TEXT;
			ALTER TABLE rooms ADD COLUMN IF NOT EXISTS checkin_at TIMESTAMPTZ;
			ALTER TABLE rooms ADD COLUMN IF NOT EXISTS checkout_at TIMESTAMPTZ;
		EXCEPTION WHEN others THEN NULL; END $$`,

		// ── Assignments ───────────────────────────────────────────────────────
		// A cleaner is assigned to clean a room on a given date/shift.
		`CREATE TABLE IF NOT EXISTS assignments (
			id          SERIAL PRIMARY KEY,
			room_id     INT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			cleaner_id  BIGINT NOT NULL REFERENCES users(telegram_id),
			type        TEXT NOT NULL DEFAULT 'checkout'
			            CHECK (type IN ('stayover', 'checkout')),
			date        DATE NOT NULL DEFAULT CURRENT_DATE,
			shift       TEXT NOT NULL DEFAULT 'morning'
			            CHECK (shift IN ('morning', 'afternoon', 'evening')),
			status      TEXT NOT NULL DEFAULT 'pending'
			            CHECK (status IN ('pending', 'in_progress', 'done', 'skipped')),
			notes       TEXT,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Migrations for assignments
		`DO $$ BEGIN
			ALTER TABLE assignments ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'checkout';
		EXCEPTION WHEN others THEN NULL; END $$`,

		// ── Reservations ──────────────────────────────────────────────────────
		// Manager-entered reservations. Drive automatic room status transitions.
		`CREATE TABLE IF NOT EXISTS reservations (
			id          BIGSERIAL PRIMARY KEY,
			room_id     INT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			guest_name  TEXT,
			checkin_at  TIMESTAMPTZ NOT NULL,
			checkout_at TIMESTAMPTZ NOT NULL,
			notes       TEXT,
			created_by  BIGINT NOT NULL REFERENCES users(telegram_id),
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,

		// ── Reminders ─────────────────────────────────────────────────────────
		// Anyone can schedule reminders for themselves or others.
		// A background goroutine fires them and marks fired_at.
		`CREATE TABLE IF NOT EXISTS reminders (
			id          BIGSERIAL PRIMARY KEY,
			fire_at     TIMESTAMPTZ NOT NULL,
			chat_id     BIGINT NOT NULL,
			message     TEXT NOT NULL,
			room_id     INT REFERENCES rooms(id) ON DELETE SET NULL,
			created_by  BIGINT NOT NULL REFERENCES users(telegram_id),
			fired_at    TIMESTAMPTZ,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS reminders_pending_idx
			ON reminders (fire_at) WHERE fired_at IS NULL`,

		// ── Invites ───────────────────────────────────────────────────────────
		// Single-use tokens for Telegram deep-link onboarding (/start TOKEN).
		// Must be created before the re-grant loop below references it.
		`CREATE TABLE IF NOT EXISTS invites (
			id         BIGSERIAL PRIMARY KEY,
			token      TEXT UNIQUE NOT NULL,
			role       TEXT NOT NULL CHECK (role IN ('manager','cleaner')),
			name       TEXT NOT NULL,
			created_by BIGINT NOT NULL REFERENCES users(telegram_id),
			used_by    BIGINT REFERENCES users(telegram_id),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			used_at    TIMESTAMPTZ,
			expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '7 days'
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
				EXECUTE format('GRANT SELECT ON invites TO %I', r);
				EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON reservations TO %I', r);
				EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON reminders TO %I', r);
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

		// ── invites ───────────────────────────────────────────────────────────
		// Only managers can create invites; everyone can read their own (for
		// confirmation messages). Marking as used is done by the superuser pool.
		`ALTER TABLE invites ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS invites_select ON invites;
			DROP POLICY IF EXISTS invites_insert ON invites;
		END $$`,
		// Managers see all invites; a cleaner can only see invites they redeemed
		`CREATE POLICY invites_select ON invites FOR SELECT
			USING (is_manager() OR used_by = current_telegram_id())`,
		`CREATE POLICY invites_insert ON invites FOR INSERT
			WITH CHECK (is_manager())`,

		// ── reservations ──────────────────────────────────────────────────────
		// Everyone can see reservations (cleaners need context).
		// Only managers can insert/update/delete.
		`ALTER TABLE reservations ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS reservations_select ON reservations;
			DROP POLICY IF EXISTS reservations_insert ON reservations;
			DROP POLICY IF EXISTS reservations_update ON reservations;
			DROP POLICY IF EXISTS reservations_delete ON reservations;
		END $$`,
		`CREATE POLICY reservations_select ON reservations FOR SELECT USING (true)`,
		`CREATE POLICY reservations_insert ON reservations FOR INSERT WITH CHECK (is_manager())`,
		`CREATE POLICY reservations_update ON reservations FOR UPDATE
			USING (is_manager()) WITH CHECK (is_manager())`,
		`CREATE POLICY reservations_delete ON reservations FOR DELETE USING (is_manager())`,

		// ── reminders ─────────────────────────────────────────────────────────
		// Everyone can create and manage their own reminders.
		// Managers can see all reminders.
		`ALTER TABLE reminders ENABLE ROW LEVEL SECURITY`,
		`DO $$ BEGIN
			DROP POLICY IF EXISTS reminders_select ON reminders;
			DROP POLICY IF EXISTS reminders_insert ON reminders;
			DROP POLICY IF EXISTS reminders_update ON reminders;
			DROP POLICY IF EXISTS reminders_delete ON reminders;
		END $$`,
		`CREATE POLICY reminders_select ON reminders FOR SELECT
			USING (is_manager() OR created_by = current_telegram_id())`,
		`CREATE POLICY reminders_insert ON reminders FOR INSERT
			WITH CHECK (created_by = current_telegram_id())`,
		`CREATE POLICY reminders_update ON reminders FOR UPDATE
			USING (is_manager() OR created_by = current_telegram_id())
			WITH CHECK (is_manager() OR created_by = current_telegram_id())`,
		`CREATE POLICY reminders_delete ON reminders FOR DELETE
			USING (is_manager() OR created_by = current_telegram_id())`,
	}

	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema error: %w\nstmt: %.80s", err, s)
		}
	}
	return nil
}
