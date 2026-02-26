include .env
export

.PHONY: build run db-apply db-diff db-inspect db-rls

build:
	go build -o m4d-coso .

run: build
	./m4d-coso

## ── Database ──────────────────────────────────────────────────────────────────

# Apply structural schema (Atlas) + RLS/functions (psql)
db-apply:
	atlas schema apply --env local --auto-approve
	psql $(DATABASE_URL) -f db/rls.sql

# Show what Atlas would change (dry run)
db-diff:
	atlas schema apply --env local --dry-run

# Dump live DB schema as SQL
db-inspect:
	atlas schema inspect --env local --format '{{ sql . }}'

# Apply only RLS/functions/grants (no structural changes)
db-rls:
	psql $(DATABASE_URL) -f db/rls.sql
