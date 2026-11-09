package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/apple"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// cannedApple answers like Notes/Music would, by recognizing the script.
type cannedApple struct{ calls []string }

func (c *cannedApple) Run(_ context.Context, script string, args ...string) ([]byte, error) {
	kind := "?"
	switch {
	case strings.Contains(script, "plaintext: {_contains"):
		kind = "notes.search"
	case strings.Contains(script, "no note titled"):
		kind = "notes.read"
	case strings.Contains(script, "defaultFolder"):
		kind = "notes.create"
	case strings.Contains(script, "M.search(lib"):
		kind = "music.play"
	case strings.Contains(script, "switch (a)"):
		kind = "music.control"
	case strings.Contains(script, "currentTrack"):
		kind = "music.status"
	}
	c.calls = append(c.calls, kind+" "+strings.Join(args, " | "))
	switch kind {
	case "notes.search":
		return []byte(`[{"title":"Packing list for Lisbon","folder":"Notes","modified":"2026-11-01","snippet":"passport, charger, sunscreen"}]`), nil
	case "notes.read":
		return []byte(`{"title":"Packing list for Lisbon","folder":"Notes","text":"Packing list for Lisbon\npassport\nphone charger\nsunscreen\nwalking shoes"}`), nil
	case "notes.create":
		return []byte(`{"title":"` + args[0] + `","folder":"Notes"}`), nil
	case "music.play":
		if args[1] == "playlist" {
			return []byte(`{"kind":"playlist","state":"playing","name":"` + args[0] + `"}`), nil
		}
		return []byte(`{"kind":"` + args[1] + `","state":"playing","name":"So What","artist":"Miles Davis","album":"Kind of Blue"}`), nil
	case "music.control", "music.status":
		return []byte(`{"state":"playing","name":"So What","artist":"Miles Davis","volume":50}`), nil
	}
	return []byte(`{}`), nil
}

// TestLiveApple: Qwen3 picks the Apple tools correctly. A fake runner stands
// in for Notes and Music, so nothing touches the real apps. VOC_LIVE=1.
func TestLiveApple(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	st.SyncNotes(ctx, "../../notes")
	r := &cannedApple{}
	music := &apple.Music{R: r}
	reg := tools.NewRegistry(append(append(append(tools.Personal(st, nil, nil), tools.AppleNotes(st, apple.Notes{R: r})...),
		tools.AppleMusic(st, music)...), tools.Media(nil, music))...)
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	s := &Session{LLM: ol, TTS: fakeTTS{}, Tools: reg, Recall: StoreRecall(st)}

	for i, c := range []struct{ say, wantCall, wantReply string }{
		{"Search my Apple Notes for the packing list.", "notes.search", "lisbon"},
		{"What's on my Lisbon packing list in Apple Notes?", "", "passport"}, // may reuse the search result
		{"Make a note called Groceries with eggs and oat milk.", "notes.create Groceries", ""},
		{"Play Kind of Blue.", "music.play", ""},
		{"Play my workout playlist.", "music.play workout | playlist", ""},
		{"Pause.", "music.control pause", "paused"},
		{"What's playing?", "music.", "so what"},
		{"Next song.", "music.control next", ""},
		{"What does my deployment note say about rollback?", "", "previous image"}, // local notes, not Apple
	} {
		before := len(r.calls)
		rec := &rec{}
		t0 := time.Now()
		if err := s.Turn(ctx, uint32(i+1), c.say, t0, rec); err != nil {
			t.Fatal(err)
		}
		var reply, trace string
		for _, e := range rec.events {
			switch e.Type {
			case "assistant_done":
				reply = e.Text
			case "tool_call":
				trace += " " + e.Name + string(e.Args)
			case "fast":
				trace += " FAST"
			case "guard":
				trace += " GUARD(" + e.Text + ")"
			}
		}
		calls := strings.Join(r.calls[before:], " ; ")
		t.Logf("USER  %s\n      TOOLS%s\n      APPLE %s\n      VOC   %s (%v)", c.say, trace, calls, reply, time.Since(t0).Round(10*time.Millisecond))
		if c.wantCall != "" && !strings.Contains(calls, c.wantCall) {
			t.Errorf("turn %d: want call %q", i+1, c.wantCall)
		}
		if i == 8 && strings.Contains(calls, "notes.") {
			t.Errorf("turn %d: used Apple Notes for a local note", i+1)
		}
		if c.wantReply != "" && !strings.Contains(strings.ToLower(reply), c.wantReply) {
			t.Errorf("turn %d: reply lacks %q", i+1, c.wantReply)
		}
	}
}
