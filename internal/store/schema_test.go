package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests are the tripwire that keeps upgrades safe: any change to the schema fails them
// until it is expressed as a new migration, and every schema that was ever released must still
// upgrade to the current one.

const goldenPath = "testdata/schema.golden"

// dumpSchema returns the schema of a database in a stable text form.
func dumpSchema(t *testing.T, s *Store) string {
	t.Helper()
	v, err := s.Version()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "-- schema version %d\n", v)
	for rows.Next() {
		var typ, name, stmt string
		if err := rows.Scan(&typ, &name, &stmt); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n-- %s %s\n%s;\n", typ, name, strings.TrimSpace(stmt))
	}
	return b.String()
}

var goldenVersionRe = regexp.MustCompile(`^-- schema version (\d+)`)

func goldenVersion(text string) int {
	m := goldenVersionRe.FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

const schemaHelp = `
The database schema changed. To make this pass:
  1. Do NOT edit an existing migration. Append a new one to the migrations list in
     internal/store/migrations.go (databases in the field have already run the old ones).
  2. Keep the previous schema as a fixture, so the upgrade from it stays tested:
       cp internal/store/testdata/schema.golden internal/store/testdata/history/schema-v<old version>.sql
  3. Regenerate the golden file:
       UPDATE_GOLDEN=1 go test ./internal/store -run TestSchemaMatchesGolden
  4. If the change also needs something on the host (files, firewall, config), add a host
     migration under deploy/migrations/ as well.`

func TestSchemaMatchesGolden(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := dumpSchema(t, s)
	old, _ := os.ReadFile(goldenPath)

	if os.Getenv("UPDATE_GOLDEN") != "" {
		// Refuse to bless a changed schema under an unchanged version: that means an existing
		// migration was edited instead of a new one being added.
		if len(old) > 0 && string(old) != got && goldenVersion(string(old)) == SchemaVersion() {
			t.Fatalf("the schema changed but the schema version is still %d: an existing migration was modified.%s", SchemaVersion(), schemaHelp)
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if string(old) != got {
		t.Fatalf("schema differs from %s.%s\n\n--- got ---\n%s", goldenPath, schemaHelp, got)
	}
	if goldenVersion(got) != SchemaVersion() {
		t.Fatalf("fresh database is at version %d, program expects %d", goldenVersion(got), SchemaVersion())
	}
}

var fixtureRe = regexp.MustCompile(`schema-v(\d+)\.sql$`)

// Every schema ever released must upgrade to exactly the current schema, with its data intact.
func TestEveryReleasedSchemaUpgrades(t *testing.T) {
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, _ := filepath.Glob("testdata/history/schema-v*.sql")
	seen := map[int]bool{}
	for _, f := range fixtures {
		m := fixtureRe.FindStringSubmatch(f)
		if m == nil {
			t.Fatalf("unexpected fixture name %s", f)
		}
		version, _ := strconv.Atoi(m[1])
		seen[version] = true
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(raw)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// A host row and a log row that must survive the upgrade.
		if _, err := db.Exec(`INSERT INTO hosts(name, match, sender, created_at) VALUES('kept', '192.0.2.1', 'a@contoso.com', 1);
			INSERT INTO message_log(ts, client_ip, port, status) VALUES(1, '192.0.2.1', '25', 'sent');`); err != nil {
			t.Fatalf("%s: seed data: %v", f, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			t.Fatal(err)
		}
		db.Close()

		s, err := Open(path)
		if err != nil {
			t.Fatalf("upgrade from schema v%d: %v", version, err)
		}
		if got := dumpSchema(t, s); got != string(want) {
			t.Errorf("upgrade from schema v%d does not give the current schema:\n%s", version, got)
		}
		if h, err := s.GetHost(1); err != nil || h.Name != "kept" {
			t.Errorf("upgrade from schema v%d lost the host: %+v %v", version, h, err)
		}
		if logs, _ := s.ListLog(LogFilter{}); len(logs) != 1 {
			t.Errorf("upgrade from schema v%d lost the message log", version)
		}
		s.Close()
	}
	// Every version below the current one needs a fixture (v0 = before schema versions existed).
	for v := 0; v < SchemaVersion(); v++ {
		if !seen[v] {
			t.Errorf("no fixture testdata/history/schema-v%d.sql: the upgrade from that schema is untested", v)
		}
	}
}

// A database written by a newer release must not be opened by an older one.
func TestNewerSchemaIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion()+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this release") {
		t.Fatalf("opened a database from a newer release: %v", err)
	}
}
