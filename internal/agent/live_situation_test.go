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

// TestLiveSituation: Qwen3 answers from the per-turn situation message.
// VOC_LIVE=1.
func TestLiveSituation(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	s := &Session{LLM: ol, TTS: fakeTTS{}, Tools: tools.NewRegistry(tools.Personal(st, nil, nil)...),
		Situation: func(context.Context) []string {
			return []string{
				`Open in voc's browser: "jazz lofi radio, beats to chill/study to - YouTube" (youtube.com).`,
				"A video or audio is playing in the browser.",
				`Music app: paused "So What" by Miles Davis.`,
				"Upcoming reminders, soonest first: Stretch (in 2 hours, " + time.Now().Add(2*time.Hour).Format("Mon Jan 2, 3:04 PM") +
					"); Review the deployment notes (in 26 hours, " + time.Now().Add(26*time.Hour).Format("Mon Jan 2, 3:04 PM") + ").",
				`Unfinished task (interrupted): "Get me ready for Thursday's billing deploy". The user can say "continue my last task".`,
			}
		}}
	for i, c := range []struct {
		say, mode string
		want      []string
	}{
		{"What am I watching right now?", ModeVoice, []string{"lofi", "lo-fi", "jazz"}},
		{"When's my next reminder?", ModeTyped, []string{"stretch"}},
		{"Is anything paused in Music?", ModeVoice, []string{"so what", "miles"}},
		{"Was I in the middle of something?", ModeHandsFree, []string{"deploy"}},
		{"What day is it?", ModeTyped, []string{strings.ToLower(time.Now().Weekday().String())}},
		{"Remind me to by oat milk tomorrow.", ModeVoice, []string{"oat milk"}}, // ASR slip: "by" → "buy" is logged, not required
	} {
		r := &rec{}
		t0 := time.Now()
		s.Turn(WithInputMode(context.Background(), c.mode), uint32(i+1), c.say, t0, r)
		var reply, trace string
		for _, e := range r.events {
			switch e.Type {
			case "assistant_done":
				reply = e.Text
			case "tool_call":
				trace += " " + e.Name + string(e.Args)
			}
		}
		t.Logf("USER  %s (%s)\n      TOOLS%s\n      VOC   %s (%v)", c.say, c.mode, trace, reply, time.Since(t0).Round(10*time.Millisecond))
		hit := false
		for _, w := range c.want {
			hit = hit || strings.Contains(strings.ToLower(reply+trace), w)
		}
		if !hit {
			t.Errorf("turn %d: reply lacks any of %v", i+1, c.want)
		}
	}
}
