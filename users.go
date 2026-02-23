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

// UserRegistry manages per-user Postgres credentials and connection pools.
// Each Telegram user gets their own Postgres role; the agent connects with
// that role's credentials so RLS + CURRENT_USER-based policies apply automatically.
type UserRegistry struct {
	adminPool *pgxpool.Pool  // superuser — only used for DDL (CREATE ROLE, INSERT users)
	dbURL     string         // base URL, used to build per-user DSNs
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

// Pool returns the connection pool for a Telegram user.
// If the user doesn't exist yet, ErrNotRegistered is returned.
func (r *UserRegistry) Pool(ctx context.Context, telegramID int64) (*pgxpool.Pool, error) {
	r.mu.Lock()
	if p, ok := r.pools[telegramID]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()

	// Look up pg_user from the users table (via admin pool)
	var pgUser string
	err := r.adminPool.QueryRow(ctx,
		`SELECT pg_user FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(&pgUser)
	if err != nil {
		return nil, fmt.Errorf("user %d not registered", telegramID)
	}

	// We don't store passwords — use trust auth via the admin pool by switching role
	// Actually we store the password in the pool config DSN at registration time.
	// Here we re-open the pool from env (each user's role is a Postgres LOGIN role).
	// For simplicity we store the DSN in a separate table. Let's fetch it.
	var pgPassword string
	err = r.adminPool.QueryRow(ctx,
		`SELECT pg_password FROM user_credentials WHERE telegram_id = $1`, telegramID,
	).Scan(&pgPassword)
	if err != nil {
		return nil, fmt.Errorf("credentials for user %d not found: %w", telegramID, err)
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

// Register creates a Postgres role for the given Telegram user and stores credentials.
// isAdmin grants elevated permissions (e.g. can see all rooms).
func (r *UserRegistry) Register(ctx context.Context, telegramID int64, isAdmin bool) error {
	pgUser := fmt.Sprintf("tg_%d", telegramID)
	pgPassword, err := randomPassword()
	if err != nil {
		return fmt.Errorf("generate password: %w", err)
	}

	// Create Postgres LOGIN role (ignore if already exists)
	_, err = r.adminPool.Exec(ctx,
		fmt.Sprintf(`DO $$ BEGIN
			CREATE ROLE %s LOGIN PASSWORD '%s';
		EXCEPTION WHEN duplicate_object THEN
			ALTER ROLE %s LOGIN PASSWORD '%s';
		END $$`, pgUser, pgPassword, pgUser, pgPassword))
	if err != nil {
		return fmt.Errorf("create role %s: %w", pgUser, err)
	}

	// Grant base permissions
	grants := []string{
		fmt.Sprintf(`GRANT CONNECT ON DATABASE m4dtimes TO %s`, pgUser),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, pgUser),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON rooms TO %s`, pgUser),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, pgUser),
		fmt.Sprintf(`GRANT SELECT ON users TO %s`, pgUser),
	}
	if isAdmin {
		grants = append(grants, fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA public TO %s`, pgUser))
	}
	for _, g := range grants {
		if _, err := r.adminPool.Exec(ctx, g); err != nil {
			log.Printf("grant for %s: %v", pgUser, err)
		}
	}

	// Store in users + credentials tables
	_, err = r.adminPool.Exec(ctx,
		`INSERT INTO users (telegram_id, pg_user, is_admin) VALUES ($1, $2, $3)
		 ON CONFLICT (telegram_id) DO UPDATE SET pg_user=$2, is_admin=$3`,
		telegramID, pgUser, isAdmin,
	)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}

	_, err = r.adminPool.Exec(ctx,
		`INSERT INTO user_credentials (telegram_id, pg_password) VALUES ($1, $2)
		 ON CONFLICT (telegram_id) DO UPDATE SET pg_password=$2`,
		telegramID, pgPassword,
	)
	if err != nil {
		return fmt.Errorf("insert credentials: %w", err)
	}

	log.Printf("registered user %d as %s (admin=%v)", telegramID, pgUser, isAdmin)
	return nil
}

// IsRegistered returns true if the user has credentials in the DB.
func (r *UserRegistry) IsRegistered(ctx context.Context, telegramID int64) bool {
	var exists bool
	r.adminPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE telegram_id=$1)`, telegramID,
	).Scan(&exists)
	return exists
}

func (r *UserRegistry) openUserPool(ctx context.Context, pgUser, pgPassword string) (*pgxpool.Pool, error) {
	// Build DSN from base URL, replacing user+password
	// Base URL format: postgresql://postgres:devpassword@host:port/db
	// We swap user+pass, keep host/port/db
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
