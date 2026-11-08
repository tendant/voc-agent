// Package store is the agent's SQLite database: notes (with FTS5 search),
// reminders, tool calls, and conversation messages.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// Open opens (creating if needed) the database at path. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn+sep(dsn)+"_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := reindexIfStale(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: time.Now}, nil
}

// noteIndexVersion bumps whenever indexing derives new fields from files
// (v2: privacy labels). Older rows get mtime=0 so the next sync re-reads them.
const noteIndexVersion = "2"

func reindexIfStale(db *sql.DB) error {
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='note_index_version'`).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if v == noteIndexVersion {
		return nil
	}
	if _, err := db.Exec(`UPDATE notes SET mtime=0`); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO meta(key, value) VALUES('note_index_version', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, noteIndexVersion)
	return err
}

func sep(dsn string) string {
	if strings.Contains(dsn, "?") {
		return "&"
	}
	return "?"
}

func (s *Store) Close() error { return s.db.Close() }

// --- notes ---

type Note struct {
	Path, Title, Body string
	MTime             int64
	Privacy           string // see Privacy; defaults to Personal
}

// NoteMTimes returns path → mtime for every indexed note.
func (s *Store) NoteMTimes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, mtime FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var m int64
		if err := rows.Scan(&p, &m); err != nil {
			return nil, err
		}
		out[p] = m
	}
	return out, rows.Err()
}

func (s *Store) UpsertNote(ctx context.Context, n Note) error {
	if n.Privacy == "" {
		n.Privacy = Personal
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes(path, title, body, mtime, privacy) VALUES(?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET title=excluded.title, body=excluded.body, mtime=excluded.mtime, privacy=excluded.privacy`,
		n.Path, n.Title, n.Body, n.MTime, n.Privacy)
	return err
}

func (s *Store) DeleteNote(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE path=?`, path)
	return err
}

type NoteHit struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

// SearchNotes runs a full-text search. Any word may match (OR), ranked by
// BM25 with titles weighted higher; plurals and tenses match via stemming.
func (s *Store) SearchNotes(ctx context.Context, query string, limit int) ([]NoteHit, error) {
	q := ftsQuery(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.path, n.title, snippet(notes_fts, 1, '', '', ' … ', 24)
		FROM notes_fts JOIN notes n ON n.id = notes_fts.rowid
		WHERE notes_fts MATCH ?
		ORDER BY bm25(notes_fts, 5.0, 1.0) LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search %q: %w", q, err)
	}
	defer rows.Close()
	var out []NoteHit
	for rows.Next() {
		var h NoteHit
		if err := rows.Scan(&h.Path, &h.Title, &h.Snippet); err != nil {
			return nil, err
		}
		h.Snippet = strings.Join(strings.Fields(h.Snippet), " ")
		out = append(out, h)
	}
	return out, rows.Err()
}

// Words too common to help a notes search ("find my notes about X").
var stop = map[string]bool{
	"a": true, "an": true, "the": true, "my": true, "me": true, "i": true, "to": true, "of": true,
	"and": true, "or": true, "for": true, "in": true, "on": true, "at": true, "about": true,
	"is": true, "it": true, "with": true, "find": true, "note": true, "notes": true, "what": true,
	"do": true, "did": true, "say": true, "says": true, "any": true, "all": true,
	"when": true, "does": true, "how": true, "who": true, "where": true, "why": true, "which": true,
	"can": true, "you": true, "are": true, "was": true, "were": true, "have": true, "has": true,
	"tell": true, "please": true, "should": true, "would": true, "could": true, "will": true,
	"this": true, "that": true, "there": true, "be": true, "s": true, "t": true, "need": true,
	"know": true, "remind": true, "reminder": true, "tomorrow": true, "today": true, "them": true,
	"us": true, "we": true, "your": true, "from": true, "up": true, "out": true, "go": true, "get": true,
	// Time words belong to the reminder, not the search.
	"minute": true, "minutes": true, "hour": true, "hours": true, "day": true, "days": true,
	"week": true, "weeks": true, "morning": true, "afternoon": true, "evening": true, "tonight": true,
	"am": true, "pm": true, "next": true, "later": true, "half": true, "noon": true,
}

// NoteExists reports whether path is an indexed note.
func (s *Store) NoteExists(ctx context.Context, path string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM notes WHERE path=?`, path).Scan(&one) == nil
}

