package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// scriptLLM replays one scripted reply per round and records requests.
type scriptLLM struct {
	rounds []llm.Message
	reqs   []llm.Request
}

func (s *scriptLLM) Chat(_ context.Context, req llm.Request, on func(string)) (llm.Message, error) {
	s.reqs = append(s.reqs, req)
	m := s.rounds[len(s.reqs)-1]
	if m.Content != "" {
		on(m.Content)
	}
	return m, nil
}

func call(name, args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Name: name, Arguments: json.RawMessage(args)}}
}

func TestTurnRunsToolsThenAnswers(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	reg := tools.NewRegistry(tools.Personal(st, nil, nil)...)
	f := &scriptLLM{rounds: []llm.Message{
		{Role: "assistant", Content: "Let me set that up.", ToolCalls: []llm.ToolCall{
			call("create_reminder", `{"text":"Review notes","when":"in 2 hours"}`),
			call("create_reminder", `{"text":"Review notes","when":"in 2 hours"}`), // duplicate call
		}},
		{Role: "assistant", Content: "Done, I'll remind you in two hours."},
	}}
	s := &Session{LLM: f, TTS: fakeTTS{}, Tools: reg, Memory: st}
	r := &rec{}
	if err := s.Turn(context.Background(), 1, "remind me to review notes in 2 hours", time.Now(), r); err != nil {
		t.Fatal(err)
	}
	if rs, _ := st.PendingReminders(context.Background()); len(rs) != 1 {
		t.Fatalf("want 1 reminder despite duplicate call, got %d", len(rs))
	}
	var calls, results int
	for _, e := range r.events {
		switch e.Type {
		case "tool_call":
			calls++
		case "tool_result":
			results++
			if !*e.OK {
				t.Fatalf("tool failed: %s", e.Result)
			}
		}
	}
	if calls != 2 || results != 2 || len(r.audio) != 2 {
		t.Fatalf("calls=%d results=%d audio=%d", calls, results, len(r.audio))
	}
	// Round 2 saw both tool results.
	last := f.reqs[1].Messages
	if last[len(last)-1].Role != "tool" || !strings.Contains(last[len(last)-1].Content, "already_exists") {
		t.Fatalf("tool results not passed back: %+v", last[len(last)-2:])
	}
	// History persisted to the store and restorable.
	s2 := &Session{LLM: f, TTS: fakeTTS{}, Memory: st}
	s2.Restore(context.Background())
	h := s2.History()
	if len(h) != 2 || h[1].Content != "Let me set that up. Done, I'll remind you in two hours." {
		t.Fatalf("restored history: %+v", h)
	}
}

func TestToolRoundsAreCapped(t *testing.T) {
	loop := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call("list_reminders", `{}`)}}
	f := &scriptLLM{rounds: []llm.Message{loop, loop, loop, loop, {Role: "assistant", Content: "You have no reminders."}}}
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	s := &Session{LLM: f, TTS: fakeTTS{}, Tools: tools.NewRegistry(tools.Personal(st, nil, nil)...)}
	if err := s.Turn(context.Background(), 1, "list", time.Now(), &rec{}); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != maxToolRounds+1 || f.reqs[maxToolRounds].Tools != nil {
		t.Fatalf("rounds=%d, last round must offer no tools", len(f.reqs))
	}
}

// TestGuardBlocksUnbackedClaim: the model claims a reminder without calling
// the tool. The claim must never reach the user; the model is pushed to call
// the tool and only the backed reply is spoken.
func TestGuardBlocksUnbackedClaim(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	f := &scriptLLM{rounds: []llm.Message{
		{Role: "assistant", Content: "Sure, I've set a reminder for tomorrow."}, // lie
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("create_reminder", `{"text":"Water plants","when":"tomorrow"}`)}},
		{Role: "assistant", Content: "Your reminder is set for tomorrow at nine."},
	}}
	s := &Session{LLM: f, TTS: fakeTTS{}, Tools: tools.NewRegistry(tools.Personal(st, nil, nil)...)}
	r := &rec{}
	if err := s.Turn(context.Background(), 1, "Remind me to water the plants tomorrow", time.Now(), r); err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	var guarded bool
	for _, e := range r.events {
		if e.Type == "llm_delta" {
			said.WriteString(e.Text)
		}
		guarded = guarded || e.Type == "guard"
	}
	if strings.Contains(said.String(), "I've set") {
		t.Fatalf("unbacked claim reached the user: %q", said.String())
	}
	if !guarded || len(r.audio) != 1 {
		t.Fatalf("guarded=%v audio=%d said=%q", guarded, len(r.audio), said.String())
	}
	if rs, _ := st.PendingReminders(context.Background()); len(rs) != 1 {
		t.Fatalf("reminders=%d", len(rs))
	}
	// The system nudge went to the model on round 2.
	m := f.reqs[1].Messages
	if !strings.Contains(m[len(m)-1].Content, "You have not called create_reminder") {
		t.Fatalf("no nudge: %+v", m[len(m)-1])
	}
}

func TestRequiredTools(t *testing.T) {
	for in, want := range map[string]bool{
		"remind me to call mom":                true,
		"Find my notes and remind me tomorrow": true,
		"set a reminder for 5pm":               true,
		"what's the weather":                   false,
	} {
		if got := len(requiredTools(in)) > 0; got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	for _, in := range []string{"Ask the cloud whether my plan is risky", "can you check with Claude", "use the cloud for this"} {
		if r := requiredTools(in); len(r) != 1 || r[0] != "ask_cloud" {
			t.Errorf("%q: %v", in, r)
		}
	}
	for _, in := range []string{"what reminders do I have", "list my reminders", "Do I have any reminders?", "show upcoming reminders"} {
		if r := requiredTools(in); len(r) != 1 || r[0] != "list_reminders" {
			t.Errorf("%q: %v", in, r)
		}
	}
}

// sayTool returns a result that should be spoken verbatim.
type sayOut struct{ A string }

func (s sayOut) Say() string { return s.A }

func TestToolSayEndsTurnVerbatim(t *testing.T) {
	type noArgs struct{}
	approvals := 0
	reg := tools.NewRegistry(tools.Typed("ask_cloud", "d", func(ctx context.Context, c tools.Call, _ noArgs) (any, error) {
		ok, _ := c.Approve(ctx, tools.ApprovalRequest{ID: "x"})
		if !ok {
			return nil, nil
		}
		return sayOut{"The cloud says: use a canary."}, nil
	}))
	f := &scriptLLM{rounds: []llm.Message{
		{Role: "assistant", Content: "Checking with the cloud.", ToolCalls: []llm.ToolCall{call("ask_cloud", `{}`)}},
		{Role: "assistant", Content: "SHOULD NOT RUN"},
	}}
	s := &Session{LLM: f, TTS: fakeTTS{}, Tools: reg,
		Approver: func(_ context.Context, turn uint32, _ Sink, req tools.ApprovalRequest) (bool, error) {
			approvals++
			return turn == 7 && req.ID == "x", nil
		}}
	r := &rec{}
	if err := s.Turn(context.Background(), 7, "ask the cloud about canaries", time.Now(), r); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 || approvals != 1 {
		t.Fatalf("rounds=%d approvals=%d", len(f.reqs), approvals)
	}
	done := r.events[len(r.events)-2]
	if done.Type != "assistant_done" || done.Text != "Checking with the cloud. The cloud says: use a canary." {
		t.Fatalf("done: %+v", done)
	}
}
