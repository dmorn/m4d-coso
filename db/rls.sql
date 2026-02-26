-- ============================================================
-- db/rls.sql — Functions, RLS policies, grants
-- Applied via: psql $DATABASE_URL -f db/rls.sql
--
-- All statements are idempotent (CREATE OR REPLACE, DROP IF EXISTS).
-- Run after db/schema.sql on every deploy or schema change.
--
-- Atlas free tier does not capture these — that's why they live here.
-- ============================================================

-- ── Helper functions ──────────────────────────────────────────────────────────

-- current_telegram_id() maps the session's login role → telegram_id.
-- Uses session_user (not current_user) because this function is SECURITY DEFINER:
-- inside it, current_user becomes the function owner (postgres), while session_user
-- always reflects the original login role.
CREATE OR REPLACE FUNCTION current_telegram_id() RETURNS bigint AS $$
    SELECT telegram_id FROM users WHERE pg_user = session_user;
$$ LANGUAGE sql STABLE SECURITY DEFINER;

-- is_manager() returns true if the current connection belongs to a manager.
CREATE OR REPLACE FUNCTION is_manager() RETURNS boolean AS $$
    SELECT COALESCE(
        (SELECT role = 'manager' FROM users WHERE telegram_id = current_telegram_id()),
        false
    );
$$ LANGUAGE sql STABLE SECURITY DEFINER;

-- ── Re-grant table access to all existing tg_* roles ─────────────────────────
-- Repairs any missing grants idempotently. Run on every startup/deploy.
-- Grants issued during Register() may be missing if tables didn't exist yet.
DO $$
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
END $$;

-- ── RLS: prompts ─────────────────────────────────────────────────────────────
-- Prompts are system config — managers can CRUD, cleaners cannot touch them.
-- The bot reads them via adminPool (superuser, bypasses RLS).
ALTER TABLE prompts ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS prompts_select ON prompts;
DROP POLICY IF EXISTS prompts_all    ON prompts;
CREATE POLICY prompts_select ON prompts FOR SELECT USING (is_manager());
CREATE POLICY prompts_all    ON prompts FOR ALL    USING (is_manager()) WITH CHECK (is_manager());

-- ── RLS: user_credentials ─────────────────────────────────────────────────────
-- Defense-in-depth: no non-superuser can ever read credentials.
-- The admin pool (postgres/superuser) bypasses RLS automatically.
ALTER TABLE user_credentials ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS credentials_deny ON user_credentials;
CREATE POLICY credentials_deny ON user_credentials USING (false);

-- ── RLS: rooms ────────────────────────────────────────────────────────────────
-- SELECT: everyone (cleaners need to know which rooms exist)
-- INSERT/UPDATE/DELETE: managers only
ALTER TABLE rooms ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rooms_select ON rooms;
DROP POLICY IF EXISTS rooms_insert ON rooms;
DROP POLICY IF EXISTS rooms_update ON rooms;
DROP POLICY IF EXISTS rooms_delete ON rooms;
CREATE POLICY rooms_select ON rooms FOR SELECT USING (true);
CREATE POLICY rooms_insert ON rooms FOR INSERT WITH CHECK (is_manager());
CREATE POLICY rooms_update ON rooms FOR UPDATE
    USING      (is_manager())
    WITH CHECK (is_manager());
CREATE POLICY rooms_delete ON rooms FOR DELETE USING (is_manager());

-- ── RLS: assignments ──────────────────────────────────────────────────────────
-- SELECT: everyone (cleaners need to see all assignments)
-- INSERT: managers any row; cleaners can self-assign (cleaner_id = own telegram_id)
-- UPDATE: managers any; cleaners only their own
-- DELETE: managers any; cleaners only their own if still 'pending'
ALTER TABLE assignments ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS assignments_select ON assignments;
DROP POLICY IF EXISTS assignments_insert ON assignments;
DROP POLICY IF EXISTS assignments_update ON assignments;
DROP POLICY IF EXISTS assignments_delete ON assignments;
CREATE POLICY assignments_select ON assignments FOR SELECT USING (true);
CREATE POLICY assignments_insert ON assignments FOR INSERT
    WITH CHECK (is_manager() OR cleaner_id = current_telegram_id());
