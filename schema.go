package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS rooms (
	id        SERIAL PRIMARY KEY,
	name      TEXT NOT NULL UNIQUE,
	floor     INT NOT NULL DEFAULT 1,
	occupied  BOOLEAN NOT NULL DEFAULT FALSE,
	notes     TEXT
);
`

func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	return err
}
