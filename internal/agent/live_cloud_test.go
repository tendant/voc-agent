package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/policy"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// TestLiveCloudEscalation runs the M3 exit criterion with local Qwen3 and a
// fake cloud API: questions touching private notes are escalated (with
// approval) and nothing local_only ever reaches the API. VOC_LIVE=1.
func TestLiveCloudEscalation(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SyncNotes(ctx, "../../notes")
	api := cloud.NewFakeAPI("Here is the cloud's answer.")
	defer api.Close()
	c := cloud.New("", option.WithBaseURL(api.URL()), option.WithAPIKey("k"), option.WithMaxRetries(0))
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	approve := true
	s := &Session{LLM: ol, TTS: fakeTTS{}, Recall: StoreRecall(st),
		Tools: tools.NewRegistry(append(tools.Personal(st, nil, nil), tools.Escalate(st, c))...),
		Approver: func(_ context.Context, _ uint32, _ Sink, req tools.ApprovalRequest) (bool, error) {
			t.Logf("   APPROVAL? %s", req.Reason)
			return approve, nil
		}}

	private, _ := st.NotesWithPrivacy(ctx, store.LocalOnly)
	fp := policy.NewFingerprints(private)
	for i, step := range []struct {
		say     string
		approve bool
	}{
		{"Ask the cloud to make me a weekly workout plan that fits my knee recovery.", true},
		{"Ask Claude to review my billing deployment plan for risks.", true},
		{"Use the cloud to explain the CAP theorem.", false},
	} {
		approve = step.approve
		before := len(api.Requests())
		r := &rec{}
		t0 := time.Now()
		if err := s.Turn(ctx, uint32(i+1), step.say, t0, r); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		var reply, trace string
		for _, e := range r.events {
			switch e.Type {
			case "assistant_done":
				reply = e.Text
			case "tool_call":
				trace += " " + e.Name + string(e.Args)
			case "guard":
				trace += " GUARD"
			}
		}
		t.Logf("USER  %s", step.say)
		t.Logf("TOOLS%s", trace)
		t.Logf("VOC   %s (%v)", reply, time.Since(t0).Round(10*time.Millisecond))
		reqs := api.Requests()
		for _, b := range reqs[before:] {
			if err := fp.CheckOutbound(b); err != nil {
				t.Errorf("turn %d LEAK: %v", i+1, err)
			}
			for _, w := range []string{"Okafor", "ACL", "Ibuprofen", "priya.k@example.com"} {
				if strings.Contains(string(b), w) {
					t.Errorf("turn %d leaked %q", i+1, w)
				}
			}
			t.Logf("SENT  %d bytes; user content: %.300q", len(b), userText(b))
		}
		if !strings.Contains(trace, "ask_cloud") {
			t.Errorf("turn %d: ask_cloud not called", i+1)
		}
		if !step.approve && len(reqs) != before {
			t.Errorf("turn %d: sent despite denial", i+1)
		}
	}
	audit, _ := st.RecentAudit(ctx, 10)
	for _, a := range audit {
		t.Logf("AUDIT %s %s sent=%dB %s", a.Decision, a.Reason, len(a.Sent), a.Detail)
	}
}

func userText(body []byte) string {
	s := string(body)
	if i := strings.Index(s, `"text":"`); i >= 0 {
		if j := strings.Index(s[i:], `"text":"`+"You are"); j < 0 {
			return s[i+8:]
		}
	}
	return s
}
