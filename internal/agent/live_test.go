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

// TestLiveTools runs the M2 exit criterion against local Qwen3 with the
// sample notes. Run with VOC_LIVE=1.
func TestLiveTools(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.SyncNotes(ctx, "../../notes"); err != nil {
		t.Fatal(err)
	}
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	s := &Session{LLM: ol, TTS: fakeTTS{}, Tools: tools.NewRegistry(tools.Personal(st, nil, nil)...), Recall: StoreRecall(st)}

	type check func(r *rec, reply string, reminders int) string
	usedTool := func(name string) check {
		return func(r *rec, _ string, _ int) string {
			for _, e := range r.events {
				if e.Type == "tool_call" && e.Name == name {
					return ""
				}
			}
			return "did not call " + name
		}
	}
	says := func(words ...string) check {
		return func(_ *rec, reply string, _ int) string {
			for _, w := range words {
				if strings.Contains(strings.ToLower(reply), w) {
					return ""
				}
			}
			return "reply mentions none of " + strings.Join(words, "/")
		}
	}
	reminders := func(n int) check {
		return func(_ *rec, _ string, got int) string {
			if got != n {
				return "reminders=" + string(rune('0'+got)) + " want " + string(rune('0'+n))
			}
			return ""
		}
	}
	steps := []struct {
		say    string
		checks []check
	}{
		{"Find my deployment notes and remind me to review them tomorrow.",
			[]check{usedTool("search_notes"), usedTool("create_reminder"), reminders(1), says("deploy", "billing")}},
		{"What's the rollback plan?", []check{says("previous image", "image tag", "0042", "down migration", "redeploy")}},
		{"What reminders do I have?", []check{usedTool("list_reminders"), says("review", "deploy")}},
		{"When does the trash go out?", []check{says("wednesday")}},
		{"What did Maya want to work on?", []check{says("search relevance")}},
		{"What's the capital of France?", []check{says("paris")}},
	}
	for i, step := range steps {
		r := &rec{}
		t0 := time.Now()
		if err := s.Turn(ctx, uint32(i+1), step.say, t0, r); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		var reply string
		var trace []string
		for _, e := range r.events {
			switch e.Type {
			case "assistant_done":
				reply = e.Text
			case "tool_call":
				trace = append(trace, e.Name+string(e.Args))
			case "recall":
				trace = append(trace, "recall"+string(e.Result))
			}
		}
		rs, _ := st.PendingReminders(ctx)
		t.Logf("USER  %s", step.say)
		t.Logf("TOOLS %s", strings.Join(trace, " → "))
		t.Logf("VOC   %s   (%v)", reply, time.Since(t0).Round(10*time.Millisecond))
		for _, c := range step.checks {
			if msg := c(r, reply, len(rs)); msg != "" {
				t.Errorf("turn %d: %s", i+1, msg)
			}
		}
	}
	rs, _ := st.PendingReminders(ctx)
	for _, r := range rs {
		t.Logf("REMINDER %q due %s source=%q", r.Text, r.DueAt.Format("Mon Jan 2 3:04 PM"), r.Source)
	}
}
