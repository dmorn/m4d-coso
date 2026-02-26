-- ============================================================
-- db/schema.sql — Structural schema (source of truth)
-- Managed by Atlas: atlas schema apply --env local
--
-- NOTE: RLS policies, functions, and grants are NOT here.
-- Atlas free tier does not capture them.
-- See db/rls.sql for those.
-- ============================================================

-- Create "users" table
CREATE TABLE "users" (
  "telegram_id" bigint NOT NULL,
  "pg_user" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "name" text NULL,
  "role" text NOT NULL DEFAULT 'cleaner',
  "language" text NOT NULL DEFAULT 'Italian',
  "is_admin" boolean NULL GENERATED ALWAYS AS (role = 'manager'::text) STORED,
  PRIMARY KEY ("telegram_id"),
  CONSTRAINT "users_pg_user_key" UNIQUE ("pg_user")
);
-- Create "rooms" table
CREATE TABLE "rooms" (
  "id" serial NOT NULL,
  "name" text NOT NULL,
  "floor" integer NOT NULL DEFAULT 1,
  "notes" text NULL,
  "status" text NOT NULL DEFAULT 'available',
  "guest_name" text NULL,
  "checkin_at" timestamptz NULL,
  "checkout_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "rooms_name_key" UNIQUE ("name")
);
-- Create "assignments" table
CREATE TABLE "assignments" (
  "id" serial NOT NULL,
  "room_id" integer NOT NULL,
  "cleaner_id" bigint NOT NULL,
  "date" date NOT NULL DEFAULT CURRENT_DATE,
  "shift" text NOT NULL DEFAULT 'morning',
  "status" text NOT NULL DEFAULT 'pending',
  "notes" text NULL,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "type" text NOT NULL DEFAULT 'checkout',
  PRIMARY KEY ("id"),
  CONSTRAINT "assignments_cleaner_id_fkey" FOREIGN KEY ("cleaner_id") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "assignments_room_id_fkey" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "assignments_shift_check" CHECK (shift = ANY (ARRAY['morning'::text, 'afternoon'::text, 'evening'::text])),
  CONSTRAINT "assignments_status_check" CHECK (status = ANY (ARRAY['pending'::text, 'in_progress'::text, 'done'::text, 'skipped'::text]))
);
-- Create "invites" table
CREATE TABLE "invites" (
  "id" bigserial NOT NULL,
  "token" text NOT NULL,
  "role" text NOT NULL,
  "name" text NOT NULL,
  "created_by" bigint NOT NULL,
  "used_by" bigint NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "used_at" timestamptz NULL,
  "expires_at" timestamptz NOT NULL DEFAULT (now() + '7 days'::interval),
  PRIMARY KEY ("id"),
  CONSTRAINT "invites_token_key" UNIQUE ("token"),
  CONSTRAINT "invites_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "invites_used_by_fkey" FOREIGN KEY ("used_by") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "invites_role_check" CHECK (role = ANY (ARRAY['manager'::text, 'cleaner'::text]))
);
-- Create "reminders" table
CREATE TABLE "reminders" (
  "id" bigserial NOT NULL,
  "fire_at" timestamptz NOT NULL,
  "chat_id" bigint NOT NULL,
  "message" text NOT NULL,
  "room_id" integer NULL,
  "created_by" bigint NOT NULL,
  "fired_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "reminders_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "reminders_room_id_fkey" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE SET NULL
);
-- Create index "reminders_pending_idx" to table: "reminders"
CREATE INDEX "reminders_pending_idx" ON "reminders" ("fire_at") WHERE (fired_at IS NULL);
-- Create "reservations" table
CREATE TABLE "reservations" (
  "id" bigserial NOT NULL,
  "room_id" integer NOT NULL,
  "guest_name" text NULL,
  "checkin_at" timestamptz NOT NULL,
  "checkout_at" timestamptz NOT NULL,
  "notes" text NULL,
  "created_by" bigint NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "reservations_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "reservations_room_id_fkey" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create "user_credentials" table
CREATE TABLE "user_credentials" (
  "telegram_id" bigint NOT NULL,
  "pg_password" text NOT NULL,
  PRIMARY KEY ("telegram_id"),
  CONSTRAINT "user_credentials_telegram_id_fkey" FOREIGN KEY ("telegram_id") REFERENCES "users" ("telegram_id") ON UPDATE NO ACTION ON DELETE CASCADE
);
