package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"discord-bot/internal/llm"
)

// openRaw opens the SQLite file without running schema/migration logic,
// used to seed a database in an older schema shape.
func openRaw(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	return sql.Open("sqlite", dsn)
}

func TestAppendRecentClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	const user = "12345"
	const channel = "dm-1"
	for i := 1; i <= 5; i++ {
		if err := s.Append(user, channel, "user", "question "+string(rune('0'+i))); err != nil {
			t.Fatalf("Append user: %v", err)
		}
		if err := s.Append(user, channel, "assistant", "answer "+string(rune('0'+i))); err != nil {
			t.Fatalf("Append assistant: %v", err)
		}
	}

	msgs, err := s.Recent(user, channel, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(msgs) != 10 {
		t.Fatalf("Recent: want 10 messages, got %d", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Content != "question 1" {
		t.Fatalf("first message = %+v, want user/question 1", msgs[0])
	}
	if msgs[9].Role != llm.RoleAssistant || msgs[9].Content != "answer 5" {
		t.Fatalf("last message = %+v, want assistant/answer 5", msgs[9])
	}

	// Limit to most recent 4.
	msgs, err = s.Recent(user, channel, 4)
	if err != nil {
		t.Fatalf("Recent(4): %v", err)
	}
	if len(msgs) != 4 || msgs[0].Content != "question 4" {
		t.Fatalf("Recent(4) = %+v, want starting at question 4", msgs)
	}

	// Other users are isolated.
	if msgs, err := s.Recent("99999", channel, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("other user should have no history, got %+v (err %v)", msgs, err)
	}

	// Other channels of the same user are isolated too.
	if msgs, err := s.Recent(user, "chan-2", 10); err != nil || len(msgs) != 0 {
		t.Fatalf("other channel should have no history, got %+v (err %v)", msgs, err)
	}

	if err := s.Clear(user, channel); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	msgs, err = s.Recent(user, channel, 10)
	if err != nil {
		t.Fatalf("Recent after clear: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("after Clear: want 0 messages, got %d", len(msgs))
	}

	// Clearing one channel must not touch the user's other conversations.
	if err := s.Append(user, "chan-2", "user", "hello"); err != nil {
		t.Fatalf("Append chan-2: %v", err)
	}
	if msgs, err := s.Recent(user, "chan-2", 10); err != nil || len(msgs) != 1 {
		t.Fatalf("chan-2 should keep its history after clearing dm-1, got %+v (err %v)", msgs, err)
	}
}

func TestCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	const user = "777"
	const channel = "chan-1"
	for i := 0; i < 20; i++ {
		if err := s.Append(user, channel, "user", "msg"); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if got, err := s.GetSummary(user, channel); err != nil || got != "" {
		t.Fatalf("GetSummary before = %q, %v; want empty", got, err)
	}

	// Compact everything except the newest 5, cap 10.
	msgs, err := s.Compactable(user, channel, 5, 10)
	if err != nil {
		t.Fatalf("Compactable: %v", err)
	}
	if len(msgs) != 10 { // 20 total - 5 kept = 15, capped at 10
		t.Fatalf("Compactable: want 10, got %d", len(msgs))
	}
	if msgs[0].ID != 1 {
		t.Fatalf("Compactable: first id = %d, want oldest (1)", msgs[0].ID)
	}

	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	if err := s.CommitCompaction(user, channel, "user likes Go", ids); err != nil {
		t.Fatalf("CommitCompaction: %v", err)
	}

	if got, err := s.GetSummary(user, channel); err != nil || got != "user likes Go" {
		t.Fatalf("GetSummary after = %q, %v; want saved summary", got, err)
	}
	count, _, err := s.Stats(user, channel)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if count != 10 { // 20 stored - 10 compacted (cap) = 10 remaining
		t.Fatalf("after compaction: want 10 rows left, got %d", count)
	}

	// Stats reports a non-zero last-message time.
	if count, last, _ := s.Stats(user, channel); last.IsZero() {
		t.Fatalf("Stats: last message time should be set (count=%d)", count)
	}

	// Summaries are per conversation: another channel starts with none.
	if got, err := s.GetSummary(user, "chan-2"); err != nil || got != "" {
		t.Fatalf("GetSummary other channel = %q, %v; want empty", got, err)
	}
}

func TestRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	const user = "42"
	for i := 0; i < retention+10; i++ {
		if err := s.Append(user, "chan-1", "user", "msg"); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	msgs, err := s.Recent(user, "chan-1", retention*2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(msgs) != retention {
		t.Fatalf("retention: want %d rows, got %d", retention, len(msgs))
	}

	// Retention is per conversation: a second channel keeps its own rows.
	for i := 0; i < 5; i++ {
		if err := s.Append(user, "chan-2", "user", "msg"); err != nil {
			t.Fatalf("Append chan-2: %v", err)
		}
	}
	msgs, err = s.Recent(user, "chan-2", retention*2)
	if err != nil {
		t.Fatalf("Recent chan-2: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("retention chan-2: want 5 rows, got %d", len(msgs))
	}
}

// TestMigration verifies that a database created with the old per-user
// schema upgrades in place without losing data.
func TestMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// Create an old-schema database directly.
	if err := func() error {
		db, err := openRaw(path)
		if err != nil {
			return err
		}
		defer db.Close()
		oldSchema := `
CREATE TABLE messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    TEXT    NOT NULL,
	role       TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
	content    TEXT    NOT NULL,
	created_at TEXT    NOT NULL
);
CREATE INDEX idx_messages_user_id ON messages (user_id, id);
CREATE TABLE summaries (
	user_id    TEXT PRIMARY KEY,
	summary    TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`
		if _, err := db.Exec(oldSchema); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO messages (user_id, role, content, created_at) VALUES ('u1', 'user', 'old msg', '2026-01-01T00:00:00Z')`); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO summaries (user_id, summary, updated_at) VALUES ('u1', 'old summary', '2026-01-01T00:00:00Z')`); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}

	s, err := New(path) // must migrate without error
	if err != nil {
		t.Fatalf("New (migrate): %v", err)
	}
	defer s.Close()

	// Old rows survive with an empty channel_id.
	msgs, err := s.Recent("u1", "", 10)
	if err != nil {
		t.Fatalf("Recent legacy: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "old msg" {
		t.Fatalf("legacy message lost, got %+v", msgs)
	}
	if got, err := s.GetSummary("u1", ""); err != nil || got != "old summary" {
		t.Fatalf("legacy summary = %q, %v; want preserved", got, err)
	}

	// New conversations work alongside legacy rows.
	if err := s.Append("u1", "chan-9", "user", "new msg"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	msgs, err = s.Recent("u1", "chan-9", 10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "new msg" {
		t.Fatalf("new conversation broken, got %+v (err %v)", msgs, err)
	}

	// Re-running New on an already-migrated DB is a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatalf("New (second run): %v", err)
	}
	defer s2.Close()
	if got, err := s2.GetSummary("u1", ""); err != nil || got != "old summary" {
		t.Fatalf("summary after re-migration = %q, %v; want preserved", got, err)
	}
}
