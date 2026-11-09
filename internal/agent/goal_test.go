package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

func content(s string) llm.Message { return llm.Message{Role: "assistant", Content: s} }

type goalHarness struct {
	st       *store.Store
	reg      *tools.Registry
	reads    int
	mu       sync.Mutex
	acts     map[string]string // idempotency key → text: a side effect, deduplicated like the real tools
	approved int
	cancelOn int // cancel the turn on this act call (simulates a crash), 0 = never
	actCalls int
	cancel   context.CancelFunc
}

func newGoalHarness(t *testing.T) *goalHarness {
	h := &goalHarness{acts: map[string]string{}}
	h.st, _ = store.Open(filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { h.st.Close() })
	type q struct {
		Query string `json:"query"`
	}
	type act struct {
		Text string `json:"text"`
	}
	h.reg = tools.NewRegistry(
		tools.Typed("read_notes", "Read notes.", func(context.Context, tools.Call, q) (any, error) {
			h.reads++
			return map[string]any{"open_questions": []string{"rotate the Stripe secret", "silence the lag alert"}}, nil
		}).ReadOnly(),
		tools.Typed("remind", "Create a reminder.", func(_ context.Context, c tools.Call, a act) (any, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.actCalls++
			if _, dup := h.acts[c.TurnKey]; dup {
				return map[string]string{"status": "already_exists"}, nil
			}
			h.acts[c.TurnKey] = a.Text
			if h.actCalls == h.cancelOn && h.cancel != nil {
				h.cancel() // "crash" right after the side effect happened
			}
			return map[string]string{"status": "created", "text": a.Text}, nil
		}),
	)
	return h
}

func (h *goalHarness) session(f *scriptLLM) *Session {
	return &Session{LLM: f, TTS: fakeTTS{}, Tools: h.reg, Tasks: h.st,
		Approver: func(context.Context, uint32, Sink, tools.ApprovalRequest) (bool, error) {
			h.approved++
			return true, nil
		}}
}

const planJSON = `{"can_do":true,"summary":"I'll read your notes and set a reminder for each open question.","steps":[
	{"tool":"read_notes","intent":"find the open questions"},{"tool":"remind","intent":"a reminder for each open question"}]}`

func TestGoalPlansApprovesAndActs(t *testing.T) {
	h := newGoalHarness(t)
	f := &scriptLLM{rounds: []llm.Message{
		content(planJSON),
		content(`{"calls":[{"query":"deploy"}]}`),
		content(`{"calls":[{"text":"rotate the Stripe secret"},{"text":"silence the lag alert"}]}`),
		content("I read your notes and set two reminders, one for each open question."),
	}}
	s := h.session(f)
	r := &rec{}
	s.Turn(context.Background(), 1, "Get me ready for Thursday's deploy.", time.Now(), r)

	if !f.reqs[0].Think || f.reqs[0].Format == nil || f.reqs[1].Think || f.reqs[1].Format == nil {
		t.Fatal("planner must think with a schema; argument filling uses a schema without thinking")
	}
	if h.approved != 1 || h.reads != 1 || len(h.acts) != 2 {
		t.Fatalf("approved=%d reads=%d acts=%v", h.approved, h.reads, h.acts)
	}
	said := spokenText(r)
	if !strings.HasPrefix(said, "Let me plan that. On it.") || !strings.Contains(said, "set two reminders") {
		t.Fatalf("said %q", said)
	}
	ts, _ := h.st.RecentTasks(context.Background(), 1)
	var p goalPlan
	json.Unmarshal(ts[0].Plan, &p)
	if ts[0].Status != "done" || p.Steps[0].Status != "done" || len(p.Steps[1].Calls) != 2 {
		t.Fatalf("journal: %s %s", ts[0].Status, ts[0].Plan)
	}
}

func TestGoalDeniedOrImpossible(t *testing.T) {
	h := newGoalHarness(t)
	f := &scriptLLM{rounds: []llm.Message{content(planJSON)}}
	s := h.session(f)
	s.Approver = func(context.Context, uint32, Sink, tools.ApprovalRequest) (bool, error) { return false, nil }
	r := &rec{}
	s.Turn(context.Background(), 1, "Get me ready for Thursday's deploy.", time.Now(), r)
	if h.reads != 0 || len(h.acts) != 0 || !strings.Contains(spokenText(r), "won't do it") {
		t.Fatalf("denied plan ran: reads=%d acts=%d said=%q", h.reads, len(h.acts), spokenText(r))
	}
	if ts, _ := h.st.RecentTasks(context.Background(), 1); ts[0].Status != "cancelled" {
		t.Fatalf("status %s", ts[0].Status)
	}

	f2 := &scriptLLM{rounds: []llm.Message{content(`{"can_do":false,"reason":"I can't book flights.","summary":"","steps":[]}`)}}
	r = &rec{}
	h.session(f2).Turn(context.Background(), 2, "Take care of booking my flight to Lisbon.", time.Now(), r)
	if !strings.Contains(spokenText(r), "can't book flights") {
		t.Fatalf("said %q", spokenText(r))
	}
}

