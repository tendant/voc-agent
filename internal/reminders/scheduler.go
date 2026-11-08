// Package reminders delivers due reminders.
package reminders

import (
	"context"
	"log/slog"
	"time"

	"github.com/tendant/voc-agent/internal/store"
)

// Scheduler fires due reminders through Deliver.
//
// Delivery is at most once: a reminder is marked fired (a conditional
// pending→fired update) before it is delivered. If nobody is listening
// (CanDeliver returns false), due reminders stay pending and fire as soon as
// a client connects and calls Wake.
type Scheduler struct {
	Store      *store.Store
	CanDeliver func() bool
	Deliver    func(store.Reminder)
	Log        *slog.Logger
	Now        func() time.Time

	wake chan struct{}
}

// maxSleep bounds how long the loop sleeps, so clock changes or missed wakes
// can't delay a reminder by more than this.
const maxSleep = 30 * time.Second

// Wake makes the scheduler re-check now (after a new reminder or a client
// connecting). It never blocks.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Init must be called before Run or Wake.
func (s *Scheduler) Init() *Scheduler {
	s.wake = make(chan struct{}, 1)
	return s
}

func (s *Scheduler) Run(ctx context.Context) {
	for {
		s.fireDue(ctx)
		sleep := maxSleep
		if next, ok, err := s.Store.NextDue(ctx); err == nil && ok {
			if d := next.Sub(s.now()); d < sleep {
				sleep = max(d, 0)
				if sleep == 0 {
					sleep = maxSleep // due but undeliverable: wait for a Wake
				}
			}
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

func (s *Scheduler) fireDue(ctx context.Context) {
	if s.CanDeliver != nil && !s.CanDeliver() {
		return
	}
	due, err := s.Store.DueReminders(ctx, s.now())
	if err != nil {
		s.Log.Error("reminders: query due", "err", err)
		return
	}
	for _, r := range due {
		ok, err := s.Store.MarkFired(ctx, r.ID)
		if err != nil {
			s.Log.Error("reminders: mark fired", "id", r.ID, "err", err)
			continue
		}
		if !ok {
			continue // someone else fired it
		}
		s.Log.Info("reminder fired", "id", r.ID, "text", r.Text, "late_s", int(s.now().Sub(r.DueAt).Seconds()))
		s.Deliver(r)
	}
}
