package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/store"
)

func escalationSetup(t *testing.T) (*store.Store, *cloud.FakeAPI, *Registry) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "private"), 0o755)
	os.MkdirAll(filepath.Join(dir, "work"), 0o755)
	os.WriteFile(filepath.Join(dir, "private/health.md"), []byte("# Health\nMy knee surgery recovery: no running until March, physical therapy twice a week."), 0o644)
	os.WriteFile(filepath.Join(dir, "work/deploy.md"), []byte("# Deploy plan\nCanary at 5%, then two waves. Pager: oncall@corp.com, 415-555-0134."), 0o644)
	st.SyncNotes(ctx, dir)
	f := cloud.NewFakeAPI("Start with a canary and define rollback triggers up front.")
	t.Cleanup(f.Close)
	c := cloud.New("", option.WithBaseURL(f.URL()), option.WithAPIKey("test"), option.WithMaxRetries(0))
	return st, f, NewRegistry(Escalate(st, c))
}

func TestEscalateApproved(t *testing.T) {
	st, f, r := escalationSetup(t)
	var shown ApprovalRequest
	call := Call{TurnKey: "t1", UserText: "review my canary deploy plan and my knee surgery recovery", Approve: func(_ context.Context, req ApprovalRequest) (bool, error) {
		shown = req
		return true, nil
	}}
	res := r.Run(context.Background(), call, "ask_cloud",
		json.RawMessage(`{"question":"Review my canary deploy plan and its waves, and my knee surgery recovery","reason":"needs analysis"}`))
	if !res.OK || res.Say != "Start with a canary and define rollback triggers up front." {
		t.Fatalf("result %s say=%q", res.JSON, res.Say)
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests: %d", len(reqs))
	}
	sent := string(reqs[0])
	for _, leak := range []string{"physical therapy", "no running", "oncall@corp.com", "555-0134"} {
		if strings.Contains(sent, leak) {
			t.Errorf("leaked %q", leak)
		}
	}
	if !strings.Contains(sent, "Canary at 5%") || !strings.Contains(sent, "[EMAIL]") {
		t.Errorf("personal context missing or unredacted: %s", sent)
	}
	if !strings.Contains(string(shown.Payload), "Canary at 5%") || !strings.Contains(string(shown.Policy), `"private/health.md"`) {
		t.Errorf("approval did not show payload/exclusions: %s %s", shown.Payload, shown.Policy)
	}
	a, _ := st.RecentAudit(context.Background(), 5)
	if len(a) != 1 || a[0].Decision != "approved" || a[0].Sent != sent || a[0].Response == "" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestEscalateDeniedSendsNothing(t *testing.T) {
	st, f, r := escalationSetup(t)
	call := Call{TurnKey: "t", Approve: func(context.Context, ApprovalRequest) (bool, error) { return false, nil }}
	res := r.Run(context.Background(), call, "ask_cloud", json.RawMessage(`{"question":"Is a canary enough?","reason":"r"}`))
	if !res.OK || !strings.Contains(string(res.JSON), `"denied"`) || len(f.Requests()) != 0 {
		t.Fatalf("%s, %d requests", res.JSON, len(f.Requests()))
	}
	a, _ := st.RecentAudit(context.Background(), 5)
	if len(a) != 1 || a[0].Decision != "denied" || a[0].Sent != "" || a[0].Proposed == "" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestEscalateBlocksCopiedPrivateText(t *testing.T) {
	st, f, r := escalationSetup(t)
	asked := false
	call := Call{Approve: func(context.Context, ApprovalRequest) (bool, error) { asked = true; return true, nil }}
	call.TurnKey = "t9"
	// The local model pasted the private note into the question.
	leaky := json.RawMessage(`{"question":"I have no running until March, physical therapy twice a week. What exercise is ok?","reason":"r"}`)
	res := r.Run(context.Background(), call, "ask_cloud", leaky)
	if res.OK || !strings.Contains(string(res.JSON), "Call ask_cloud again with a general question") || asked {
		t.Fatalf("first block should ask the model to rephrase: %s asked=%v", res.JSON, asked)
	}
	res = r.Run(context.Background(), call, "ask_cloud", leaky) // still leaky
	if !res.OK || !strings.Contains(string(res.JSON), `"blocked"`) || res.Say == "" || asked || len(f.Requests()) != 0 {
		t.Fatalf("second block should end the turn: %s asked=%v", res.JSON, asked)
	}
	a, _ := st.RecentAudit(context.Background(), 5)
	if len(a) != 2 || a[0].Decision != "blocked" || !strings.Contains(a[0].Detail, "Health") {
		t.Fatalf("audit: %+v", a)
	}
	// A paraphrased question passes the hard check but is flagged for the user.
	var shown ApprovalRequest
	call.Approve = func(_ context.Context, req ApprovalRequest) (bool, error) { asked, shown = true, req; return true, nil }
	res = r.Run(context.Background(), call, "ask_cloud", json.RawMessage(`{"question":"What exercise is safe while I avoid running until March after knee surgery?","reason":"r"}`))
	if !res.OK || !asked || len(f.Requests()) != 1 {
		t.Fatalf("general question should be sent after approval: %s", res.JSON)
	}
	if !strings.Contains(string(shown.Policy), `"warnings":["knee","march","running","surgery"]`) {
		t.Fatalf("paraphrase not flagged: %s", shown.Policy)
	}
}

func TestEscalateBlocksParaphraseOnce(t *testing.T) {
	_, f, r := escalationSetup(t)
	asked := 0
	call := Call{TurnKey: "p1", UserText: "make me a workout plan", Approve: func(context.Context, ApprovalRequest) (bool, error) { asked++; return true, nil }}
	// Paraphrased private details: no 6-word copy, but several private-only words.
	para := json.RawMessage(`{"question":"Plan workouts given knee surgery recovery with therapy twice weekly and no running until March","reason":"r"}`)
	res := r.Run(context.Background(), call, "ask_cloud", para)
	if res.OK || !strings.Contains(string(res.JSON), "local-only notes") || asked != 0 || len(f.Requests()) != 0 {
		t.Fatalf("paraphrase should be pushed back first: %s", res.JSON)
	}
	// A general rephrasing goes to approval.
	res = r.Run(context.Background(), call, "ask_cloud", json.RawMessage(`{"question":"What are good low-impact weekly workouts?","reason":"r"}`))
	if !res.OK || asked != 1 {
		t.Fatalf("general question: %s", res.JSON)
	}
}