func TestGoalReportCannotLie(t *testing.T) {
	h := newGoalHarness(t)
	f := &scriptLLM{rounds: []llm.Message{
		content(`{"can_do":true,"summary":"I'll read your notes.","steps":[{"tool":"read_notes","intent":"look"}]}`),
		content(`{"calls":[{"query":"x"}]}`),
		content("I read your notes and I've set a reminder for tomorrow."), // no reminder was made
	}}
	r := &rec{}
	h.session(f).Turn(context.Background(), 1, "Help me prepare for the deploy.", time.Now(), r)
	if said := spokenText(r); strings.Contains(said, "set a reminder") || !strings.Contains(said, "finished all 1 steps") {
		t.Fatalf("said %q", said)
	}
	if h.approved != 0 {
		t.Fatal("a read-only plan shouldn't ask for approval")
	}
}

// TestGoalResumeDoesNotRepeat: the task is cut off right after the first
// reminder was created. Resuming re-runs only unfinished work, and the
// in-flight reminder isn't created twice (same idempotency key).
func TestGoalResumeDoesNotRepeat(t *testing.T) {
	h := newGoalHarness(t)
	h.cancelOn = 1
	f := &scriptLLM{rounds: []llm.Message{
		content(planJSON),
		content(`{"calls":[{"query":"deploy"}]}`),
		content(`{"calls":[{"text":"rotate the Stripe secret"},{"text":"silence the lag alert"}]}`),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.session(f).Turn(ctx, 1, "Get me ready for Thursday's deploy.", time.Now(), &rec{})
	if len(h.acts) != 1 {
		t.Fatalf("before the crash: %v", h.acts)
	}
	h.st.MarkInterrupted(context.Background()) // what startup does
	ts, _ := h.st.RecentTasks(context.Background(), 1)
	if ts[0].Status != "interrupted" {
		t.Fatalf("status %s", ts[0].Status)
	}

	// After "restart": a new session resumes.
	f2 := &scriptLLM{rounds: []llm.Message{
		content(`{"calls":[{"text":"rotate the Stripe secret"},{"text":"silence the lag alert"}]}`),
		content("I set reminders for both open questions."),
	}}
	r := &rec{}
	h.session(f2).Turn(context.Background(), 2, "Continue my last task.", time.Now(), r)
	if h.reads != 1 {
		t.Fatalf("finished read step ran again: %d", h.reads)
	}
	if len(h.acts) != 2 {
		t.Fatalf("want 2 distinct reminders, got %v", h.acts)
	}
	ts, _ = h.st.RecentTasks(context.Background(), 1)
	if ts[0].Status != "done" || !strings.Contains(spokenText(r), "pick up where I left off") && h.approved < 2 {
		t.Fatalf("resume: status=%s said=%q approvals=%d", ts[0].Status, spokenText(r), h.approved)
	}
}

func TestGoalIntents(t *testing.T) {
	for in, want := range map[string]bool{
		"Get me ready for Thursday's deploy.": true, "Help me prepare for my one-on-one with Maya": true,
		"Victor, take care of my morning": true, "Can you plan my week?": true, "Organize my notes": true,
		"Remind me to call mom": false, "What's the rollback plan?": false, "play some jazz": false,
	} {
		if got := goalIntent.MatchString(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	for in, want := range map[string]bool{"Continue my last task": true, "resume the task": true, "finish what you were doing": true, "continue playing": false} {
		if got := resumeIntent.MatchString(in); got != want {
			t.Errorf("resume %q: %v", in, got)
		}
	}
}

func TestPlanPostProcessing(t *testing.T) {
	h := newGoalHarness(t)
	// Five separate reminder steps, as a small planner tends to write.
	f := &scriptLLM{rounds: []llm.Message{
		content(`{"can_do":true,"summary":"x","steps":[{"tool":"read_notes","intent":"look"},
			{"tool":"remind","intent":"a"},{"tool":"remind","intent":"b"},{"tool":"remind","intent":"c"}]}`),
	}}
	s := h.session(f)
	p, err := s.plan(context.Background(), goalEnv{}, "get me ready", nil)
	if err != nil || len(p.Steps) != 2 || p.Steps[1].Intent != "a; b; c" {
		t.Fatalf("merged plan: %+v %v", p.Steps, err)
	}
}
