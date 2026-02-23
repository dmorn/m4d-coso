# m4d-coso

Hotel Cimon management agent. Built on [m4dtimes SDK](https://github.com/dmorn/m4dtimes).

> **Note:** Currently builds for native `linux/arm64`. pgx requires real TCP sockets,
> which are not available in `wasip1`. The wasip1 migration path is the host bridge pattern
> (PostgREST via `go:wasmimport "host" "fetch"`). See [m4dtimes SDK docs](https://github.com/dmorn/m4dtimes/tree/main/sdk).

## Tools

| Tool | Description |
|------|-------------|
| `list_rooms` | List all rooms with status and notes |
| `set_occupied` | Mark a room occupied or free |
| `add_room` | Add a new room |
| `add_note` | Add/update a note on a room |

## Setup

```bash
cp .env.example .env
# edit .env with your credentials

go build -o m4d-coso .
./m4d-coso
```

## Config

| Env | Default | Description |
|-----|---------|-------------|
| `TELEGRAM_BOT_TOKEN` | required | Bot token from @BotFather |
| `LLM_API_KEY` | required | Anthropic API key |
| `DATABASE_URL` | `postgresql://postgres:devpassword@localhost:5432/m4dtimes` | Postgres connection string |
| `HOTEL_NAME` | `Hotel Cimon` | Hotel name shown in system prompt |
