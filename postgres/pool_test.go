package postgres

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConfigDSN(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		host     string
		port     uint16
		user     string
		password string
		database string
		noSSL    bool
	}{
		{
			name: "reserved characters in credentials and database",
			cfg:  Config{Host: "db", User: "app/user", Password: "p@ss/w:rd#1%?", Database: "my?db#1"},
			host: "db", port: 5432, user: "app/user", password: "p@ss/w:rd#1%?", database: "my?db#1", noSSL: true,
		},
		{
			name: "ipv6 host with explicit port",
			cfg:  Config{Host: "::1", Port: 6543, User: "u", Password: "p", Database: "d"},
			host: "::1", port: 6543, user: "u", password: "p", database: "d", noSSL: true,
		},
		{
			name: "explicit sslmode is kept",
			cfg:  Config{Host: "db", User: "u", Database: "d", SSLMode: "verify-full"},
			host: "db", port: 5432, user: "u", database: "d",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := tc.cfg.DSN()
			if tc.noSSL && strings.Contains(dsn, "sslmode") {
				t.Errorf("DSN %q sets sslmode although SSLMode is empty", dsn)
			}
			if tc.cfg.SSLMode != "" && !strings.Contains(dsn, "sslmode="+tc.cfg.SSLMode) {
				t.Errorf("DSN %q lost sslmode=%s", dsn, tc.cfg.SSLMode)
			}
			parsed, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("ParseConfig(%q): %v", dsn, err)
			}
			cc := parsed.ConnConfig
			if cc.Host != tc.host || cc.Port != tc.port || cc.User != tc.user || cc.Password != tc.password || cc.Database != tc.database {
				t.Errorf("parsed host=%q port=%d user=%q password=%q database=%q", cc.Host, cc.Port, cc.User, cc.Password, cc.Database)
			}
		})
	}
}
