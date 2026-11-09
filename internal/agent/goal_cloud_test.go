package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// TestGoalWithClaudePlanner: goal mode planning on (fake) Claude. A recalled
// local_only note never reaches the API, emails are redacted, and a request
// the guard blocks falls back to the local model.
func TestGoalWithClaudePlanner(t *testing.T) {
	h := newGoalHarness(t)
	api := cloud.NewFakeAPI("")
	api.Replies = []string{
		planJSON,
		`{"calls":[{"query":"deploy"}]}`,
		// step 2 details and the report contain the "local_only" text, so
		// the guard blocks them and they run locally
	}
	defer api.Close()
	c := cloud.New("", option.WithBaseURL(api.URL()), option.WithAPIKey("k"), option.WithMaxRetries(0))
	blocked := 0
	audited := 0
	p := &cloud.Planner{C: c,
		Guard: func(body []byte) error {
			if strings.Contains(string(body), "silence the lag alert") { // pretend that's local_only text
				blocked++
				return errors.New("local-only")
			}
			return nil
		},
		Audit: func(cloud.Sent, string, error) { audited++ }}
	local := &scriptLLM{rounds: []llm.Message{
		content(`{"calls":[{"text":"rotate the Stripe secret"},{"text":"silence the lag alert"}]}`),
		content("I read your notes and set two reminders."),
	}}
	s := h.session(nil)
	s.LLM = local
	s.Planner, s.PlannerThinks, s.PlannerCloud = llm.Fallback{Primary: p, Secondary: local}, true, true
	s.Recall = func(context.Context, string) ([]Recalled, error) {
		return []Recalled{
			{Title: "Health", Body: "knee surgery recovery, physical therapy Mondays", Privacy: store.LocalOnly},
			{Title: "Deploy", Body: "Ask priya.k@example.com about the Stripe secret", Privacy: store.Personal},
		}, nil
	}
	r := &rec{}
	s.Turn(context.Background(), 1, "Get me ready for Thursday's deploy.", time.Now(), r)

	if len(h.acts) != 2 || h.approved != 1 {
		t.Fatalf("acts=%v approved=%d said=%q", h.acts, h.approved, spokenText(r))
	}
	for i, b := range api.Requests() {
		body := string(b)
		if strings.Contains(body, "knee surgery") || strings.Contains(body, "physical therapy") {
			t.Fatalf("request %d carried a local_only note", i)
		}
		if strings.Contains(body, "priya.k@example.com") {
			t.Fatalf("request %d not redacted", i)
		}
	}
	if !strings.Contains(spokenText(r), "set two reminders") {
		t.Fatalf("said %q", spokenText(r))
	}
	// plan + step 1 went to Claude; step 2 + report were blocked and ran locally.
	if len(api.Requests()) != 2 || blocked != 2 || audited != 4 || len(local.reqs) != 2 {
		t.Fatalf("cloud requests=%d blocked=%d audited=%d local calls=%d", len(api.Requests()), blocked, audited, len(local.reqs))
	}
}

var _ = tools.Call{}
