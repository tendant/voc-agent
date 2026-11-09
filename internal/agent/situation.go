package agent

import (
	"context"
	"strings"
	"time"
)

// The situation is what's true right now, outside the conversation: the
// time, how the user is talking, what's open or playing, upcoming reminders,
// an unfinished task. It is given to the model each turn as one message just
// before the user's words, so replies like "pause it" or "what's this?" have
// something to refer to, and so the system prompt stays identical across
// turns (the model server can then reuse its cached prompt).

type inputModeKey struct{}

// Input modes, as told to the model.
const (
	ModeTyped     = "typed"
	ModeVoice     = "push-to-talk voice"
	ModeHandsFree = "hands-free voice"
)

// WithInputMode records how this turn's request arrived.
func WithInputMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, inputModeKey{}, mode)
}

func inputMode(ctx context.Context) string {
	m, _ := ctx.Value(inputModeKey{}).(string)
	return m
}

// situationBudget bounds gathering; anything slower is left out.
const situationBudget = 300 * time.Millisecond

// situation renders the situation message for this turn.
func (s *Session) situation(ctx context.Context) string {
	lines := []string{"It is " + s.now().Format("Monday, January 2, 2006, 3:04 PM MST") + "."}
	switch inputMode(ctx) {
	case ModeTyped:
		lines = append(lines, "The user typed this message.")
	case ModeVoice, ModeHandsFree:
		lines = append(lines, "The user is speaking ("+inputMode(ctx)+"); their words were transcribed and may contain recognition errors.")
	}
	if s.Situation != nil {
		sctx, cancel := context.WithTimeout(ctx, situationBudget)
		for _, l := range s.Situation(sctx) {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		cancel()
	}
	return "Current situation (use it to understand the request; mention it only if relevant, and don't guess at reasons it doesn't state):\n- " + strings.Join(lines, "\n- ")
}

// UserText returns the user's own words from a stored user message (which
// also carries that turn's context block).
func UserText(content string) string {
	if _, words, ok := strings.Cut(content, "\n[The user's message]\n"); ok {
		return words
	}
	return content
}
