package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/policy"
	"github.com/tendant/voc-agent/internal/store"
)

// approvalTimeout bounds how long a turn waits for the user to decide.
const approvalTimeout = 2 * time.Minute

type escalateArgs struct {
	Question string `json:"question" desc:"The complete question to ask, self-contained, in the user's words. Do not copy private details into it."`
	Reason   string `json:"reason" desc:"One short sentence on why the cloud is needed"`
}

type escalateResult struct {
	Status string `json:"status"` // answered | denied | blocked
	Answer string `json:"answer,omitempty"`
	Model  string `json:"model,omitempty"`
	Detail string `json:"detail,omitempty"`
	say    string
}

func (r escalateResult) Say() string { return r.say }

// Escalate returns the ask_cloud tool. Every outcome — answered, denied,
// blocked by policy, or failed — is written to the audit log with the exact
// bytes that left the machine (none unless approved).
func Escalate(st *store.Store, c *cloud.Claude) Tool {
	var mu sync.Mutex
	blocked := map[string]int{} // turn key → times blocked (small; one entry per turn)
	return Typed("ask_cloud",
		"Ask a more capable cloud model (Claude) a question. Use it when the user asks to use the cloud or Claude, or when the question needs deep reasoning, analysis, or knowledge you don't have. The user must approve exactly what is sent; private notes are never sent.",
		func(ctx context.Context, call Call, a escalateArgs) (any, error) {
			audit := store.AuditEntry{TurnKey: call.TurnKey, Destination: c.Destination(), Model: c.Model, Reason: a.Reason}
			record := func(decision, detail string) {
				audit.Decision, audit.Detail = decision, detail
				st.AddAudit(context.WithoutCancel(ctx), audit)
			}

			// 1. Build the minimal, redacted payload.
			rel, err := st.RelevantNotes(ctx, a.Question, 3, 1500)
			if err != nil {
				return nil, err
			}
			var srcs []policy.Source
			for _, n := range rel {
				srcs = append(srcs, policy.Source{Title: n.Title, Path: n.Path, Privacy: n.Privacy, Text: n.Body})
			}
			plan := policy.Build(a.Question, srcs)
			params := c.Params(plan.System, plan.User)
			body, err := cloud.Body(params)
			if err != nil {
				return nil, err
			}
			audit.Proposed = string(body)

			// 2. Hard constraint: no local_only text, even if the model copied
			// it into the question.
			private, err := st.NotesWithPrivacy(ctx, store.LocalOnly)
			if err != nil {
				return nil, err
			}
			var others []store.RelevantNote
			for _, label := range []string{store.Personal, store.Public} {
				ns, err := st.NotesWithPrivacy(ctx, label)
				if err != nil {
					return nil, err
				}
				others = append(others, ns...)
			}
			dist := policy.NewDistinctive(private, others)
			plan.Warnings = dist.Except(dist.Find(plan.User), call.UserText) // the user's own words aren't a leak
			audit.Policy = plan.JSON()
			fp := policy.NewFingerprints(private)
			// Paraphrase isn't caught by fingerprints ("right ACL surgery on
			// September 30" for a note that says "right ACL, operated September
			// 30"). Two or more words found only in local-only notes get the
			// same first response as a copy: nothing is sent and the model must
			// rephrase. If it still uses them, they stay flagged in the dialog.
			mu.Lock()
			firstTry := blocked[call.TurnKey] == 0
			mu.Unlock()
			if firstTry && len(plan.Warnings) >= 2 {
				record("blocked", "mentions details found only in local-only notes: "+strings.Join(plan.Warnings, ", "))
				mu.Lock()
				blocked[call.TurnKey]++
				mu.Unlock()
				return nil, fmt.Errorf("blocked by privacy policy, nothing was sent: the question mentions details from the user's local-only notes (%s). "+
					"Call ask_cloud again with a general question that leaves out the user's private details (no conditions, dates, medications, names or schedules from their notes)", strings.Join(plan.Warnings, ", "))
			}
			if err := fp.CheckOutbound(body); err != nil {
				record("blocked", err.Error())
				mu.Lock()
				blocked[call.TurnKey]++
				n := blocked[call.TurnKey]
				mu.Unlock()
				if n == 1 {
					// Let the model try once more with a generalized question;
					// the user still approves whatever it writes.
					return nil, fmt.Errorf("blocked by privacy policy, nothing was sent: the question copied details from a local-only note (%q). "+
						"Call ask_cloud again with a general question that leaves out the user's private details (no names, dates, medications, or schedules from their notes)", err.(*policy.Violation).Note)
				}
				return escalateResult{Status: "blocked", Detail: "The question contained local-only information, so nothing was sent.",
					say: "I can't send that to the cloud because it includes information you marked as local only. Nothing was sent."}, nil
			}

			// 3. Explicit approval of the exact payload.
			if call.Approve == nil {
				record("denied", "no approval channel")
				return nil, errors.New("approval is not available, so nothing was sent")
			}
			actx, cancel := context.WithTimeout(ctx, approvalTimeout)
			ok, err := call.Approve(actx, ApprovalRequest{
				ID: newID(), Destination: c.Destination(), Model: c.Model, Reason: a.Reason,
				Payload: body, Policy: audit.Policy,
			})
			cancel()
			if err != nil || !ok {
				detail := "user denied"
				if err != nil {
					detail = "no decision: " + err.Error()
				}
				record("denied", detail)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return escalateResult{Status: "denied", Detail: "The user did not approve; nothing was sent.",
					say: "Okay, I won't send it. Nothing left your device."}, nil
			}

			// 4. Send; the middleware re-verifies the real bytes.
			var sent cloud.Sent
			ans, err := c.Ask(ctx, params, body, fp.CheckOutbound, &sent)
			audit.Sent, audit.SentSHA256, audit.Response = string(sent.Body), sent.SHA256, ans.Text
			if ans.Model != "" && ans.Model != c.Model {
				audit.Model = c.Model + " (served by " + ans.Model + ")"
			}
			if err != nil {
				record("error", err.Error())
				return nil, fmt.Errorf("cloud request failed: %v", err)
			}
			record("approved", "")
			return escalateResult{Status: "answered", Answer: ans.Text, Model: ans.Model, say: ans.Text}, nil
		})
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
