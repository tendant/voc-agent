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
	yesRe = regexp.MustCompile(`^(yes|yeah|yep|approve[ds]?|approval|send( it)?|go ahead|ok(ay)?|sure|do it|confirm(ed)?)\b`)
	noRe  = regexp.MustCompile(`^(no|nope|cancel(led)?|deny|denied|don'?t|do not|stop|never ?mind|reject(ed)?)\b`)
)

// voiceDecision maps an utterance to approve (true) or deny (false).
func voiceDecision(text string) (approve, ok bool) {
	t := strings.ToLower(strings.TrimSpace(strings.Trim(text, " .!?,")))
	switch {
	case yesRe.MatchString(t):
		return true, true
	case noRe.MatchString(t):
		return false, true
	}
	return false, false
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
		if a, err := s.TTS.Synthesize("This needs Claude in the cloud. Check what will be sent on screen, then say approve or cancel."); err == nil {
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
