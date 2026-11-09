package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/apple"
	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/policy"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// TestLiveGoals runs goal mode with the real local planner (Qwen3-8B with
// thinking) on the sample notes. Apple apps are faked. VOC_LIVE=1.
func TestLiveGoals(t *testing.T) {
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
	var plans []string
	var planner llm.Chat
	thinks, inCloud := true, false
	switch m := os.Getenv("VOC_PLANNER"); m {
	case "":
	case "claude": // real Claude via ../../.env; local fallback
		loadEnvFile(t, "../../.env")
		c := cloud.New("")
		p := &cloud.Planner{C: c, Guard: func(b []byte) error {
			private, _ := st.NotesWithPrivacy(ctx, store.LocalOnly)
			return policy.NewFingerprints(private).CheckOutbound(b)
		}, Audit: func(sent cloud.Sent, reply string, err error) {
			t.Logf("      CLAUDE %d bytes sent, err=%v", len(sent.Body), err)
		}}
		planner, inCloud = llm.Fallback{Primary: p, Secondary: ol, OnFallback: func(err error) { t.Logf("      FALLBACK %v", err) }}, true
	default: // e.g. qwen3-coder:30b
		planner, thinks = &llm.Ollama{BaseURL: ol.BaseURL, Model: m, Temperature: 0.2, KeepAlive: "30m"}, os.Getenv("VOC_PLANNER_THINKS") != ""
	}
	s := &Session{LLM: ol, TTS: fakeTTS{}, Tools: reg, Recall: StoreRecall(st), Tasks: st, PlanFast: os.Getenv("VOC_PLAN_FAST") != "",
		Planner: planner, PlannerThinks: thinks, PlannerCloud: inCloud,
		Approver: func(_ context.Context, _ uint32, _ Sink, req tools.ApprovalRequest) (bool, error) {
			plans = append(plans, req.Prompt)
			return true, nil
		}}

	for i, g := range []struct {
		say   string
		check func(rems []store.Reminder, apple string) string
	}{
		{"Get me ready for Thursday's billing deploy.", func(rems []store.Reminder, _ string) string {
			if len(rems) == 0 {
				return "no reminders"
			}
			return ""
		}},
		{"Help me prepare for my one-on-one with Maya.", nil},
		{"Plan my evening: remind me to take the recycling out at 8 pm and put on some jazz.", func(rems []store.Reminder, a string) string {
			found := false
			for _, r := range rems {
				found = found || strings.Contains(strings.ToLower(r.Text), "recycl")
			}
			if !found {
				return "no recycling reminder"
			}
			if !strings.Contains(a, "music.play") {
				return "no music"
			}
			return ""
		}},
		{"Take care of booking my flight to Lisbon.", nil},
	} {
		before := len(r.calls)
		rec := &rec{}
		t0 := time.Now()
		if err := s.Turn(ctx, uint32(i+1), g.say, t0, rec); err != nil {
			t.Fatal(err)
		}
		var trace []string
		for _, e := range rec.events {
			if e.Type == "tool_call" {
				trace = append(trace, e.Name+string(e.Args))
			}
		}
		ts, _ := st.RecentTasks(ctx, 1)
		var p goalPlan
		if len(ts) > 0 {
			json.Unmarshal(ts[0].Plan, &p)
		}
		var steps []string
		for _, x := range p.Steps {
			steps = append(steps, x.Tool+":"+x.Status)
		}
		rems, _ := st.PendingReminders(ctx)
		t.Logf("GOAL  %s\n      PLAN  %s | %s\n      TOOLS %s\n      APPLE %s\n      VOC   %s\n      (%v)",
			g.say, strings.Join(steps, ", "), p.Summary, strings.Join(trace, " ; "), strings.Join(r.calls[before:], " ; "),
			spokenText(rec), time.Since(t0).Round(100*time.Millisecond))
		if g.check != nil {
			if msg := g.check(rems, strings.Join(r.calls[before:], " ")); msg != "" {
				t.Errorf("goal %d: %s", i+1, msg)
			}
		}
	}
	rems, _ := st.PendingReminders(ctx)
	for _, r := range rems {
		t.Logf("REMINDER %q %s", r.Text, r.DueAt.Format("Mon 3:04 PM"))
	}
}

// loadEnvFile sets KEY=VALUE lines from path (values never logged).
func loadEnvFile(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skip("no " + path)
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if ok && !strings.HasPrefix(k, "#") && os.Getenv(strings.TrimSpace(k)) == "" {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}
