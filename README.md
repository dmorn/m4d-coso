# m4d-coso

Hotel management agent for [Hotel Cimon](https://hotelcimon.it), built on the
[m4dtimes SDK](https://github.com/dmorn/m4dtimes).

Each hotel staff member interacts with the bot via Telegram. The bot connects
them to Postgres under their own dedicated role — Row Level Security enforces
what each person can read and write, at the database level, with no application
logic required.

## Architecture

```
Telegram message
      │
      ▼
 m4dtimes agent  (claude-sonnet)
      │
      │  BuildExtra(userID) → per-user *pgxpool.Pool
      │
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
postgres (superuser, BYPASSRLS)   ← admin pool: schema setup only
  tg_7756297856  LOGIN            ← Dani, manager
  tg_9876543210  LOGIN            ← Maria, cleaner
  tg_1234567890  LOGIN            ← Luigi, cleaner
```

Credentials (one random 32-hex password per role) are generated at registration
time and stored in `user_credentials`. The admin pool retrieves them to open
each user's pool on first use.

### Identity functions

```sql
-- Maps the session's login role → telegram_id.
-- Uses session_user (not current_user) because SECURITY DEFINER functions run
-- as the owner, which would make current_user = 'postgres'.
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
| `assignments` | everyone | manager | manager OR own row¹ | manager |
| `users` | everyone | manager | manager OR own row² | manager |
| `user_credentials` | nobody³ | nobody³ | nobody³ | nobody³ |

¹ Cleaner can update their own assignments, but `WITH CHECK` prevents changing `cleaner_id` (no re-assigning the task to someone else).  
² A cleaner can update their own `users` row (e.g. name). The system prompt should restrict what the LLM actually allows.  
³ `USING(false)` — no non-superuser role can ever read or write this table, regardless of any GRANT.

## Database schema

### `rooms`

| Column | Type | Description |
|--------|------|-------------|
| `id` | serial | Primary key |
| `name` | text UNIQUE | Room identifier, e.g. `"101"`, `"Suite A"` |
| `floor` | integer | Floor number |
| `notes` | text | Manager notes: maintenance issues, VIP guests, etc. |

### `assignments`

Cleaning tasks: who cleans which room, on what day and shift.

| Column | Type | Description |
|--------|------|-------------|
| `id` | serial | Primary key |
| `room_id` | integer | → `rooms(id)` |
| `cleaner_id` | bigint | → `users(telegram_id)` |
| `date` | date | Cleaning date (default: today) |
| `shift` | text | `morning` / `afternoon` / `evening` |
| `status` | text | `pending` → `in_progress` → `done` / `skipped` |
| `notes` | text | Cleaner's notes: damage found, missing items, etc. |
| `updated_at` | timestamptz | Last update timestamp |

### `users`

Hotel staff registry.

| Column | Type | Description |
|--------|------|-------------|
| `telegram_id` | bigint | Primary key — Telegram user ID |
| `pg_user` | text UNIQUE | Postgres role name (`tg_<telegram_id>`) |
| `name` | text | Display name |
| `role` | text | `manager` or `cleaner` |
| `is_admin` | boolean | Computed: `role = 'manager'` |
| `created_at` | timestamptz | Registration date |

### `user_credentials`

Postgres passwords. Never exposed to the agent or users.

| Column | Type | Description |
|--------|------|-------------|
| `telegram_id` | bigint | → `users(telegram_id)` |
| `pg_password` | text | Random 32-hex password |

## Setup

### Prerequisites

- Go 1.24+
- PostgreSQL 14+ (or Docker — see below)
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

```bash
cp .env.example .env
```

Edit `.env`:

```env
TELEGRAM_BOT_TOKEN=<token from @BotFather>
LLM_API_KEY=<anthropic api key>
DATABASE_URL=postgresql://postgres:devpassword@localhost:5432/m4dtimes
HOTEL_NAME="Hotel Cimon"
```

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TELEGRAM_BOT_TOKEN` | ✅ | — | Bot token from @BotFather |
| `LLM_API_KEY` | ✅ | — | Anthropic API key |
| `DATABASE_URL` | | `postgresql://postgres:devpassword@localhost:5432/m4dtimes` | Admin connection string (superuser) |
| `HOTEL_NAME` | | `Hotel Cimon` | Hotel name shown in system prompts |

### Build and run

```bash
go build -o m4d-coso .
./m4d-coso
```

On first start the agent will:

1. Connect to Postgres with the admin pool
2. Run `ensureSchema` — creates tables, functions, and RLS policies (idempotent)
3. Re-grant table access to all existing `tg_*` roles (repairs stale grants)
4. Bootstrap the admin user (hardcoded Telegram ID `7756297856`) as manager if not already registered

### Auto-start with systemd

```ini
# ~/.config/systemd/user/m4d-coso.service
[Unit]
Description=Hotel Cimon agent
After=network.target

[Service]
WorkingDirectory=/home/vins/repos/m4d-coso
EnvironmentFile=/home/vins/repos/m4d-coso/.env
ExecStart=/home/vins/repos/m4d-coso/m4d-coso
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now m4d-coso
systemctl --user status m4d-coso
journalctl --user -u m4d-coso -f
```

## Usage

### For managers

Send any message to the bot describing what you need:

- *"Show me today's pending assignments"*
- *"Assign Maria to rooms 101 and 102, morning shift"*
- *"Add room 305 on floor 3"*
- *"Who finished their shift today?"*
- *"Add a note to room 201: broken shower, maintenance called"*

### For cleaners

- *"What do I have today?"*
- *"Mark room 101 as done"*
- *"I found a broken chair in room 203, add a note"*
- *"What's the status of the morning shift?"*

### First-time staff registration

When a new cleaner sends their first message, they are automatically registered
as a `cleaner`. The manager can then verify or promote them:

```sql
-- Promote a user to manager
UPDATE users SET role = 'manager' WHERE telegram_id = <id>;

-- List all staff
SELECT telegram_id, name, role, created_at FROM users ORDER BY role, name;
```

### Adding rooms (manager)

Either via the bot:
> *"Add rooms 101 through 120 on floors 1 and 2"*

Or directly via psql:

```sql
INSERT INTO rooms (name, floor) VALUES
  ('101', 1), ('102', 1), ('103', 1),
  ('201', 2), ('202', 2), ('203', 2);
```

### Creating assignments (manager)

Via the bot:
> *"Assign Maria the morning shift for rooms 101-103 today"*

Or directly:

```sql
INSERT INTO assignments (room_id, cleaner_id, date, shift)
SELECT r.id, u.telegram_id, CURRENT_DATE, 'morning'
FROM rooms r, users u
WHERE r.name IN ('101','102','103')
  AND u.name = 'Maria';
```

## Code structure

```
m4d-coso/
├── main.go      — wires admin pool, UserRegistry, agent options, system prompt dispatch
├── schema.go    — ensureSchema(): tables, functions, RLS policies, grant repair loop
├── users.go     — UserRegistry: per-user Postgres role creation, pool management
├── tools.go     — execute_sql tool: SELECT → rows table, DML → rows affected
├── prompt.go    — role-specific system prompts (manager vs cleaner)
├── go.mod
└── .env.example
```

### Key design decisions

**Why `session_user` instead of `current_user` in `current_telegram_id()`?**  
SECURITY DEFINER functions run as their owner (`postgres`). Inside them,
`current_user` is `postgres`, not the caller — so a role lookup would always
return null. `session_user` always reflects the login role regardless of
SECURITY DEFINER or SET ROLE.

**Why `USING(false)` on `user_credentials`?**  
Defense-in-depth. The table has no GRANT to `tg_*` roles (first layer), but if
a GRANT were accidentally added, the RLS policy still blocks all access from any
non-superuser role.

**Why `WITH CHECK` on assignment UPDATE?**  
`USING` controls which rows are *visible* for an UPDATE. Without `WITH CHECK`,
a cleaner could update their own assignment but change `cleaner_id` to someone
else — the post-update row would no longer belong to them, bypassing the policy.
`WITH CHECK` enforces the same predicate on the *resulting* row.

**Why a grant repair loop in `ensureSchema`?**  
Postgres roles are cluster-scoped (not per-database). If the bot starts with an
empty DB, the admin role might already exist from a previous run, but the tables
are new — any grants issued when the tables didn't exist were no-ops. The loop
re-issues all grants to every `tg_*` role on every startup, making it safe to
wipe and recreate the database schema.

**Why `execute_sql` instead of typed tools?**  
A hotel schema has many query patterns (reports, bulk assignments, filters by
date/floor/shift, etc.). Typed tools would need one per query shape. A single
`execute_sql` tool lets the LLM compose arbitrary SQL; RLS enforces safety
boundaries, so the only risk is data the user is already authorized to touch.

## Future: WASM migration

The agent currently runs as a native `linux/arm64` binary because `pgx` requires
real TCP sockets, which are not available in Go's `wasip1` target
(`net/net_fake.go` stubs out all networking).

The planned migration path is the **host bridge pattern**:

```
WASM agent
  └── go:wasmimport "host" "fetch"  →  host (wazero)  →  PostgREST HTTP  →  Postgres
```

PostgREST would replace the per-user pool approach:
- JWT contains `telegram_id` as the `sub` claim and the Postgres role as `role`
- PostgREST sets `SET LOCAL ROLE tg_<id>` per-request (same as `set_config('role', ..., true)`)
- `auth.uid()` / `current_telegram_id()` still work via GUC — same RLS policies apply

See [m4dtimes SDK docs](https://github.com/dmorn/m4dtimes/tree/main/sdk) for the
host bridge implementation roadmap.
