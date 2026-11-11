package deliverystate_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/flow/deliverystate"
)

func TestMemory(t *testing.T) { suite(t, deliverystate.NewMemory()) }
func TestPostgres(t *testing.T) {
	url := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if url == "" {
		t.Skip("set SAIGE_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("sinktest_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); pool.Close() })
	store := deliverystate.NewPostgres(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suite(t, store)
}
func suite(t *testing.T, store deliverystate.Store) {
	ctx := context.Background()
	key := deliverystate.Key{Tenant: "tenant", Scope: "client", Run: "run", Channel: "channel", Target: "target"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Update(ctx, key, func(data []byte) ([]byte, bool, error) {
				n := 0
				if len(data) > 0 {
					var err error
					n, err = strconv.Atoi(string(data))
					if err != nil {
						return nil, false, err
					}
				}
				return []byte(strconv.Itoa(n + 1)), true, nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assert := func(want string) {
		t.Helper()
		if err := store.Update(ctx, key, func(data []byte) ([]byte, bool, error) {
			if string(data) != want {
				t.Errorf("state=%q want=%q", data, want)
			}
			return data, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assert("20")
	keys, err := store.Pending(ctx, key.Scope, 1)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("pending=%v err=%v", keys, err)
	}
	failure := errors.New("surface failed")
	if err := store.Update(ctx, key, func(data []byte) ([]byte, bool, error) { data[0] = 'X'; return []byte("bad"), false, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	assert("20")
	for _, mutate := range []func(*deliverystate.Key){func(k *deliverystate.Key) { k.Tenant += "x" }, func(k *deliverystate.Key) { k.Scope += "x" }, func(k *deliverystate.Key) { k.Run += "x" }, func(k *deliverystate.Key) { k.Channel += "x" }, func(k *deliverystate.Key) { k.Target += "x" }} {
		isolated := key
		mutate(&isolated)
		if err := store.Update(ctx, isolated, func(data []byte) ([]byte, bool, error) {
			if len(data) != 0 {
				t.Error("scope leaked")
			}
			return []byte("new"), false, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Update(canceled, key, func([]byte) ([]byte, bool, error) { t.Error("called after cancellation"); return nil, false, nil }); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := store.Update(ctx, deliverystate.Key{}, func([]byte) ([]byte, bool, error) { t.Error("invalid key called callback"); return nil, false, nil }); err == nil {
		t.Fatal("invalid key accepted")
	}
}
