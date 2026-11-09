package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

type pendingApproval struct {
	id   string
	turn uint32
	ch   chan bool
}

var (
	// Approving sends data, so it is matched strictly: exact words plus known
	// ASR mishearings of an isolated "approve" ("Prove.", "A prove.").
	yesRe = regexp.MustCompile(`^(yes|yeah|yep|approve[ds]?|aprove[ds]?|(a ?)?prove[ds]?|approval|send( it)?|go ahead|ok(ay)?|sure|do it|confirm(ed)?)\b`)
	// Includes how ASR tends to mishear an isolated "cancel".
	noRe = regexp.MustCompile(`^(no|nope|cancel(led|s)?|canceled|council|counsel|consul|cancer|deny|denied|don'?t|do not|stop|never ?mind|reject(ed)?)\b`)
)

// voiceDecision maps an utterance to approve (true) or deny (false). ASR is
// weakest on isolated words, so short replies are matched fuzzily, but only
// toward "cancel": a misheard cancel is harmless, a misheard approve is not.
func voiceDecision(text string) (approve, ok bool) {
	t := strings.ToLower(strings.TrimSpace(strings.Trim(text, " .!?,")))
	switch {
	case t == "":
		return false, false
	case noRe.MatchString(t):
		return false, true
	case yesRe.MatchString(t):
		return true, true
	}
	words := strings.FieldsFunc(t, func(r rune) bool { return r == ' ' || r == ',' || r == '.' || r == '!' })
	if len(words) > 3 {
		return false, false
	}
	for _, w := range words {
		if near(w, "cancel") || near(w, "deny") {
			return false, true
		}
	}
	return false, false
}

// isShortReply reports whether an undecided utterance during an approval is
// probably a misheard answer rather than a new request.
func isShortReply(text string) bool { return len(strings.Fields(text)) <= 3 }

// near reports an edit distance of at most 2 (and not a tiny word).
func near(a, b string) bool {
	if len(a) < 3 {
		return false
	}
	return editDistance(a, b) <= 2
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// reprompt asks again when an answer to a pending approval wasn't understood.
func (s *Server) reprompt(p *pendingApproval, sink agent.Sink) {
	sink.Event(agent.Event{Type: "approval_reprompt", Turn: p.turn, Text: "Say approve or cancel."})
	if s.TTS != nil {
		if a, err := s.TTS.Synthesize("Sorry, I didn't catch that. Say approve, or cancel."); err == nil {
			sink.Audio(p.turn, a)
		}
	}
}

// Approve implements agent.Session.Approver: it shows the request to the
// user and waits for a button press or a spoken "approve"/"cancel".
func (s *Server) Approve(ctx context.Context, turn uint32, sink agent.Sink, req tools.ApprovalRequest) (bool, error) {
	p := &pendingApproval{id: req.ID, turn: turn, ch: make(chan bool, 1)}
	s.mu.Lock()
	if s.approvals == nil {
		s.approvals = map[string]*pendingApproval{}
	}
	s.approvals[p.id] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.approvals, p.id)
		s.mu.Unlock()
	}()

	b, _ := json.Marshal(req)
	sink.Event(agent.Event{Type: "approval_request", Turn: turn, Result: b})
	if s.TTS != nil {
		prompt := req.Prompt
		if prompt == "" {
			prompt = "This needs Claude in the cloud. Check what will be sent on screen, then say approve or cancel."
		}
		if a, err := s.TTS.Synthesize(prompt); err == nil {
			sink.Audio(turn, a)
		}
	}
	var ok bool
	var err error
	select {
	case ok = <-p.ch:
	case <-ctx.Done():
		err = ctx.Err()
	}
	res, _ := json.Marshal(map[string]any{"id": p.id, "approved": ok})
	sink.Event(agent.Event{Type: "approval_resolved", Turn: turn, Result: res, OK: &ok})
	return ok, err
}

// pending returns the newest outstanding approval, if any.
func (s *Server) pending() *pendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.approvals { // single user: at most one in practice
		return p
	}
	return nil
}

func (s *Server) resolve(id string, ok bool) bool {
	s.mu.Lock()
	p := s.approvals[id]
	s.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case p.ch <- ok:
		return true
	default:
		return false
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "no store", http.StatusNotFound)
		return
	}
	es, err := s.Store.RecentAudit(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if es == nil {
		es = []store.AuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(es)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"cloud": s.Cloud})
}
