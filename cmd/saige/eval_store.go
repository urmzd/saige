package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urmzd/saige/eval/pgstore"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

// isPostgresStore reports whether a --store value is a PostgreSQL URL rather
// than a directory.
func isPostgresStore(spec string) bool {
	return strings.HasPrefix(spec, "postgres://") || strings.HasPrefix(spec, "postgresql://")
}

// openEvalStore opens a results store named by --store: a PostgreSQL URL,
// migrated on connect and scoped to tenant, or a directory. A directory is
// created when create is set and must exist otherwise. close releases the
// store's resources.
func openEvalStore(ctx context.Context, spec, tenant string, create bool) (s store.Store, close func(), err error) {
	if spec == "" {
		return nil, nil, fmt.Errorf("--store is required")
	}
	if isPostgresStore(spec) {
		pool, err := connectPostgres(ctx, spec)
		if err != nil {
			return nil, nil, fmt.Errorf("results store: %w", err)
		}
		pg, err := pgstore.New(ctx, pool, tenant)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		return pg, pool.Close, nil
	}
	if tenant != "" {
		return nil, nil, fmt.Errorf("--tenant needs a PostgreSQL results store")
	}
	if !create {
		if _, err := os.Stat(spec); err != nil {
			return nil, nil, fmt.Errorf("results store %s: %w", spec, err)
		}
	}
	fs, err := filestore.Open(spec)
	if err != nil {
		return nil, nil, err
	}
	return fs, func() {}, nil
}

const (
	storeFlagUsage  = "Results store: a directory, or a PostgreSQL URL (postgres://...) migrated on connect"
	tenantFlagUsage = "Tenant scope within a PostgreSQL results store"
)