CREATE POLICY assignments_update ON assignments FOR UPDATE
    USING      (is_manager() OR cleaner_id = current_telegram_id())
    WITH CHECK (is_manager() OR cleaner_id = current_telegram_id());
CREATE POLICY assignments_delete ON assignments FOR DELETE
    USING (is_manager() OR (cleaner_id = current_telegram_id() AND status = 'pending'));

-- ── RLS: users ────────────────────────────────────────────────────────────────
-- SELECT: everyone
-- INSERT: managers only
-- UPDATE: managers any; a user can update their own row
-- DELETE: managers only
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS users_select ON users;
DROP POLICY IF EXISTS users_write  ON users;  -- legacy (FOR ALL)
DROP POLICY IF EXISTS users_insert ON users;
DROP POLICY IF EXISTS users_update ON users;
DROP POLICY IF EXISTS users_delete ON users;
CREATE POLICY users_select ON users FOR SELECT USING (true);
CREATE POLICY users_insert ON users FOR INSERT WITH CHECK (is_manager());
CREATE POLICY users_update ON users FOR UPDATE
    USING      (is_manager() OR telegram_id = current_telegram_id())
    WITH CHECK (is_manager() OR telegram_id = current_telegram_id());
CREATE POLICY users_delete ON users FOR DELETE USING (is_manager());

-- ── RLS: invites ──────────────────────────────────────────────────────────────
-- SELECT: managers see all; cleaners see only invites they redeemed
-- INSERT: managers only
ALTER TABLE invites ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS invites_select ON invites;
DROP POLICY IF EXISTS invites_insert ON invites;
CREATE POLICY invites_select ON invites FOR SELECT
    USING (is_manager() OR used_by = current_telegram_id());
CREATE POLICY invites_insert ON invites FOR INSERT
    WITH CHECK (is_manager());

-- ── RLS: reservations ─────────────────────────────────────────────────────────
-- SELECT: everyone (cleaners need context)
-- INSERT/UPDATE/DELETE: managers only
ALTER TABLE reservations ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS reservations_select ON reservations;
DROP POLICY IF EXISTS reservations_insert ON reservations;
DROP POLICY IF EXISTS reservations_update ON reservations;
DROP POLICY IF EXISTS reservations_delete ON reservations;
CREATE POLICY reservations_select ON reservations FOR SELECT USING (true);
CREATE POLICY reservations_insert ON reservations FOR INSERT WITH CHECK (is_manager());
CREATE POLICY reservations_update ON reservations FOR UPDATE
    USING (is_manager()) WITH CHECK (is_manager());
CREATE POLICY reservations_delete ON reservations FOR DELETE USING (is_manager());

-- ── RLS: reminders ────────────────────────────────────────────────────────────
-- SELECT: managers see all; others see their own
-- INSERT: created_by must be own telegram_id
-- UPDATE/DELETE: managers any; others their own
ALTER TABLE reminders ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS reminders_select ON reminders;
DROP POLICY IF EXISTS reminders_insert ON reminders;
DROP POLICY IF EXISTS reminders_update ON reminders;
DROP POLICY IF EXISTS reminders_delete ON reminders;
CREATE POLICY reminders_select ON reminders FOR SELECT
    USING (is_manager() OR created_by = current_telegram_id());
CREATE POLICY reminders_insert ON reminders FOR INSERT
    WITH CHECK (created_by = current_telegram_id());
CREATE POLICY reminders_update ON reminders FOR UPDATE
    USING      (is_manager() OR created_by = current_telegram_id())
    WITH CHECK (is_manager() OR created_by = current_telegram_id());
CREATE POLICY reminders_delete ON reminders FOR DELETE
    USING (is_manager() OR created_by = current_telegram_id());
