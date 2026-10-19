// Package store persists hosts, settings and the message log in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Host is an allowed client. Match is an IP address, a CIDR, or a DNS name
// that is resolved when clients connect, or a comma-separated list of those
// (a service that sends from several addresses).
type Host struct {
	ID     int64
	Name   string
	Match  string
	Sender string // mailbox used to send through Graph
	// Optional SMTP logins. A host with logins must match by address *and* log in with one of
	// them (over TLS); a host without logins is allowed by its address alone.
	Logins      []Login
	RewriteFrom bool // replace the From header address with the sending mailbox
	Enabled     bool
	Note        string
	CreatedAt   time.Time
}

// Login is one SMTP username and password of a host rule. Several logins on the same rule (or on
// rules for the same address) let one machine send as different mailboxes depending on the login.
type Login struct {
	ID       int64
	HostID   int64
	Username string
	PassHash string // bcrypt
	Sender   string // mailbox for this login; empty = the host's mailbox
}

// Matches returns the individual entries of Match.
func (h *Host) Matches() []string { return SplitMatch(h.Match) }

// RequiresLogin reports whether clients of this host must log in.
func (h *Host) RequiresLogin() bool { return len(h.Logins) > 0 }

// SenderFor returns the mailbox a client of this host sends as: the login's own mailbox if it
// has one, otherwise the host's. l may be nil (not logged in).
func (h *Host) SenderFor(l *Login) string {
	if l != nil && l.Sender != "" {
		return l.Sender
	}
	return h.Sender
}

// SplitMatch splits a host match value into its entries. Commas, semicolons
// and any whitespace separate entries.
func SplitMatch(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

type LogEntry struct {
	ID         int64
	Time       time.Time
	ClientIP   string
	Port       string
	HostID     int64
	HostName   string
	Sender     string
	EnvFrom    string
	Recipients string
	Subject    string
	Size       int64
	Status     string // "sent", "rejected", "failed"
	Error      string
	DurationMS int64
}

type Stats struct {
	Sent24h, Failed24h, Rejected24h int
}

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrateBaseline is schema version 1: everything up to and including the first release with a
// schema version. It is idempotent, because databases from before versioning (user_version 0)
// already contain some or all of it.
func (s *Store) migrateBaseline() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS hosts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	match        TEXT NOT NULL UNIQUE,
	sender       TEXT NOT NULL,
	rewrite_from INTEGER NOT NULL DEFAULT 1,
	enabled      INTEGER NOT NULL DEFAULT 1,
	note         TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          INTEGER NOT NULL,
	client_ip   TEXT NOT NULL,
	port        TEXT NOT NULL,
	host_id     INTEGER NOT NULL DEFAULT 0,
	host_name   TEXT NOT NULL DEFAULT '',
	sender      TEXT NOT NULL DEFAULT '',
	env_from    TEXT NOT NULL DEFAULT '',
	recipients  TEXT NOT NULL DEFAULT '',
	subject     TEXT NOT NULL DEFAULT '',
	size        INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS message_log_ts ON message_log(ts);
`)
	if err != nil {
		return err
	}
	// Columns added after the first release.
	for _, col := range []string{
		`smtp_user TEXT NOT NULL DEFAULT ''`,
		`smtp_pass_hash TEXT NOT NULL DEFAULT ''`,
	} {
		if err := s.addColumn("hosts", col); err != nil {
			return err
		}
	}
	if err := s.migrateConns(); err != nil {
		return err
	}
	// Databases from before the allowed-senders list: seed it from the hosts' current
	// mailboxes, so upgrading doesn't block mail that works today.
	if _, err := s.GetSetting(keyAllowedSenders); errors.Is(err, ErrNotFound) {
		rows, err := s.db.Query(`SELECT DISTINCT sender FROM hosts`)
		if err != nil {
			return err
		}
		var seed []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			seed = append(seed, v)
		}
		rows.Close()
		return s.SetAllowedSenders(seed)
	}
	return nil
}

// addColumn adds a column definition to table unless a column of that name exists.
func (s *Store) addColumn(table, def string) error {
	name := strings.Fields(def)[0]
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		if n == name {
			return nil
		}
	}
	rows.Close()
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + def)
	return err
}

// --- allowed sender mailboxes ---

const keyAllowedSenders = "allowed_senders"

// NormalizeSenders lower-cases, trims, de-duplicates and sorts mailbox addresses.
func NormalizeSenders(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// AllowedSenders returns the mailboxes host rules may send as, sorted.
func (s *Store) AllowedSenders() ([]string, error) {
	v, err := s.GetSetting(keyAllowedSenders)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return NormalizeSenders(strings.Split(v, "\n")), nil
}

func (s *Store) SetAllowedSenders(list []string) error {
	return s.SetSetting(keyAllowedSenders, strings.Join(NormalizeSenders(list), "\n"))
}

// SenderAllowed reports whether mailbox is on the allowed list (case-insensitive).
func (s *Store) SenderAllowed(mailbox string) (bool, error) {
	list, err := s.AllowedSenders()
	if err != nil {
		return false, err
	}
	_, found := slices.BinarySearch(list, strings.ToLower(strings.TrimSpace(mailbox)))
	return found, nil
}

// --- settings ---

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// --- hosts ---

const hostCols = `id, name, match, sender, rewrite_from, enabled, note, created_at`

func scanHost(sc interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var created int64
	err := sc.Scan(&h.ID, &h.Name, &h.Match, &h.Sender, &h.RewriteFrom, &h.Enabled, &h.Note, &created)
	h.CreatedAt = time.Unix(created, 0)
	return h, err
}

func (s *Store) ListHosts() ([]Host, error) {
	rows, err := s.db.Query(`SELECT ` + hostCols + ` FROM hosts ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	logins, err := s.logins(`SELECT id, host_id, username, pass_hash, sender FROM host_logins ORDER BY username`)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Logins = logins[out[i].ID]
	}
	return out, nil
}

