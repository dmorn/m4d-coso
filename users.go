package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Role string

const (
	RoleManager Role = "manager"
	RoleCleaner Role = "cleaner"
)

// UserRegistry manages per-user Postgres credentials and connection pools.
type UserRegistry struct {
	adminPool *pgxpool.Pool
	dbURL     string
	mu        sync.Mutex
	pools     map[int64]*pgxpool.Pool
}

func newUserRegistry(adminPool *pgxpool.Pool, dbURL string) *UserRegistry {
	return &UserRegistry{
		adminPool: adminPool,
		dbURL:     dbURL,
		pools:     make(map[int64]*pgxpool.Pool),
	}
}

// Pool returns the per-user connection pool. Opens it on first call.
func (r *UserRegistry) Pool(ctx context.Context, telegramID int64) (*pgxpool.Pool, error) {
	r.mu.Lock()
	if p, ok := r.pools[telegramID]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()

	var pgUser, pgPassword string
	err := r.adminPool.QueryRow(ctx,
		`SELECT u.pg_user, c.pg_password
		 FROM users u JOIN user_credentials c USING (telegram_id)
		 WHERE u.telegram_id = $1`, telegramID,
	).Scan(&pgUser, &pgPassword)
	if err != nil {
		return nil, fmt.Errorf("user %d not registered", telegramID)
	}

	pool, err := r.openUserPool(ctx, pgUser, pgPassword)
	if err != nil {
		return nil, fmt.Errorf("open pool for user %d: %w", telegramID, err)
	}

	r.mu.Lock()
	r.pools[telegramID] = pool
	r.mu.Unlock()
	return pool, nil
}

// Register creates a Postgres role and registers the user.
func (r *UserRegistry) Register(ctx context.Context, telegramID int64, role Role, name string) error {
	pgUser := fmt.Sprintf("tg_%d", telegramID)
	pgPassword, err := randomPassword()
	if err != nil {
		return fmt.Errorf("generate password: %w", err)
	}

	// Create or update the Postgres LOGIN role
	_, err = r.adminPool.Exec(ctx, fmt.Sprintf(
		`DO $$ BEGIN
			CREATE ROLE %s LOGIN PASSWORD '%s';
		EXCEPTION WHEN duplicate_object THEN
			ALTER ROLE %s LOGIN PASSWORD '%s';
		END $$`, pgUser, pgPassword, pgUser, pgPassword))
	if err != nil {
		return fmt.Errorf("create role: %w", err)
	}

	// Base grants for all users
	grants := []string{
		fmt.Sprintf(`GRANT CONNECT ON DATABASE m4dtimes TO %s`, pgUser),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, pgUser),
		// Tables: RLS policies will restrict what they can actually do
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON rooms TO %s`, pgUser),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON assignments TO %s`, pgUser),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON users TO %s`, pgUser),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, pgUser),
	}
	for _, g := range grants {
		if _, err := r.adminPool.Exec(ctx, g); err != nil {
			log.Printf("warn: grant for %s: %v", pgUser, err)
		}
	}

	// Upsert into users table
	_, err = r.adminPool.Exec(ctx,
		`INSERT INTO users (telegram_id, pg_user, name, role)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (telegram_id) DO UPDATE SET pg_user=$2, name=$3, role=$4`,
		telegramID, pgUser, name, string(role),
	)
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	// Upsert credentials
	_, err = r.adminPool.Exec(ctx,
		`INSERT INTO user_credentials (telegram_id, pg_password)
		 VALUES ($1, $2)
		 ON CONFLICT (telegram_id) DO UPDATE SET pg_password=$2`,
		telegramID, pgPassword,
	)
	if err != nil {
		return fmt.Errorf("upsert credentials: %w", err)
	}

	log.Printf("registered user %d (%s) as %s role=%s", telegramID, name, pgUser, role)
	return nil
}

// IsRegistered returns true if the user exists in the DB.
func (r *UserRegistry) IsRegistered(ctx context.Context, telegramID int64) bool {
	var exists bool
	r.adminPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE telegram_id=$1)`, telegramID,
	).Scan(&exists)
	return exists
}

func (r *UserRegistry) openUserPool(ctx context.Context, pgUser, pgPassword string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(r.dbURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.User = pgUser
	cfg.ConnConfig.Password = pgPassword
	cfg.MaxConns = 3

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping as %s: %w", pgUser, err)
	}
	return pool, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
