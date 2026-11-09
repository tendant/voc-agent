package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "voc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func write(t *testing.T, dir, rel, body string) {
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNotesSyncAndSearch(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	dir := t.TempDir()
	write(t, dir, "work/deploy.md", "# Deployment checklist\nRun migrations, then deploy the API with canary rollout.")
	write(t, dir, "groceries.md", "Milk, eggs, coffee beans.")
	write(t, dir, "image.png", "not a note")

	if n, err := s.SyncNotes(ctx, dir); err != nil || n != 2 {
		t.Fatalf("sync: n=%d err=%v", n, err)
	}
	if n, _ := s.SyncNotes(ctx, dir); n != 0 {
		t.Fatalf("unchanged resync reindexed %d", n)
	}

	hits, err := s.SearchNotes(ctx, "find my deployment notes", 5)
	if err != nil || len(hits) != 1 || hits[0].Path != "work/deploy.md" || hits[0].Title != "Deployment checklist" {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	// Prefix + stemming: "deploying" and "migration" both match.
	if hits, _ := s.SearchNotes(ctx, "deploying migration", 5); len(hits) != 1 {
		t.Fatalf("stem search: %+v", hits)
	}
	// FTS syntax in user text must not break the query.
	if _, err := s.SearchNotes(ctx, `"coffee" AND (NEAR) *`, 5); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.SearchNotes(ctx, "remind me in 30 minutes", 5); hits != nil {
		t.Fatalf("time words should not search: %+v", hits)
	}
	if !s.NoteExists(ctx, "work/deploy.md") || s.NoteExists(ctx, "Deployment checklist") {
		t.Fatal("NoteExists")
	}
	if hits, _ := s.SearchNotes(ctx, "the my notes", 5); hits != nil {
		t.Fatalf("stopwords-only query should return nothing: %+v", hits)
	}

	// Edit and delete are picked up.
	time.Sleep(10 * time.Millisecond)
	write(t, dir, "groceries.md", "Milk, eggs, oat milk, kombucha.")
	os.Remove(filepath.Join(dir, "work/deploy.md"))
	if n, _ := s.SyncNotes(ctx, dir); n != 2 {
		t.Fatalf("expected 2 changes, got %d", n)
	}
	if hits, _ := s.SearchNotes(ctx, "kombucha", 5); len(hits) != 1 {
		t.Fatal("edit not indexed")
	}
	if hits, _ := s.SearchNotes(ctx, "deployment", 5); len(hits) != 0 {
		t.Fatal("deleted note still searchable")
	}
}

func TestMissingNotesDir(t *testing.T) {
	if _, err := open(t).SyncNotes(context.Background(), filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatal(err)
	}
}

func TestRemindersIdempotentAndFireOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	due := time.Now().Add(time.Hour).Truncate(time.Second)
	r1, created, err := s.CreateReminder(ctx, "k1", "review notes", due, "work/deploy.md")
	if err != nil || !created {
		t.Fatal(created, err)
	}
	r2, created, _ := s.CreateReminder(ctx, "k1", "review notes", due, "")
	if created || r2.ID != r1.ID {
		t.Fatalf("retry created a duplicate: %+v", r2)
	}
	if rs, _ := s.PendingReminders(ctx); len(rs) != 1 || !rs[0].DueAt.Equal(due) {
		t.Fatalf("pending: %+v", rs)
	}
	if rs, _ := s.DueReminders(ctx, time.Now()); len(rs) != 0 {
		t.Fatal("not yet due")
	}
	if next, ok, _ := s.NextDue(ctx); !ok || !next.Equal(due) {
		t.Fatal("next due wrong")
	}
	if ok, _ := s.MarkFired(ctx, r1.ID); !ok {
		t.Fatal("first fire")
	}
	if ok, _ := s.MarkFired(ctx, r1.ID); ok {
		t.Fatal("fired twice")
	}
	if _, ok, _ := s.NextDue(ctx); ok {
		t.Fatal("no pending reminders should remain")
	}
}

func TestMessagesPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "voc.db")
	s, _ := Open(path)
	for _, c := range []string{"a", "b", "c"} {
		s.AppendMessages(ctx, Message{Role: "user", Content: c}, Message{Role: "assistant", Content: c + "!"})
	}
	s.Close()
	s, _ = Open(path) // survives restart
	defer s.Close()
	got, _ := s.RecentMessages(ctx, 3)
	if len(got) != 3 || got[0].Content != "b!" || got[2].Content != "c!" {
		t.Fatalf("got %+v", got)
	}
}

func TestPrivacyLabels(t *testing.T) {
	for rel, c := range map[string]struct{ body, want string }{
		"private/health.md":       {"# Health", LocalOnly},
		"work/private/salary.md":  {"x", LocalOnly},
		"public/recipes.md":       {"x", Public},
		"work/deploy.md":          {"x", Personal},
		"work/secret.md":          {"---\ntitle: x\nprivacy: local_only\n---\nbody", LocalOnly},
		"private/ok-to-share.md":  {"---\nprivacy: public\n---\n", Public},
		"work/bad-frontmatter.md": {"---\nprivacy: banana\n---\n", Personal},
	} {
		if got := Privacy(rel, c.body); got != c.want {
			t.Errorf("%s: %s, want %s", rel, got, c.want)
		}
	}
	ctx := context.Background()
	s := open(t)
	dir := t.TempDir()
	write(t, dir, "private/health.md", "# Health\nBlood pressure 128/82.")
	write(t, dir, "work/deploy.md", "# Deploy\nCanary first.")
	s.SyncNotes(ctx, dir)
	lo, _ := s.NotesWithPrivacy(ctx, LocalOnly)
	if len(lo) != 1 || lo[0].Path != "private/health.md" {
		t.Fatalf("local_only notes: %+v", lo)
	}
	if rel, _ := s.RelevantNotes(ctx, "canary deploy", 2, 100); len(rel) != 1 || rel[0].Privacy != Personal {
		t.Fatalf("relevant: %+v", rel)
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.AddAudit(ctx, AuditEntry{TurnKey: "t", Decision: "denied", Destination: "api.anthropic.com", Model: "m", Reason: "r"})
	s.AddAudit(ctx, AuditEntry{TurnKey: "t", Decision: "approved", Destination: "api.anthropic.com", Model: "m", Reason: "r", Sent: `{"a":1}`})
	got, err := s.RecentAudit(ctx, 10)
	if err != nil || len(got) != 2 || got[0].Decision != "approved" || got[0].Sent != `{"a":1}` || string(got[1].Policy) != "{}" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestOldDatabaseUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The M2 messages table, before tool columns existed.
	if _, err := raw.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, role TEXT NOT NULL, content TEXT NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO messages(role, content, created_at) VALUES('user', 'hi', 1);`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AppendMessages(ctx, Message{Role: "tool", Content: "{}", ToolName: "list_reminders"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.RecentMessages(ctx, 10)
	if len(got) != 2 || got[0].Content != "hi" || got[1].ToolName != "list_reminders" {
		t.Fatalf("%+v", got)
	}
}

func TestTasks(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, err := s.CreateTask(ctx, "get ready", "running", json.RawMessage(`{"steps":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	s.CreateTask(ctx, "older done", "done", nil)
	if n, _ := s.MarkInterrupted(ctx); n != 1 {
		t.Fatalf("interrupted %d", n)
	}
	r, ok, _ := s.ResumableTask(ctx)
	if !ok || r.ID != id || r.Status != "interrupted" {
		t.Fatalf("resumable: %+v %v", r, ok)
	}
	s.UpdateTask(ctx, id, "done", json.RawMessage(`{"steps":[1]}`))
	if got, _ := s.Task(ctx, id); got.Status != "done" || string(got.Plan) != `{"steps":[1]}` {
		t.Fatalf("%+v", got)
	}
	if _, ok, _ := s.ResumableTask(ctx); ok {
		t.Fatal("nothing should be resumable")
	}
}