func (s *Store) GetHost(id int64) (Host, error) {
	h, err := scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	logins, err := s.logins(`SELECT id, host_id, username, pass_hash, sender FROM host_logins WHERE host_id = ? ORDER BY username`, id)
	h.Logins = logins[id]
	return h, err
}

// logins runs a host_logins query and groups the rows by host.
func (s *Store) logins(query string, args ...any) (map[int64][]Login, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Login{}
	for rows.Next() {
		var l Login
		if err := rows.Scan(&l.ID, &l.HostID, &l.Username, &l.PassHash, &l.Sender); err != nil {
			return nil, err
		}
		out[l.HostID] = append(out[l.HostID], l)
	}
	return out, rows.Err()
}

// SaveHost inserts or updates a host together with its logins: logins with ID 0 are added,
// existing ones are updated, and logins of the host that are no longer in h.Logins are removed.
// Everything happens in one transaction.
func (s *Store) SaveHost(h *Host) (err error) {
	// IDs assigned inside a transaction that is rolled back must not stick, or the next save of
	// the same form would update rows that don't exist.
	oldID, oldLogins := h.ID, slices.Clone(h.Logins)
	defer func() {
		if err != nil {
			h.ID, h.Logins = oldID, oldLogins
		}
	}()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if h.ID == 0 {
		res, err := tx.Exec(`INSERT INTO hosts(name, match, sender, rewrite_from, enabled, note, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`,
			h.Name, h.Match, h.Sender, h.RewriteFrom, h.Enabled, h.Note, time.Now().Unix())
		if err != nil {
			return friendly(err)
		}
		if h.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`UPDATE hosts SET name = ?, match = ?, sender = ?, rewrite_from = ?, enabled = ?,
		note = ? WHERE id = ?`, h.Name, h.Match, h.Sender, h.RewriteFrom, h.Enabled, h.Note, h.ID); err != nil {
		return friendly(err)
	}

	keep := []any{h.ID}
	marks := []string{}
	for _, l := range h.Logins {
		if l.ID != 0 {
			keep = append(keep, l.ID)
			marks = append(marks, "?")
		}
	}
	del := `DELETE FROM host_logins WHERE host_id = ?`
	if len(marks) > 0 {
		del += ` AND id NOT IN (` + strings.Join(marks, ",") + `)`
	}
	if _, err := tx.Exec(del, keep...); err != nil {
		return err
	}
	for i := range h.Logins {
		l := &h.Logins[i]
		l.HostID = h.ID
		if l.ID == 0 {
			res, err := tx.Exec(`INSERT INTO host_logins(host_id, username, pass_hash, sender, created_at)
				VALUES(?, ?, ?, ?, ?)`, h.ID, l.Username, l.PassHash, l.Sender, time.Now().Unix())
			if err != nil {
				return friendly(err)
			}
			if l.ID, err = res.LastInsertId(); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE host_logins SET username = ?, pass_hash = ?, sender = ? WHERE id = ? AND host_id = ?`,
			l.Username, l.PassHash, l.Sender, l.ID, h.ID); err != nil {
			return friendly(err)
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteHost(id int64) error {
	_, err := s.db.Exec(`DELETE FROM hosts WHERE id = ?`, id)
	return err
}

func friendly(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "UNIQUE constraint failed: hosts.match"):
		return errors.New("another host already uses this address / network / hostname")
	case strings.Contains(err.Error(), "UNIQUE constraint failed: host_logins"):
		return errors.New("this host already has a login with that username")
	}
	return err
}

// --- message log ---

func (s *Store) AddLog(e *LogEntry) error {
	_, err := s.db.Exec(`INSERT INTO message_log(ts, client_ip, port, host_id, host_name, sender, env_from,
		recipients, subject, size, status, error, duration_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Time.Unix(), e.ClientIP, e.Port, e.HostID, e.HostName, e.Sender, e.EnvFrom,
		e.Recipients, e.Subject, e.Size, e.Status, e.Error, e.DurationMS)
	return err
}

type LogFilter struct {
	Status string
	Query  string // matches IP, host, sender, from, recipients, subject
	Limit  int
	Offset int
}

func (s *Store) ListLog(f LogFilter) ([]LogEntry, error) {
	where := []string{"1=1"}
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		where = append(where, `(client_ip LIKE ? OR host_name LIKE ? OR sender LIKE ? OR env_from LIKE ?
			OR recipients LIKE ? OR subject LIKE ?)`)
		q := "%" + f.Query + "%"
		args = append(args, q, q, q, q, q, q)
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.Query(`SELECT id, ts, client_ip, port, host_id, host_name, sender, env_from, recipients,
		subject, size, status, error, duration_ms FROM message_log WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.ClientIP, &e.Port, &e.HostID, &e.HostName, &e.Sender, &e.EnvFrom,
			&e.Recipients, &e.Subject, &e.Size, &e.Status, &e.Error, &e.DurationMS); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	since := time.Now().Add(-24 * time.Hour).Unix()
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(status = 'sent'), 0),
		COALESCE(SUM(status = 'failed'), 0),
		COALESCE(SUM(status = 'rejected'), 0)
		FROM message_log WHERE ts >= ?`, since).Scan(&st.Sent24h, &st.Failed24h, &st.Rejected24h)
	return st, err
}

func (s *Store) PruneLog(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM message_log WHERE ts < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
