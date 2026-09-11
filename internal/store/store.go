package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"app/internal/llm"
)

// retention caps how many messages are kept per conversation to bound growth.
const retention = 200

// Store persists chat history in a single SQLite file, so conversations
// survive bot restarts. History is scoped per conversation: the pair of
// (user_id, channel_id). DMs and each server channel or thread therefore
// keep their own independent context.
type Store struct {
	db *sql.DB
}

// New opens (creating if needed) the SQLite database at path and
// ensures the schema exists, migrating older per-user schemas in place.
func New(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; avoids lock contention

	// Tables first; the composite index is created after migrate() so it
	// never references a column that an old database doesn't have yet.
	schema := `
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    TEXT    NOT NULL,
	channel_id TEXT    NOT NULL DEFAULT '',
	role       TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
	content    TEXT    NOT NULL,
	created_at TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS summaries (
	user_id    TEXT NOT NULL,
	channel_id TEXT NOT NULL DEFAULT '',
	summary    TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (user_id, channel_id)
);
CREATE TABLE IF NOT EXISTS reminders (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    TEXT    NOT NULL,
	channel_id TEXT    NOT NULL,
	message    TEXT    NOT NULL,
	due_at     INTEGER NOT NULL, -- unix seconds (UTC)
	created_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders (due_at);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate upgrades databases created before history was scoped per
// conversation. It is idempotent and safe to run on every startup.
func migrate(db *sql.DB) error {
	// messages: add channel_id if the table predates per-conversation scoping.
	if ok, err := hasColumn(db, "messages", "channel_id"); err != nil {
		return fmt.Errorf("inspect messages: %w", err)
	} else if !ok {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN channel_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add messages.channel_id: %w", err)
		}
	}

	// summaries: rebuild with a composite (user_id, channel_id) primary key.
	if ok, err := hasColumn(db, "summaries", "channel_id"); err != nil {
		return fmt.Errorf("inspect summaries: %w", err)
	} else if !ok {
		stmts := []string{
			`CREATE TABLE IF NOT EXISTS summaries_migrated (
				user_id    TEXT NOT NULL,
				channel_id TEXT NOT NULL DEFAULT '',
				summary    TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (user_id, channel_id)
			)`,
			`INSERT OR IGNORE INTO summaries_migrated SELECT user_id, '', summary, updated_at FROM summaries`,
			`DROP TABLE summaries`,
			`ALTER TABLE summaries_migrated RENAME TO summaries`,
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("migrate summaries: %w", err)
			}
		}
	}

	// Replace the legacy single-column index with the composite one.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_messages_user_id`); err != nil {
		return fmt.Errorf("drop legacy index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages (user_id, channel_id, id)`); err != nil {
		return fmt.Errorf("create composite index: %w", err)
	}
	return nil
}

// hasColumn reports whether table has a column named col.
func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("pragma: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan pragma: %w", err)
		}
		if name == col {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate pragma: %w", err)
	}
	return false, nil
}

// Append records one message for the conversation and prunes the oldest so
// each conversation keeps at most `retention` rows.
func (s *Store) Append(userID, channelID, role, content string) error {
	if _, err := s.db.Exec(
		`INSERT INTO messages (user_id, channel_id, role, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, channelID, role, content, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	_, err := s.db.Exec(`
		DELETE FROM messages
		WHERE user_id = ? AND channel_id = ? AND id NOT IN (
			SELECT id FROM messages WHERE user_id = ? AND channel_id = ? ORDER BY id DESC LIMIT ?
		)`, userID, channelID, userID, channelID, retention)
	if err != nil {
		return fmt.Errorf("prune old messages: %w", err)
	}
	return nil
}

// Recent returns the conversation's last n messages in chronological order.
func (s *Store) Recent(userID, channelID string, n int) ([]llm.Message, error) {
	if n <= 0 {
		n = 20
	}
	rows, err := s.db.Query(
		`SELECT role, content FROM messages WHERE user_id = ? AND channel_id = ? ORDER BY id DESC LIMIT ?`,
		userID, channelID, n,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent: %w", err)
	}
	defer rows.Close()

	var msgs []llm.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		msgs = append(msgs, llm.Message{Role: llm.Role(role), Content: content})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	// Query returned newest-first; reverse to chronological.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// Clear wipes one conversation's raw history (its summary is untouched).
func (s *Store) Clear(userID, channelID string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE user_id = ? AND channel_id = ?`, userID, channelID); err != nil {
		return fmt.Errorf("clear history: %w", err)
	}
	return nil
}

