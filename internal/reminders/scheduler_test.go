package reminders

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/store"
)

func TestSchedulerFiresOnceAndWaitsForListener(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var listening atomic.Bool
	var mu sync.Mutex
	var got []string
	fired := make(chan struct{}, 10)
	s := (&Scheduler{
		Store:      st,
		CanDeliver: listening.Load,
		Deliver: func(r store.Reminder) {
			mu.Lock()
			got = append(got, r.Text)
			mu.Unlock()
			fired <- struct{}{}
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Init()
	go s.Run(ctx)

	st.CreateReminder(ctx, "a", "past due", time.Now().Add(-time.Minute), "")
	st.CreateReminder(ctx, "b", "soon", time.Now().Add(1500*time.Millisecond), "")
	st.CreateReminder(ctx, "c", "later", time.Now().Add(time.Hour), "")
	s.Wake()

	select {
	case <-fired:
		t.Fatal("fired with nobody listening")
	case <-time.After(300 * time.Millisecond):
	}
	listening.Store(true)
	s.Wake() // a client connected
	for i := 0; i < 2; i++ {
		select {
		case <-fired:
		case <-time.After(4 * time.Second):
			t.Fatalf("only %d reminders fired", i)
		}
	}
	select {
	case <-fired:
		t.Fatal("extra delivery")
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "past due" || got[1] != "soon" {
		t.Fatalf("got %v", got)
	}
	if rs, _ := st.PendingReminders(ctx); len(rs) != 1 || rs[0].Text != "later" {
		t.Fatalf("pending: %+v", rs)
	}
}
