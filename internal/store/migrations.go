package store

import "fmt"

// Schema migrations.
//
// The schema version is the number of migrations applied, stored in SQLite's PRAGMA user_version.
// Opening a database applies the missing ones in order, so any older database is brought up to
// date by simply starting a newer release.
//
// Rules, enforced by the tests in schema_test.go:
//   - To change the schema, APPEND a migration. Never edit or reorder an existing one: databases
//     in the field have already run it.
//   - A migration must be safe to run again if the process dies half-way (use IF NOT EXISTS,
//     addColumn, or a transaction).
//   - After adding one, regenerate testdata/schema.golden and keep the previous golden as
//     testdata/history/schema-v<N>.sql (the test output says exactly what to do).
var migrations = []struct {
	name  string
	apply func(*Store) error
}{
	{"baseline: settings, hosts with SMTP login, message_log, conn_log, allowed senders", (*Store).migrateBaseline},
}

// SchemaVersion is the schema version this program writes.
func SchemaVersion() int { return len(migrations) }

// Version returns the schema version of the open database.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func (s *Store) migrate() error {
	v, err := s.Version()
	if err != nil {
		return err
	}
	if v > len(migrations) {
		// A newer release wrote this database. Running an older program against it could
		// corrupt data it doesn't know about.
		return fmt.Errorf("database schema version %d is newer than this release supports (%d); "+
			"install the newer release again, or restore the backup taken before the update", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if err := migrations[i].apply(s); err != nil {
			return fmt.Errorf("schema migration %d (%s): %w", i+1, migrations[i].name, err)
		}
		// PRAGMA doesn't take parameters; the value is an int we computed.
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return nil
}
