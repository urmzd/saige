// Package postgres provides shared PostgreSQL connection management.
package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// Config holds PostgreSQL connection settings.
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	SSLMode  string
	MaxConns int32
	URL      string // if set, used directly (overrides individual fields)
}

// DSN builds a postgres:// URL from the individual fields, escaping the user,
// password and database name and bracketing IPv6 hosts. Port defaults to
// 5432. An empty SSLMode leaves sslmode out, so pgx uses its default
// ("prefer": TLS when the server offers it, without certificate
// verification). Set SSLMode to "verify-full" to require a verified server.
// URL, when set, is not consulted.
func (cfg Config) DSN() string {
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(port)),
		Path:   "/" + cfg.Database,
	}
	switch {
	case cfg.User != "" && cfg.Password != "":
		u.User = url.UserPassword(cfg.User, cfg.Password)
	case cfg.User != "":
		u.User = url.User(cfg.User)
	case cfg.Password != "":
		u.User = url.UserPassword("", cfg.Password)
	}
	if cfg.SSLMode != "" {
		u.RawQuery = url.Values{"sslmode": {cfg.SSLMode}}.Encode()
	}
	return u.String()
}

// NewPool creates a new pgxpool.Pool with pgvector types registered.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	dsn := cfg.URL
	if dsn == "" {
		dsn = cfg.DSN()
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}

	// Register pgvector types on each new connection.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return pool, nil
}
