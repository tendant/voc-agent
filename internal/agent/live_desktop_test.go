package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

type recordingOpener struct{ opened []string }

func (r *recordingOpener) OpenURL(_ context.Context, u string) error {
	r.opened = append(r.opened, u)
	return nil
}
func (r *recordingOpener) OpenApp(_ context.Context, a string) error {
	r.opened = append(r.opened, "app:"+a)
	return nil
}

// TestLiveDesktop checks Qwen3 picks the right desktop action. Nothing is
// actually opened. VOC_LIVE=1.
func TestLiveDesktop(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	st.SyncNotes(ctx, "../../notes")
	op := &recordingOpener{}
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	s := &Session{LLM: ol, TTS: fakeTTS{}, Recall: StoreRecall(st),
		Tools: tools.NewRegistry(append(tools.Personal(st, nil, nil), tools.Desktop(st, op, tools.DefaultApps)...)...)}
	for i, c := range []struct{ say, want string }{
		{"Open YouTube and search for lo-fi jazz.", "youtube.com/results?search_query=lo-fi+jazz"},
		{"Play some Taylor Swift on YouTube.", "youtube.com/results?search_query=taylor+swift"},
		{"Look up the CAP theorem on Wikipedia.", "wikipedia.org/w/index.php?search=cap+theorem"},
		{"Open the calculator.", "app:calculator"},
		{"Go to github.com slash tendant slash voc-agent.", "github.com/tendant/voc-agent"},
		{"Find coffee shops near me on the map.", "google.com/maps/search/coffee"},
		{"Open Terminal.", ""}, // not allowed: nothing opens, and voc must not claim it did
	} {
		before := len(op.opened)
		r := &rec{}
		t0 := time.Now()
		if err := s.Turn(ctx, uint32(i+1), c.say, t0, r); err != nil {
			t.Fatal(err)
		}
		var reply, trace string
		for _, e := range r.events {
			switch e.Type {
			case "assistant_done":
				reply = e.Text
			case "tool_call":
				trace += " " + e.Name + string(e.Args)
			case "guard":
				trace += " GUARD(" + e.Text + ")"
			}
		}
		opened := strings.ToLower(strings.Join(op.opened[before:], " "))
		t.Logf("USER  %s\n      TOOLS%s\n      OPENED %s\n      VOC   %s (%v)", c.say, trace, opened, reply, time.Since(t0).Round(10*time.Millisecond))
		if c.want == "" {
			if opened != "" {
				t.Errorf("turn %d opened %q", i+1, opened)
			}
			if strings.Contains(strings.ToLower(reply), "opened") && !strings.Contains(strings.ToLower(reply), "not") {
				t.Errorf("turn %d claims success: %q", i+1, reply)
			}
		} else if !strings.Contains(opened, c.want) {
			t.Errorf("turn %d: want %q, opened %q", i+1, c.want, opened)
		}
	}
}
