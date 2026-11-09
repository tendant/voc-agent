package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/tendant/voc-agent/internal/apple"
	"github.com/tendant/voc-agent/internal/browser"
	"github.com/tendant/voc-agent/internal/store"
)

func appleErr(app string, err error) error {
	if errors.Is(err, apple.ErrNotAllowed) {
		return errors.New(apple.PermissionHint(app))
	}
	return err
}

// privateResult carries the user's own data (Apple Notes). Like web page
// text, it marks the turn so tools that could carry data out need the user's
// own words to run.
type privateResult struct {
	Data any    `json:"data"`
	Note string `json:"note"`
}

func (privateResult) Untrusted() bool { return true }

// AppleNotes returns tools to search, read and create Apple Notes. There is
// deliberately no edit or delete.
func AppleNotes(st *store.Store, n apple.Notes) []Tool {
	type searchArgs struct {
		Query string `json:"query" desc:"Words to look for in note titles and text"`
	}
	type readArgs struct {
		Title string `json:"title" desc:"Title of the note to read, as found by apple_notes_search"`
	}
	type createArgs struct {
		Title string `json:"title" desc:"Short title for the new note"`
		Body  string `json:"body,omitempty" desc:"The note's text, one item per line"`
	}
	var mu sync.Mutex
	made := map[string]map[string]any{} // turn key + title → result: no duplicates on retries
	const private = "From the user's Apple Notes: private. Use it to answer; don't send it anywhere unless the user asks."
	return []Tool{
		Typed("apple_notes_search",
			"Search the user's Apple Notes app (titles and text). Use it when the user mentions Apple Notes or the Notes app.",
			func(ctx context.Context, _ Call, a searchArgs) (any, error) {
				hits, err := n.Search(ctx, a.Query, 6)
				if err != nil {
					return nil, appleErr("Notes", err)
				}
				return privateResult{Data: map[string]any{"count": len(hits), "notes": hits}, Note: private}, nil
			}).ReadOnly(),
		Typed("apple_notes_read",
			"Read one Apple Note by its title.",
			func(ctx context.Context, _ Call, a readArgs) (any, error) {
				note, err := n.Read(ctx, a.Title)
				if err != nil {
					return nil, appleErr("Notes", err)
				}
				return privateResult{Data: note, Note: private}, nil
			}).ReadOnly(),
		Typed("apple_notes_create",
			"Create a new note in the user's Apple Notes app. Only say it was saved after this returns status \"created\".",
			func(ctx context.Context, c Call, a createArgs) (any, error) {
				if err := leakCheck(ctx, st, a.Title+"\n"+a.Body); err != nil {
					// local_only text belongs in voc's private notes, not in a
					// synced app
					return nil, fmt.Errorf("not saved: it contains text from your local-only notes, which stay out of synced apps")
				}
				key := "apple_notes_create\x00" + c.TurnKey + "\x00" + strings.ToLower(strings.TrimSpace(a.Title))
				mu.Lock()
				prev, dup := made[key]
				mu.Unlock()
				if dup {
					return prev, nil
				}
				if st != nil { // survives restarts: a resumed task won't make it twice
					if r, ok := st.ActionDone(ctx, key); ok {
						var res map[string]any
						json.Unmarshal(r, &res)
						res["status"] = "already_exists"
						return res, nil
					}
				}
				hit, err := n.Create(ctx, strings.TrimSpace(a.Title), a.Body)
				if err != nil {
					return nil, appleErr("Notes", err)
				}
				res := map[string]any{"status": "created", "title": hit.Title, "folder": hit.Folder}
				mu.Lock()
				made[key] = res
				mu.Unlock()
				if st != nil {
					b, _ := json.Marshal(res)
					st.RecordAction(context.WithoutCancel(ctx), key, b)
				}
				return res, nil
			}),
	}
}

// AppleMusic returns the music_play tool.
func AppleMusic(st *store.Store, m *apple.Music) []Tool {
	type playArgs struct {
		Query string `json:"query" desc:"What to play: a song, album, artist or playlist name, in the user's words"`
		Kind  string `json:"kind,omitempty" desc:"What the query names, if the user said" enum:"any,song,album,artist,playlist"`
	}
	return []Tool{
		Typed("music_play",
			"Play something from the user's Apple Music library or playlists in the Music app. Use it when the user asks to play music, a song, an album, an artist or a playlist (not a video).",
			func(ctx context.Context, _ Call, a playArgs) (any, error) {
				if err := leakCheck(ctx, st, a.Query); err != nil {
					return nil, err
				}
				q, kind := musicQuery(a.Query, a.Kind)
				t, err := m.Play(ctx, q, kind)
				if err != nil {
					return nil, appleErr("Music", err)
				}
				return t, nil
			}),
	}
}

// Media is one "media" tool for whatever is playing: the YouTube/web video
// in voc's browser or the Music app. Either may be nil.
func Media(b *browser.Browser, m *apple.Music) Tool {
	type mediaArgs struct {
		Action string `json:"action" desc:"What to do with what's playing (status says what is playing)" enum:"play,pause,next,previous,mute,unmute,louder,quieter,restart,forward,back,status"`
	}
	browserMedia := func(ctx context.Context) bool {
		if b == nil || !b.IsOpen() {
			return false
		}
		if b.Playing(ctx) {
			return true
		}
		// A paused video still counts when Music isn't playing.
		return m == nil || !m.Playing(ctx)
	}
	t := Typed("media",
		"Control what's playing, the video in the browser or the Music app: play (resume), pause, next, previous, mute, unmute, louder, quieter, restart, forward, back, or status (what's playing).",
		func(ctx context.Context, _ Call, a mediaArgs) (any, error) {
			if browserMedia(ctx) {
				switch a.Action {
				case "status":
					p, err := b.Read(ctx)
					if err != nil {
						return nil, err
					}
					return map[string]any{"source": "browser", "title": p.Title, "playing": b.Playing(ctx)}, nil
				case "previous":
					return b.Nav(ctx, "back")
				}
				st, err := b.Media(ctx, a.Action)
				if err != nil {
					return nil, err
				}
				return st, nil
			}
			if m == nil {
				return nil, errors.New("nothing is playing")
			}
			action := map[string]string{"restart": "previous", "forward": "next", "back": "previous"}[a.Action]
			if action == "" {
				action = a.Action
			}
			tr, err := m.Control(ctx, action)
			if err != nil {
				return nil, appleErr("Music", err)
			}
			return tr, nil
		})
	return t.When(func() bool { return m != nil || (b != nil && b.IsOpen()) })
}

var musicNoise = regexp.MustCompile(`(?i)^(?:(?:play|put on|listen to|my|the|some|a|an|me)\s+)+|\s+(?:playlist|album|record|songs?|tracks?|music)$|^(?:songs?|music|tracks?)\s+by\s+`)

// musicQuery strips words that describe the request rather than name the
// music ("my workout playlist" → "workout", kind playlist), since the
// library search matches names.
func musicQuery(q, kind string) (string, string) {
	lower := strings.ToLower(q)
	switch {
	case kind == "" || kind == "any":
		switch {
		case strings.HasSuffix(lower, " playlist"):
			kind = "playlist"
		case strings.HasSuffix(lower, " album"):
			kind = "album"
		case strings.HasPrefix(lower, "songs by ") || strings.HasPrefix(lower, "music by "):
			kind = "artist"
		}
	}
	for {
		next := strings.TrimSpace(musicNoise.ReplaceAllString(q, ""))
		if next == q || next == "" {
			break
		}
		q = next
	}
	return q, kind
}
