package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

func TestVoiceDecision(t *testing.T) {
	for in, want := range map[string]int{"Approve.": 1, "yes, send it": 1, "Go ahead!": 1, "okay": 1,
		"Prove.": 1, "A prove.": 1, "approved": 1, "Aprove it": 1,
		"No.": 0, "cancel": 0, "Cancer.": 0, "Council.": 0, "Cencil.": 0, "improve": -1, "don't send it": 0, "never mind": 0,
		"what is this": -1, "": -1, "tell me about the weather today": -1, "a": -1} {
		ok, decided := voiceDecision(in)
		got := -1
		if decided {
			got = map[bool]int{true: 1, false: 0}[ok]
		}
		if got != want {
			t.Errorf("%q: %d want %d", in, got, want)
		}
	}
}

// cloudLLM always asks the cloud once, then would say something else.
type cloudLLM struct{ round int }

func (c *cloudLLM) Chat(_ context.Context, req llm.Request, on func(string)) (llm.Message, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		on("Fine.")
		return llm.Message{Role: "assistant", Content: "Fine."}, nil
	}
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{
		Name: "ask_cloud", Arguments: json.RawMessage(`{"question":"Is a 5% canary enough?","reason":"needs judgment"}`)}}}}, nil
}

func TestApprovalOverWebSocket(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "private"), 0o755)
	os.WriteFile(filepath.Join(dir, "private/x.md"), []byte("secret canary codename bluebird rollout"), 0o644)
	st.SyncNotes(context.Background(), dir)
	api := cloud.NewFakeAPI("Five percent is fine if you watch error rates.")
	defer api.Close()
	c := cloud.New("", option.WithBaseURL(api.URL()), option.WithAPIKey("k"), option.WithMaxRetries(0))

	srv := &Server{TTS: toneTTS{}, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv.Agent = &agent.Session{LLM: &cloudLLM{}, TTS: toneTTS{}, Tools: tools.NewRegistry(tools.Escalate(st, c)), Approver: srv.Approve}
	cl := dial(t, srv)

	for i, decision := range []string{"approve", "no thanks"} {
		turn := uint32(i + 1) // answering an approval does not start a turn
		cl.send(`{"type":"text","text":"ask the cloud if a 5% canary is enough"}`)
		evs, _ := cl.until("approval_request", turn)
		req := find(evs, "approval_request")
		var ar tools.ApprovalRequest
		json.Unmarshal(req.Result, &ar)
		if !strings.Contains(string(ar.Payload), "Is a 5% canary enough?") || strings.Contains(string(ar.Payload), "bluebird") {
			t.Fatalf("payload: %s", ar.Payload)
		}
		if len(api.Requests()) != i {
			t.Fatal("sent before approval")
		}
		cl.send(`{"type":"text","text":"` + decision + `"}`)
		evs, _ = cl.until("assistant_done", turn)
		done := find(evs, "assistant_done")
		if i == 0 && done.Text != "Five percent is fine if you watch error rates." {
			t.Fatalf("approved reply: %+v", done)
		}
		if i == 1 && (!strings.Contains(done.Text, "won't send") || len(api.Requests()) != 1) {
			t.Fatalf("denied reply: %+v, requests %d", done, len(api.Requests()))
		}
		cl.until("timing", turn)
	}
	a, _ := st.RecentAudit(context.Background(), 10)
	if len(a) != 2 || a[0].Decision != "denied" || a[1].Decision != "approved" || a[1].Sent == "" {
		t.Fatalf("audit: %+v", a)
	}
}