// RelevantNote is a whole note matched for automatic context.
type RelevantNote struct {
	Path, Title, Body, Privacy string
}

// RelevantNotes returns the best-matching notes for free text, with bodies
// truncated to maxChars. Used to give the model context on every turn.
func (s *Store) RelevantNotes(ctx context.Context, text string, limit, maxChars int) ([]RelevantNote, error) {
	q := ftsQuery(text)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.path, n.title, n.body, n.privacy
		FROM notes_fts JOIN notes n ON n.id = notes_fts.rowid
		WHERE notes_fts MATCH ?
		ORDER BY bm25(notes_fts, 5.0, 1.0) LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: relevant %q: %w", q, err)
	}
	defer rows.Close()
	var out []RelevantNote
	for rows.Next() {
		var n RelevantNote
		if err := rows.Scan(&n.Path, &n.Title, &n.Body, &n.Privacy); err != nil {
			return nil, err
		}
		if len(n.Body) > maxChars {
			n.Body = n.Body[:maxChars] + " …"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QueryTerms returns the search terms ftsQuery would use for text.
func QueryTerms(text string) []string {
	var out []string
	for _, t := range strings.Split(ftsQuery(text), " OR ") {
		if t = strings.TrimSuffix(strings.Trim(t, `"*`), `"`); t != "" {
			out = append(out, strings.Trim(t, `"`))
		}
	}
	return out
}

// ftsQuery turns free text into a safe FTS5 query: each word is quoted
// (so punctuation and FTS operators can't break the syntax) and OR-ed, with
// a prefix match so "deploy" finds "deployment".
func ftsQuery(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	seen := map[string]bool{}
	for _, w := range words {
		if stop[w] || seen[w] || isSmallNumber(w) {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " OR ")
}

// --- reminders ---

type Reminder struct {
	ID      int64     `json:"id"`
	Text    string    `json:"text"`
	DueAt   time.Time `json:"due_at"`
	Status  string    `json:"status"`
	Source  string    `json:"source,omitempty"`
	Created time.Time `json:"created_at"`
}

// CreateReminder inserts a reminder unless one with the same idempotency key
// exists, in which case it returns that one and created=false.
func (s *Store) CreateReminder(ctx context.Context, key, text string, due time.Time, source string) (r Reminder, created bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO reminders(idem_key, text, due_at, source, created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(idem_key) DO NOTHING`,
		key, text, due.Unix(), source, s.Now().Unix())
	if err != nil {
		return r, false, err
	}
	n, _ := res.RowsAffected()
	r, err = s.reminderBy(ctx, `idem_key=?`, key)
	return r, n == 1, err
}

func (s *Store) reminderBy(ctx context.Context, where string, arg any) (Reminder, error) {
	rs, err := s.reminders(ctx, `WHERE `+where, arg)
	if err != nil {
		return Reminder{}, err
	}
	if len(rs) == 0 {
		return Reminder{}, sql.ErrNoRows
	}
	return rs[0], nil
}

func (s *Store) reminders(ctx context.Context, tail string, args ...any) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, due_at, status, source, created_at FROM reminders `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var due, created int64
		if err := rows.Scan(&r.ID, &r.Text, &due, &r.Status, &r.Source, &created); err != nil {
			return nil, err
		}
		r.DueAt, r.Created = time.Unix(due, 0), time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingReminders returns reminders that have not fired, soonest first.
func (s *Store) PendingReminders(ctx context.Context) ([]Reminder, error) {
	return s.reminders(ctx, `WHERE status='pending' ORDER BY due_at, id`)
}

// RecentReminders returns pending reminders plus those fired in the last day.
func (s *Store) RecentReminders(ctx context.Context) ([]Reminder, error) {
	return s.reminders(ctx, `WHERE status='pending' OR (status='fired' AND fired_at > ?) ORDER BY due_at, id`,
		s.Now().Add(-24*time.Hour).Unix())
}

// DueReminders returns pending reminders due at or before now.
func (s *Store) DueReminders(ctx context.Context, now time.Time) ([]Reminder, error) {
	return s.reminders(ctx, `WHERE status='pending' AND due_at <= ? ORDER BY due_at, id`, now.Unix())
}

// MarkFired transitions a pending reminder to fired. It reports false if the
// reminder was not pending (already fired or cancelled), so delivery happens
// at most once even if two schedulers race.
func (s *Store) MarkFired(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE reminders SET status='fired', fired_at=? WHERE id=? AND status='pending'`,
		s.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// NextDue returns when the next pending reminder is due.
func (s *Store) NextDue(ctx context.Context) (time.Time, bool, error) {
	var due sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(due_at) FROM reminders WHERE status='pending'`).Scan(&due)
	if err != nil || !due.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(due.Int64, 0), true, nil
}

// --- tool calls & messages ---

func (s *Store) LogToolCall(ctx context.Context, turnKey, name, args, result string, ok bool, d time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tool_calls(turn_key, name, args, result, ok, ms, created_at) VALUES(?,?,?,?,?,?,?)`,
		turnKey, name, args, result, ok, d.Milliseconds(), s.Now().Unix())
	return err
}

type Message struct{ Role, Content string }

func (s *Store) AppendMessages(ctx context.Context, msgs ...Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(role, content, created_at) VALUES(?,?,?)`,
			m.Role, m.Content, s.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecentMessages returns the last n messages in chronological order.
func (s *Store) RecentMessages(ctx context.Context, n int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, content FROM (SELECT id, role, content FROM messages ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// isSmallNumber skips "1", "30" (as in "in 30 minutes") but keeps longer
// numbers like ticket or migration IDs ("0042", "2026").
func isSmallNumber(w string) bool {
	if len(w) > 2 {
		return false
	}
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// NotesWithPrivacy returns full notes carrying the given label.
func (s *Store) NotesWithPrivacy(ctx context.Context, privacy string) ([]RelevantNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, title, body, privacy FROM notes WHERE privacy=? ORDER BY path`, privacy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelevantNote
	for rows.Next() {
		var n RelevantNote
		if err := rows.Scan(&n.Path, &n.Title, &n.Body, &n.Privacy); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- audit ---

type AuditEntry struct {
	ID          int64           `json:"id"`
	Created     time.Time       `json:"created_at"`
	TurnKey     string          `json:"turn_key"`
	Decision    string          `json:"decision"`
	Destination string          `json:"destination"`
	Model       string          `json:"model"`
	Reason      string          `json:"reason"`
	Detail      string          `json:"detail,omitempty"`
	Proposed    string          `json:"proposed,omitempty"`
	Sent        string          `json:"sent,omitempty"`
	SentSHA256  string          `json:"sent_sha256,omitempty"`
	Response    string          `json:"response,omitempty"`
	Policy      json.RawMessage `json:"policy"`
}

func (s *Store) AddAudit(ctx context.Context, e AuditEntry) (int64, error) {
	if len(e.Policy) == 0 {
		e.Policy = json.RawMessage("{}")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO audit(created_at, turn_key, decision, destination, model, reason, detail, proposed, sent, sent_sha256, response, policy)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Now().Unix(), e.TurnKey, e.Decision, e.Destination, e.Model, e.Reason, e.Detail,
		e.Proposed, e.Sent, e.SentSHA256, e.Response, string(e.Policy))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentAudit returns the newest entries first.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, turn_key, decision, destination, model, reason, detail, proposed, sent, sent_sha256, response, policy
		FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var created int64
		var pol string
		if err := rows.Scan(&e.ID, &created, &e.TurnKey, &e.Decision, &e.Destination, &e.Model, &e.Reason,
			&e.Detail, &e.Proposed, &e.Sent, &e.SentSHA256, &e.Response, &pol); err != nil {
			return nil, err
		}
		e.Created, e.Policy = time.Unix(created, 0), json.RawMessage(pol)
		out = append(out, e)
	}
	return out, rows.Err()
}