// StoredMessage is a raw history row, including its id for targeted deletes.
type StoredMessage struct {
	ID      int64
	Role    llm.Role
	Content string
}

// GetSummary returns the conversation's running summary ("" if none).
func (s *Store) GetSummary(userID, channelID string) (string, error) {
	var summary string
	err := s.db.QueryRow(`SELECT summary FROM summaries WHERE user_id = ? AND channel_id = ?`, userID, channelID).Scan(&summary)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get summary: %w", err)
	}
	return summary, nil
}

// Stats returns how many messages the conversation has stored and when the
// most recent one was written (zero time if none).
func (s *Store) Stats(userID, channelID string) (int, time.Time, error) {
	var count int
	var last *string
	err := s.db.QueryRow(`SELECT COUNT(*), MAX(created_at) FROM messages WHERE user_id = ? AND channel_id = ?`, userID, channelID).Scan(&count, &last)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("stats: %w", err)
	}
	if last == nil {
		return count, time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, *last)
	if err != nil {
		return count, time.Time{}, nil
	}
	return count, t, nil
}

// Compactable returns the oldest messages of a conversation eligible for
// compaction: everything except the newest keepRecent, capped at max rows,
// in chronological order.
func (s *Store) Compactable(userID, channelID string, keepRecent, max int) ([]StoredMessage, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id = ? AND channel_id = ?`, userID, channelID).Scan(&total); err != nil {
		return nil, fmt.Errorf("count messages: %w", err)
	}
	limit := total - keepRecent
	if limit <= 0 {
		return nil, nil
	}
	if limit > max {
		limit = max
	}
	rows, err := s.db.Query(
		`SELECT id, role, content FROM messages WHERE user_id = ? AND channel_id = ? ORDER BY id ASC LIMIT ?`,
		userID, channelID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select compactable: %w", err)
	}
	defer rows.Close()

	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var role string
		if err := rows.Scan(&m.ID, &role, &m.Content); err != nil {
			return nil, fmt.Errorf("scan compactable: %w", err)
		}
		m.Role = llm.Role(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate compactable: %w", err)
	}
	return out, nil
}

// CommitCompaction atomically stores the new summary and deletes exactly the
// compacted rows (by id), so a crash can never lose or double-count history.
func (s *Store) CommitCompaction(userID, channelID, summary string, ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO summaries (user_id, channel_id, summary, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET summary = excluded.summary, updated_at = excluded.updated_at`,
		userID, channelID, summary, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("upsert summary: %w", err)
	}

	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, userID, channelID) // first placeholders are user_id, channel_id
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	if _, err := tx.Exec(
		`DELETE FROM messages WHERE user_id = ? AND channel_id = ? AND id IN (`+strings.Join(ph, ", ")+`)`, args...,
	); err != nil {
		return fmt.Errorf("delete compacted rows: %w", err)
	}
	return tx.Commit()
}

// Reminder is a scheduled ping for one user in one channel. Reminders
// survive bot restarts; the reminder worker delivers them when due.
type Reminder struct {
	ID        int64
	UserID    string
	ChannelID string
	Message   string
	DueAt     time.Time
}

// AddReminder stores a new reminder and returns its id.
func (s *Store) AddReminder(userID, channelID, message string, dueAt time.Time) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO reminders (user_id, channel_id, message, due_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, channelID, message, dueAt.UTC().Unix(), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("insert reminder: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reminder id: %w", err)
	}
	return id, nil
}

// DueReminders returns all reminders whose due time has passed, soonest first.
func (s *Store) DueReminders(now time.Time) ([]Reminder, error) {
	return s.queryReminders(
		`SELECT id, user_id, channel_id, message, due_at FROM reminders WHERE due_at <= ? ORDER BY due_at ASC`,
		now.UTC().Unix(),
	)
}

// PendingForUser lists a user's not-yet-delivered reminders across all
// channels, soonest first.
func (s *Store) PendingForUser(userID string) ([]Reminder, error) {
	return s.queryReminders(
		`SELECT id, user_id, channel_id, message, due_at FROM reminders WHERE user_id = ? ORDER BY due_at ASC`,
		userID,
	)
}

// DeleteReminder removes a delivered (or stale) reminder.
func (s *Store) DeleteReminder(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM reminders WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	return nil
}

func (s *Store) queryReminders(query string, args ...any) ([]Reminder, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query reminders: %w", err)
	}
	defer rows.Close()

	var out []Reminder
	for rows.Next() {
		var r Reminder
		var due int64
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChannelID, &r.Message, &due); err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		r.DueAt = time.Unix(due, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reminders: %w", err)
	}
	return out, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
