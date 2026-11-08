package store

import (
	"context"
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
		s.AppendMessages(ctx, Message{"user", c}, Message{"assistant", c + "!"})
	}
	s.Close()
	s, _ = Open(path) // survives restart
	defer s.Close()
	got, _ := s.RecentMessages(ctx, 3)
	if len(got) != 3 || got[0].Content != "b!" || got[2].Content != "c!" {
		t.Fatalf("got %+v", got)
	}
}
