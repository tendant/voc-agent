package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tendant/voc-agent/internal/apple"
	browserpkg "github.com/tendant/voc-agent/internal/browser"
	"github.com/tendant/voc-agent/internal/store"
)

// situationFacts gathers what's true right now for the agent's per-turn
// situation message. Every source is optional and cheap: nothing launches
// the browser or Music, and an app that isn't running is skipped.
func situationFacts(st *store.Store, b *browserpkg.Browser, music *apple.Music, cloudOn bool) func(ctx context.Context) []string {
	var mu sync.Mutex
	var musicLine string
	var musicAt time.Time
	musicOff := false // permission denied: stop asking this session
	return func(ctx context.Context) []string {
		var (
			lines []string
			lmu   sync.Mutex
			wg    sync.WaitGroup
		)
		// Each source runs in parallel within the caller's deadline; results
		// are sorted back into a fixed order below.
		var page, media, music_, rem, task string
		if b != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				title, u, ok := b.Where(ctx)
				if !ok {
					return
				}
				host := u
				if pu, err := url.Parse(u); err == nil && pu.Host != "" {
					host = strings.TrimPrefix(pu.Host, "www.")
				}
				page = fmt.Sprintf("Open in voc's browser: %q (%s).", strings.TrimSpace(title), host)
				if b.Playing(ctx) {
					media = "A video or audio is playing in the browser."
				}
			}()
		}
		if music != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mu.Lock()
				defer mu.Unlock()
				if musicOff {
					return
				}
				if time.Since(musicAt) > 5*time.Second {
					musicLine, musicAt = "", time.Now()
					t, err := music.Status(ctx)
					switch {
					case errors.Is(err, apple.ErrNotAllowed):
						musicOff = true
					case err == nil && (t.State == "playing" || t.State == "paused") && t.Name != "":
						musicLine = fmt.Sprintf("Music app: %s %q by %s.", t.State, t.Name, t.Artist)
					}
				}
				music_ = musicLine
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs, err := st.PendingReminders(ctx)
			if err != nil || len(rs) == 0 {
				rem = "No upcoming reminders."
				return
			}
			var parts []string
			for i, r := range rs {
				if i == 3 {
					parts = append(parts, fmt.Sprintf("and %d more", len(rs)-3))
					break
				}
				parts = append(parts, fmt.Sprintf("%s (%s, %s)", r.Text, untilText(time.Until(r.DueAt)), r.DueAt.Format("Mon Jan 2, 3:04 PM")))
			}
			rem = "Upcoming reminders, soonest first: " + strings.Join(parts, "; ") + "."
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t, ok, err := st.ResumableTask(ctx); err == nil && ok && time.Since(t.Updated) < 24*time.Hour {
				task = fmt.Sprintf("Unfinished task (%s): %q. The user can say \"continue my last task\".", t.Status, t.Goal)
			}
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done(): // budget spent: use what's in
		}
		lmu.Lock()
		defer lmu.Unlock()
		for _, l := range []string{page, media, music_, rem, task} {
			if l != "" {
				lines = append(lines, l)
			}
		}
		if cloudOn {
			lines = append(lines, "Cloud help (Claude) is available, with the user's approval.")
		} else {
			lines = append(lines, "Cloud help is off; everything runs on this computer.")
		}
		return lines
	}
}

// untilText says how far away a time is, which models order more reliably
// than timestamps.
func untilText(d time.Duration) string {
	switch {
	case d < 0:
		return "overdue"
	case d < time.Hour:
		return fmt.Sprintf("in %d minutes", int(d.Minutes()+0.5))
	case d < 36*time.Hour:
		return fmt.Sprintf("in %d hours", int(d.Hours()+0.5))
	}
	return fmt.Sprintf("in %d days", int(d.Hours()/24+0.5))
}
