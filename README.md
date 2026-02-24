# m4d-coso

Hotel management agent for [Hotel Cimon](https://hotelcimon.it), built on the
[m4dtimes SDK](https://github.com/dmorn/m4dtimes).

Each hotel staff member interacts with the bot via Telegram. The agent connects
them to Postgres under their own dedicated role — Row Level Security enforces
what each person can read and write, at the database level, with no application
logic required.

## Architecture

```
Telegram message
      │
      ▼
 m4dtimes agent  ──  per-user ContextManager (isolated history)
      │                      │
      │  BuildExtra(userID)  │  OnAppend → session/<userID>.jsonl
      │  → per-user *pgxpool │
      │                      ▼
      │           sdk/session Store
      ▼
 execute_sql tool
      │
      ▼  connecting as tg_<telegram_id>
 PostgreSQL  ──  RLS policies enforce permissions
```

### Per-user Postgres roles

Every staff member maps to a dedicated Postgres LOGIN role named `tg_<telegram_id>`.
When the agent calls `execute_sql`, it runs the query through that user's connection
pool — Postgres enforces RLS as if the user ran the query directly.

```
postgres (superuser, BYPASSRLS)   ← admin pool: schema setup + invites + reminders
  tg_7756297856  LOGIN            ← Dani, manager
  tg_9876543210  LOGIN            ← Maria, cleaner
```

Credentials (one random 32-hex password per role) are generated at registration
time and stored in `user_credentials`. The admin pool retrieves them to open
each user's pool on first use. If a user re-registers (re-using an invite), the
password is rotated and the cached pool is evicted.

### Per-user conversation contexts

The agent maintains a `ContextManager` per user (keyed by `telegram_id`). Each
user's conversation history is completely isolated — a cleaner's "Sì" never
leaks into the manager's session or vice versa.

### ContextInjector: cross-user message relay

When `send_user_message` sends a DM to a user, it also injects that message into
the recipient's conversation context via `ToolContext.ContextInjector`. This
ensures the recipient's next LLM turn has full awareness of what was said:

```
Manager: "chiedi a Philip se può coprire i turni"
  → bot sends DM to Philip
  → bot injects DM into Philip's ContextManager

Philip: "Sì"
  → Philip's context: [assistant: "Dani ti chiede..."] [user: "Sì"]
  → bot replies to Philip + send_user_message → manager: "Philip risponde: Sì"
```

### Session recording

Every message — user input, assistant reply, tool calls, tool results — is
appended to a per-user JSONL file under `SESSION_DIR` (default: `./sessions`).
Format is compatible with Pi/OpenClaw session transcripts.

```jsonl
{"type":"session","version":1,"id":"...","userId":7756297856,"timestamp":"..."}
{"type":"message","id":"a1b2c3d4","parentId":"...","timestamp":"...","message":{"role":"user","content":[{"type":"text","text":"Ciao!"}]}}
{"type":"message","id":"e5f6a7b8","parentId":"a1b2c3d4","timestamp":"...","message":{"role":"assistant","usage":{"input_tokens":42,"output_tokens":15},"content":[...]}}
```

### Identity functions

```sql
-- Maps the session's login role → telegram_id.
-- Uses session_user (not current_user) because SECURITY DEFINER functions run
-- as the owner, making current_user = 'postgres' inside them.
CREATE FUNCTION current_telegram_id() RETURNS bigint AS $$
  SELECT telegram_id FROM users WHERE pg_user = session_user;
$$ LANGUAGE sql STABLE SECURITY DEFINER;

-- True if the current connection belongs to a manager.
CREATE FUNCTION is_manager() RETURNS boolean AS $$
  SELECT COALESCE(
    (SELECT role = 'manager' FROM users WHERE telegram_id = current_telegram_id()),
    false
  );
$$ LANGUAGE sql STABLE SECURITY DEFINER;
```

### RLS policies

| Table | SELECT | INSERT | UPDATE | DELETE |
|---|---|---|---|---|
| `rooms` | everyone | manager | manager | manager |
| `assignments` | everyone | manager OR own `cleaner_id`¹ | manager OR own row² | manager OR own pending row³ |
| `reservations` | everyone | manager | manager | manager |
| `reminders` | manager OR own | own (`created_by`) | manager OR own | manager OR own |
| `users` | everyone | manager | manager OR own row | manager |
| `invites` | manager OR redeemed by self | manager | — | — |
| `user_credentials` | nobody⁴ | nobody⁴ | nobody⁴ | nobody⁴ |

¹ Cleaners self-assign by INSERT with their own `telegram_id` as `cleaner_id`. Multiple cleaners can claim the same room/date/type.  
² `WITH CHECK` prevents changing `cleaner_id` to someone else (no re-assigning another cleaner's task).  
³ Cleaners can retract their own claim only while `status = 'pending'` — once started, it cannot be undone.  
⁴ `USING(false)` — absolutely no non-superuser access, regardless of any GRANT.

## Database schema

### `rooms`

| Column | Type | Description |
|--------|------|-------------|
| `id` | serial | Primary key |
| `name` | text UNIQUE | Room identifier, e.g. `"101"`, `"Suite A"` |
| `floor` | integer | Floor number |
| `notes` | text | Maintenance notes, special instructions |
| `status` | text | See room lifecycle below |
| `guest_name` | text | Current or incoming guest name |
| `checkin_at` | timestamptz | Current/next check-in time |
| `checkout_at` | timestamptz | Current/next checkout time |

#### Room lifecycle

```
available ──────────────────────────────────────────────► available
    │                                                           ▲
    │ check-in                                                  │
    ▼                                                           │
occupied ──► stayover_due ──► cleaning ──► ready ──────────────┤
    │                                                           │
    └──► checkout_due ──────► cleaning ──► ready ───────────────┘
    │
    └──► out_of_service (any → any, maintenance)
```

| Status | Meaning |
|--------|---------|
| `available` | Free and clean, ready for check-in |
| `occupied` | Guests currently in the room |
| `stayover_due` | Guests staying another night — daily tidy needed |
| `checkout_due` | Guests checking out today — full clean needed |
| `cleaning` | Cleaner currently working |
| `ready` | Cleaned and inspected, awaiting check-in |
| `out_of_service` | Maintenance, not available |

### `assignments`

Cleaning tasks. A room can have multiple assignments (one per cleaner) — cleaners self-assign.

| Column | Type | Description |
|--------|------|-------------|
| `id` | serial | Primary key |
| `room_id` | integer | → `rooms(id)` |
| `cleaner_id` | bigint | → `users(telegram_id)` |
| `type` | text | `stayover` (tidy) or `checkout` (full clean) |
| `date` | date | Cleaning date |
| `shift` | text | `morning` / `afternoon` / `evening` |
| `status` | text | `pending` → `in_progress` → `done` / `skipped` |
| `notes` | text | Cleaner's notes: damage, missing items, issues |
| `updated_at` | timestamptz | Last update |

### `reservations`

Manager-entered reservations. Source of truth for room occupancy and scheduling.

| Column | Type | Description |
|--------|------|-------------|
| `id` | bigserial | Primary key |
| `room_id` | integer | → `rooms(id)` |
| `guest_name` | text | Guest name |
| `checkin_at` | timestamptz | Arrival |
| `checkout_at` | timestamptz | Departure |
| `notes` | text | VIP notes, special requests |
| `created_by` | bigint | → `users(telegram_id)` |
| `created_at` | timestamptz | Entry time |

### `reminders`

Timed notifications sent by the reminder goroutine.

| Column | Type | Description |
|--------|------|-------------|
| `id` | bigserial | Primary key |
| `fire_at` | timestamptz | When to fire (indexed, must be future) |
| `chat_id` | bigint | Telegram chat to deliver to |
| `message` | text | Reminder text |
| `room_id` | integer | Optional room context |
| `created_by` | bigint | → `users(telegram_id)` |
| `fired_at` | timestamptz | NULL = pending; set when fired |

### `invites`

One-time invite tokens for Telegram deep-link onboarding.

| Column | Type | Description |
|--------|------|-------------|
| `id` | bigserial | Primary key |
| `token` | text UNIQUE | 32-hex random token |
| `role` | text | `manager` or `cleaner` |
| `name` | text | Display name for the new user |
| `created_by` | bigint | Manager who created it |
| `used_by` | bigint | Who redeemed it (NULL = unused) |
| `created_at` | timestamptz | Creation time |
| `used_at` | timestamptz | Redemption time (NULL = unused) |
| `expires_at` | timestamptz | 7-day TTL |

Deep link format: `https://t.me/<BOT_NAME>?start=<token>`

When a new user opens the link and clicks Start, Telegram sends `/start <token>`.
The `HandleStart` hook intercepts it before `Authorize`, redeems the token, creates
the Postgres role, and sends the welcome message — all before the LLM is invoked.

### `users`

Hotel staff registry.

| Column | Type | Description |
|--------|------|-------------|
| `telegram_id` | bigint PK | Telegram user ID |
| `pg_user` | text UNIQUE | Postgres role (`tg_<telegram_id>`) |
| `name` | text | Display name |
| `role` | text | `manager` or `cleaner` |
| `is_admin` | boolean | Computed: `role = 'manager'` |
| `created_at` | timestamptz | Registration date |

## Tools

| Tool | Who | Description |
|------|-----|-------------|
| `execute_sql` | all | Arbitrary SQL via user's RLS-constrained pool |
| `generate_invite` | manager | Creates one-time Telegram deep-link invite |
| `send_user_message` | all | DM to user by name, role, or `all`; injects into recipient's context |
| `schedule_reminder` | all | Timed Telegram reminder; fired by background goroutine |

## Setup

### Prerequisites

- Go 1.24+
- PostgreSQL 14+ (or Docker)
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- An Anthropic API key

### Local Postgres with Docker

```bash
docker run -d \
  --name postgres-dev \
  -e POSTGRES_DB=m4dtimes \
  -e POSTGRES_PASSWORD=devpassword \
  -p 5432:5432 \
  postgres:17-alpine
```

### Configuration

```env
TELEGRAM_BOT_TOKEN=<token from @BotFather>
LLM_API_KEY=<anthropic api key>
LLM_MODEL=claude-sonnet-4-6
DATABASE_URL=postgresql://postgres:devpassword@localhost:5432/m4dtimes
HOTEL_NAME="Hotel Cimon"
BOT_NAME=cimon_hotel_bot
SESSION_DIR=./sessions
```

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TELEGRAM_BOT_TOKEN` | ✅ | — | Bot token from @BotFather |
| `LLM_API_KEY` | ✅ | — | Anthropic API key |
| `LLM_MODEL` | | `claude-3-5-sonnet-20241022` | Model name |
| `DATABASE_URL` | | `postgresql://postgres:devpassword@localhost:5432/m4dtimes` | Superuser connection string |
| `HOTEL_NAME` | | `Hotel Cimon` | Used in system prompts |
| `BOT_NAME` | | `cimon_hotel_bot` | Bot username (for invite deep links) |
| `SESSION_DIR` | | `./sessions` | Directory for JSONL session transcripts |

### Build and run

```bash
go build -o m4d-coso .
./m4d-coso
```

On first start the agent will:

1. Connect to Postgres with the admin pool
2. Run `ensureSchema` — creates all tables, functions, RLS policies (idempotent)
3. Re-grant table access to all existing `tg_*` roles (repairs stale grants from previous runs)
4. Bootstrap the admin user (Telegram ID in `main.go`) as manager if not registered
5. Create `SESSION_DIR` and start the session store
6. Start the reminder goroutine (polls every 30s, fires pending reminders)

### Auto-start with systemd

```ini
[Unit]
Description=Hotel Cimon agent
After=network.target docker.service
Wants=docker.service

[Service]
User=vins
WorkingDirectory=/home/vins/repos/m4d-coso
EnvironmentFile=/home/vins/repos/m4d-coso/.env
ExecStart=/home/vins/repos/m4d-coso/m4d-coso
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

## Onboarding new staff

1. Manager asks the bot: *"Genera un link per Maria, ruolo cleaner"*
2. Bot calls `generate_invite`, returns `https://t.me/cimon_hotel_bot?start=<token>`
3. Manager forwards the link to Maria
4. Maria opens the link → Telegram sends `/start <token>` → bot registers her and sends welcome

> **Note:** The deep link only auto-sends `/start <token>` if the user has never
> opened the bot before. For users already in the bot's chat, they must type
> `/start <token>` manually.

## Room lifecycle walkthrough

### Check-in

```sql
-- 1. Insert reservation
INSERT INTO reservations (room_id, guest_name, checkin_at, checkout_at, created_by)
VALUES (3, 'Rossi Mario', '2026-03-01 14:00:00+01', '2026-03-05 11:00:00+01', <manager_id>);

-- 2. Update room status
UPDATE rooms SET status='occupied', guest_name='Rossi Mario',
  checkin_at='2026-03-01 14:00:00+01', checkout_at='2026-03-05 11:00:00+01'
WHERE id=3;
```

### Cleaner self-assignment

Cleaners see rooms with `checkout_due` or `stayover_due` and claim them:

```sql
-- View available rooms
SELECT r.id, r.name, r.status, COUNT(a.id) AS assigned_cleaners
FROM rooms r
LEFT JOIN assignments a ON a.room_id=r.id AND a.date=CURRENT_DATE
WHERE r.status IN ('checkout_due','stayover_due','cleaning')
GROUP BY r.id ORDER BY r.floor;

-- Self-assign (cleaner_id enforced by RLS — can only insert own id)
INSERT INTO assignments (room_id, cleaner_id, type, date, shift, status)
VALUES (3, current_telegram_id(), 'checkout', CURRENT_DATE, 'morning', 'pending');
```

### Cleaning progress

```sql
UPDATE assignments SET status='in_progress', updated_at=now()
WHERE id=? AND cleaner_id=current_telegram_id();

UPDATE assignments SET status='done', updated_at=now()
WHERE id=? AND cleaner_id=current_telegram_id();

UPDATE rooms SET status='ready' WHERE id=3;
```

## Code structure

```
m4d-coso/
├── main.go      — agent wiring: pool, registry, options, session store, reminder loop
├── schema.go    — ensureSchema(): tables, functions, RLS policies, grant repair loop
├── users.go     — UserRegistry: Postgres role lifecycle, per-user pool cache
├── tools.go     — 4 tools: execute_sql, generate_invite, send_user_message, schedule_reminder
├── prompt.go    — role-specific system prompts: managerPrompt, cleanerPrompt
├── go.mod
├── .env
└── sessions/    — per-user JSONL transcripts (SESSION_DIR)
```

## Key design decisions

**Why `execute_sql` instead of typed tools for CRUD?**
A hotel has many query patterns. Typed tools would need one per query shape.
`execute_sql` lets the LLM compose arbitrary SQL; RLS enforces safety. The schema
and workflow examples in the system prompt guide the LLM to do the right thing.

**Why per-user Postgres roles instead of a single app role?**
RLS policies can reference `current_telegram_id()` — the DB itself becomes the
permission engine. No application-level `if user.role == 'manager'` checks.

**Why `session_user` in `current_telegram_id()`?**
SECURITY DEFINER functions run as owner (`postgres`). Inside them `current_user`
returns `postgres`, not the caller. `session_user` always reflects the login role.

**Why `WITH CHECK` on assignment UPDATE?**
`USING` controls which rows are visible for UPDATE. Without `WITH CHECK`, a
cleaner could update their row but change `cleaner_id` to someone else.
`WITH CHECK` enforces the predicate on the resulting row, not just the original.

**Why a grant repair loop in `ensureSchema`?**
Postgres roles are cluster-scoped. If the bot restarts against a fresh DB, any
`tg_*` roles from a previous run exist but may lack grants on newly created
tables. The loop re-issues all grants idempotently on every startup.

**Why per-user `ContextManager` instead of a shared one?**
A shared context caused cross-user contamination: when Philip replied "Sì" after
the manager asked him to cover shifts, the bot had the manager's conversation
history and replied as if answering the manager — but sent it to Philip.
Per-user contexts are the only correct design for a multi-user bot.

**Why `ContextInjector` in `ToolContext`?**
`send_user_message` needs to inject the sent DM into the recipient's context so
their next LLM turn has awareness of the question. Direct coupling to `Agent`
would invert the dependency (tools → agent). Instead, the SDK defines a
`ContextInjector` interface; the agent implements it and passes itself via
`ToolContext`. Tools use the interface, not the concrete type.

**Why JSONL session recording?**
Journald is sufficient for ops debugging but insufficient for product analytics
(token usage per user, conversation replay, LLM behaviour auditing). JSONL files
per user are append-only, human-readable, and compatible with Pi/OpenClaw tooling.
The `OnAppend` hook on `ContextManager` records every message without modifying
the agent loop.
