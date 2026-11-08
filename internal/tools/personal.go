package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/tendant/voc-agent/internal/store"
)

// Personal returns the notes and reminder tools backed by st. onReminder is
// called after a reminder is created (to wake the scheduler); it may be nil.
func Personal(st *store.Store, now func() time.Time, onReminder func()) []Tool {
	if now == nil {
		now = time.Now
	}
	type searchArgs struct {
		Query string `json:"query" desc:"Keywords to search for, e.g. \"deployment checklist\""`
		Limit int    `json:"limit,omitempty" desc:"Maximum results, default 5"`
	}
	type createArgs struct {
		Text   string `json:"text" desc:"What to remind the user about, phrased as a short task, e.g. \"Review the deployment notes\""`
		When   string `json:"when" desc:"When to remind, in the user's own words, e.g. \"tomorrow at 9am\", \"in 20 minutes\", \"friday evening\". Do not convert it to a date yourself."`
		Source string `json:"source,omitempty" desc:"Path of a note this reminder refers to, if any"`
	}
	type listArgs struct{}

	fmtDue := func(t time.Time) string {
		n := now()
		day := t.Format("Monday, January 2")
		switch {
		case sameDay(t, n):
			day = "today"
		case sameDay(t, n.AddDate(0, 0, 1)):
			day = "tomorrow"
		}
		return day + " at " + t.Format("3:04 PM")
	}
	type reminderOut struct {
		ID     int64  `json:"id"`
		Text   string `json:"text"`
		Due    string `json:"due"`
		DueISO string `json:"due_iso"`
		Source string `json:"source,omitempty"`
	}
	toOut := func(r store.Reminder) reminderOut {
		return reminderOut{r.ID, r.Text, fmtDue(r.DueAt), r.DueAt.Format(time.RFC3339), r.Source}
	}

	return []Tool{
		Typed("search_notes",
			"Search the user's private notes (local files) by keywords. Use this whenever the user asks about their notes, plans, projects, or anything they may have written down. Returns matching notes with their path and a snippet.",
			func(ctx context.Context, _ Call, a searchArgs) (any, error) {
				limit := a.Limit
				if limit <= 0 || limit > 10 {
					limit = 5
				}
				hits, err := st.SearchNotes(ctx, a.Query, limit)
				if err != nil {
					return nil, err
				}
				if hits == nil {
					hits = []store.NoteHit{}
				}
				return map[string]any{"count": len(hits), "results": hits}, nil
			}),
		Typed("create_reminder",
			"Create a reminder that will be spoken to the user at the given time. Only say a reminder is set after this tool returns status \"created\" or \"already_exists\".",
			func(ctx context.Context, c Call, a createArgs) (any, error) {
				due, err := ParseWhen(a.When, now())
				if err != nil {
					return nil, err
				}
				text := strings.TrimSpace(a.Text)
				if a.Source != "" && !st.NoteExists(ctx, a.Source) {
					a.Source = "" // models sometimes put a title or guess here
				}
				key := idemKey(c.TurnKey, strings.ToLower(text), strconv.FormatInt(due.Unix(), 10))
				r, created, err := st.CreateReminder(ctx, key, text, due, a.Source)
				if err != nil {
					return nil, err
				}
				if onReminder != nil {
					onReminder()
				}
				status := "created"
				if !created {
					status = "already_exists"
				}
				return map[string]any{"status": status, "reminder": toOut(r)}, nil
			}),
		Typed("list_reminders",
			"List the user's upcoming (not yet delivered) reminders.",
			func(ctx context.Context, _ Call, _ listArgs) (any, error) {
				rs, err := st.PendingReminders(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]reminderOut, 0, len(rs))
				for _, r := range rs {
					out = append(out, toOut(r))
				}
				return map[string]any{"count": len(out), "reminders": out}, nil
			}),
	}
}

func idemKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
