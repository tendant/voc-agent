// Package agent runs conversation turns: user text → LLM (+ tools) →
// sentences → TTS.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
	"github.com/tendant/voc-agent/internal/voice"
)

// Event is a JSON message for the client. Turn identifies the turn so the
// client can drop output from turns it has abandoned.
type Event struct {
	Type   string          `json:"type"`
	Turn   uint32          `json:"turn,omitempty"`
	Text   string          `json:"text,omitempty"`
	Lang   string          `json:"lang,omitempty"`
	Final  bool            `json:"final,omitempty"`
	MS     map[string]int  `json:"ms,omitempty"`
	Error  string          `json:"error,omitempty"`
	Name   string          `json:"name,omitempty"`   // tool_call, tool_result
	Args   json.RawMessage `json:"args,omitempty"`   // tool_call
	Result json.RawMessage `json:"result,omitempty"` // tool_result
	OK     *bool           `json:"ok,omitempty"`     // tool_result
}

// Sink receives a turn's output. Implementations must be safe for concurrent
// use: events and audio are sent from different goroutines.
type Sink interface {
	Event(Event) error
	Audio(turn uint32, a voice.Audio) error
}

// Recalled is a note given to the model as context.
type Recalled struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Body  string `json:"-"`
}

// Memory persists the conversation across restarts.
type Memory interface {
	RecentMessages(ctx context.Context, n int) ([]store.Message, error)
	AppendMessages(ctx context.Context, msgs ...store.Message) error
}

const basePrompt = `You are voc, a private voice assistant running entirely on the user's own computer.
Your replies are spoken aloud by a text-to-speech voice, so:
- Answer in one to three short sentences unless the user asks for more detail.
- Don't end with offers like "Let me know if you need anything else."
- Use plain conversational sentences. No markdown, lists, headings, emojis, file paths, or code.
- Write numbers, dates, and units the way you would say them.
- Always reply in English.
Never claim to have done something you have not done.`

const toolPrompt = `
You have tools for the user's private notes and reminders. Rules:
1. The user's notes are your only knowledge of their life. Notes that match the user's words are provided below their message. Answer questions about the user's own work, home, people, plans, or schedule only from those notes or from search_notes results, and mention the note's title. If the provided notes don't answer it, call search_notes with different keywords. If still nothing, say you couldn't find it in their notes. Never guess or invent details.
2. When the user asks to be reminded of something, you must call create_reminder, passing "when" exactly as the user said it. A reminder exists only after create_reminder returns status "created" or "already_exists"; then confirm it using the "due" text it returns. If it returns an error, tell the user briefly or ask when they meant.
3. If a request has several parts, make one tool call for each part, in order. For example: search_notes, then create_reminder.
4. If ask_cloud is available, use it when the user asks for the cloud or Claude, or when a question needs deep reasoning or knowledge you lack. Write a self-contained question and never copy private details into it. The user approves what is sent.
5. Never say file paths or tool names out loud.
You cannot use a calendar, email, or the internet.`

const noToolPrompt = `
You cannot take actions (no reminders, notes, calendar, or internet). If asked, say that isn't available yet.`

const maxToolRounds = 4

// Intents that make a tool call mandatory. Small local models sometimes
// reply "I'll remind you" without calling the tool, or answer "what
// reminders do I have" from stale conversation memory.
var (
	remindIntent = regexp.MustCompile(`(?i)\bremind (me|us)\b|\b(set|create|add|make|schedule) (me )?(a |an |another )?reminders?\b`)
	cloudIntent  = regexp.MustCompile(`(?i)\b(ask|use|check with|escalate to|send (it|this) to)( the)? (cloud|claude)\b`)
	listIntent   = regexp.MustCompile(`(?i)\b(what|which|any|list|show|read|upcoming|pending|my)\b.*\breminders\b|\breminders\b.*\b(do i|have i|are there)\b`)
)

// requiredTools returns tools the user's words make mandatory this turn; the
// turn loop refuses to finish until they have been called.
func requiredTools(user string) []string {
	switch {
	case cloudIntent.MatchString(user):
		return []string{"ask_cloud"}
	case remindIntent.MatchString(user):
		return []string{"create_reminder"}
	case listIntent.MatchString(user):
		return []string{"list_reminders"}
	}
	return nil
}

// Session holds one user's conversation. Turns are serialized.
type Session struct {
	LLM    llm.Chat
	TTS    voice.TTS
	Tools  *tools.Registry // nil: no tools
	Memory Memory          // nil: history is in-memory only
	System string          // overrides the built-in prompt
	// Recall finds notes relevant to what the user said; they are added to
	// the model's context every turn so answers don't depend on the model
	// remembering to search. May be nil.
	Recall func(ctx context.Context, user string) ([]Recalled, error)
	// Approver asks the user to allow a sensitive action (cloud escalation)
	// and blocks until they decide. Nil means every request is refused.
	Approver   func(ctx context.Context, turn uint32, sink Sink, req tools.ApprovalRequest) (bool, error)
	MaxHistory int // messages kept, excluding the system prompt; default 20
	Now        func() time.Time

	turnMu  sync.Mutex // one turn at a time
	mu      sync.Mutex // guards history
	history []llm.Message
}

// Restore loads recent conversation from Memory.
func (s *Session) Restore(ctx context.Context) error {
	if s.Memory == nil {
		return nil
	}
	msgs, err := s.Memory.RecentMessages(ctx, s.maxHistory())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = s.history[:0]
	for _, m := range msgs {
		s.history = append(s.history, llm.Message{Role: m.Role, Content: m.Content})
	}
	return nil
}

func (s *Session) maxHistory() int {
	if s.MaxHistory == 0 {
		return 20
	}
	return s.MaxHistory
}

func (s *Session) systemPrompt() string {
	p := s.System
	if p == "" {
		p = basePrompt
		if s.Tools != nil {
			p += toolPrompt
		} else {
			p += noToolPrompt
		}
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return p + "\nThe current local time is " + now().Format("Monday, January 2, 2006, 3:04 PM MST") + "."
}

// History returns a copy of the conversation so far.
func (s *Session) History() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Message(nil), s.history...)
}

func (s *Session) remember(ctx context.Context, msgs ...llm.Message) error {
	s.mu.Lock()
	s.history = append(s.history, msgs...)
	if n := len(s.history) - s.maxHistory(); n > 0 {
		s.history = append([]llm.Message(nil), s.history[n:]...)
	}
	s.mu.Unlock()
	if s.Memory == nil {
		return nil
	}
	var sm []store.Message
	for _, m := range msgs {
		sm = append(sm, store.Message{Role: m.Role, Content: m.Content})
	}
	// Persist even if the turn was cancelled.
	return s.Memory.AppendMessages(context.WithoutCancel(ctx), sm...)
}

func newTurnKey() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Turn answers one user utterance, streaming text deltas, tool activity and
// speech to sink. start is when the user finished speaking; timings are
// relative to it. Cancelling ctx stops generation, tools and speech; the
// partial reply is kept in history so the conversation stays coherent.
func (s *Session) Turn(ctx context.Context, turn uint32, user string, start time.Time, sink Sink) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	ms := func() int { return int(time.Since(start).Milliseconds()) }
	timing := map[string]int{}

	msgs := append([]llm.Message{{Role: "system", Content: s.systemPrompt()}}, s.History()...)
	msgs = append(msgs, llm.Message{Role: "user", Content: user})
	if s.Recall != nil {
		notes, err := s.Recall(ctx, user)
		if err != nil {
			sink.Event(Event{Type: "error", Turn: turn, Error: "recall: " + err.Error()})
		} else if len(notes) > 0 {
			var b strings.Builder
			b.WriteString("Notes from the user's files that may relate to their message. Use them only if relevant:\n")
			for _, n := range notes {
				fmt.Fprintf(&b, "\n--- Note: %s ---\n%s\n", n.Title, strings.TrimSpace(n.Body))
			}
			msgs = append(msgs, llm.Message{Role: "system", Content: b.String()})
			res, _ := json.Marshal(notes)
			sink.Event(Event{Type: "recall", Turn: turn, Result: res})
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sentences := make(chan string, 32)
	// Written only by the producer goroutine; read after sentences closes.
	var (
		spoken          strings.Builder // everything the assistant said
		llmErr          error
		llmTTFT, llmEnd = -1, -1
		toolMS          int
	)
	go func() {
		defer close(sentences)
		emit := func(ss []string) {
			for _, x := range ss {
				select {
				case sentences <- x:
				case <-ctx.Done():
				}
			}
		}
		call := tools.Call{TurnKey: newTurnKey()}
		if s.Approver != nil {
			call.Approve = func(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
				return s.Approver(ctx, turn, sink, req)
			}
		}
		var required []string
		for _, r := range requiredTools(user) {
			if s.Tools != nil && s.Tools.Has(r) { // e.g. ask_cloud only when cloud is enabled
				required = append(required, r)
			}
		}
		called := map[string]bool{}
		unmet := func() []string {
			var out []string
			for _, r := range required {
				if !called[r] {
					out = append(out, r)
				}
			}
			return out
		}
		for round := 0; ; round++ {
			req := llm.Request{Messages: msgs}
			if s.Tools != nil && round < maxToolRounds {
				req.Tools = s.Tools.Specs()
			}
			// While a required action is outstanding, hold this round's text
			// until we know it came with a tool call; otherwise a claim like
			// "I've set a reminder" could be spoken before it is true.
			hold := len(unmet()) > 0 && req.Tools != nil
			var held []string
			say := func(d string) {
				spoken.WriteString(d)
				sink.Event(Event{Type: "llm_delta", Turn: turn, Text: d})
			}
			ch := &Chunker{MinLen: 12}
			if spoken.Len() > 0 && round > 0 {
				spoken.WriteString(" ")
			}
			reply, err := s.LLM.Chat(ctx, req, func(d string) {
				if llmTTFT < 0 {
					llmTTFT = ms()
				}
				if hold {
					held = append(held, d)
					return
				}
				say(d)
				emit(ch.Push(d))
			})
			if hold && (len(reply.ToolCalls) > 0 || err != nil) {
				// Text that accompanies a tool call is safe to say.
				for _, d := range held {
					say(d)
					emit(ch.Push(d))
				}
				held = nil
			}
			emit(ch.Flush()) // speak "Let me check." while the tool runs
			if err != nil {
				llmErr = err
				break
			}
			if len(reply.ToolCalls) == 0 {
				if missing := unmet(); hold && len(missing) > 0 && round+1 < maxToolRounds {
					// Discard the unbacked reply and insist on the tool call.
					msgs = append(msgs, llm.Message{Role: "assistant", Content: reply.Content},
						llm.Message{Role: "system", Content: "You have not called " + strings.Join(missing, ", ") +
							" yet, so it has not happened. Call it now. Do not reply in text first."})
					sink.Event(Event{Type: "guard", Turn: turn, Text: "required " + strings.Join(missing, ", ") + "; retrying"})
					continue
				}
				for _, d := range held { // guard gave up or nothing required
					say(d)
					emit(ch.Push(d))
				}
				emit(ch.Flush())
				break
			}
			msgs = append(msgs, reply)
			var verbatim string
			for _, tc := range reply.ToolCalls {
				if ctx.Err() != nil {
					break
				}
				called[tc.Function.Name] = true
				sink.Event(Event{Type: "tool_call", Turn: turn, Name: tc.Function.Name, Args: tc.Function.Arguments})
				res := s.Tools.Run(ctx, call, tc.Function.Name, tc.Function.Arguments)
				toolMS += res.MS
				ok := res.OK
				sink.Event(Event{Type: "tool_result", Turn: turn, Name: tc.Function.Name, Result: res.JSON, OK: &ok, MS: map[string]int{"ms": res.MS}})
				msgs = append(msgs, llm.Message{Role: "tool", ToolName: tc.Function.Name, Content: string(res.JSON)})
				if res.Say != "" {
					verbatim = res.Say
				}
			}
			if ctx.Err() != nil {
				llmErr = ctx.Err()
				break
			}
			if verbatim != "" {
				// The tool's answer is the reply (e.g. from the cloud): speak it
				// verbatim instead of having the local model paraphrase it.
				if spoken.Len() > 0 {
					spoken.WriteString(" ")
				}
				spoken.WriteString(verbatim)
				sink.Event(Event{Type: "llm_delta", Turn: turn, Text: verbatim})
				ch := &Chunker{MinLen: 12}
				emit(ch.Push(verbatim))
				emit(ch.Flush())
				break
			}
		}
		llmEnd = ms()
	}()

	var ttsErr error
	for sent := range sentences {
		if ctx.Err() != nil || ttsErr != nil {
			continue // drain so the producer can finish
		}
		a, err := s.TTS.Synthesize(sent)
		if err != nil {
			ttsErr = fmt.Errorf("tts: %w", err)
			continue
		}
		if _, ok := timing["tts_first"]; !ok {
			timing["tts_first"] = ms()
		}
		if err := sink.Audio(turn, a); err != nil {
			ttsErr = err
			cancel()
		}
	}
	// The producer goroutine has exited (channel closed), so its variables
	// are safe to read.
	timing["total"] = ms()
	if llmTTFT >= 0 {
		timing["llm_ttft"] = llmTTFT
	}
	timing["llm_done"] = llmEnd
	if toolMS > 0 {
		timing["tools"] = toolMS
	}

	reply := strings.Join(strings.Fields(spoken.String()), " ")
	interrupted := errors.Is(llmErr, context.Canceled) || ctx.Err() != nil
	if reply != "" || !interrupted {
		content := reply
		if interrupted {
			content += " [interrupted]"
		}
		if err := s.remember(ctx, llm.Message{Role: "user", Content: user}, llm.Message{Role: "assistant", Content: content}); err != nil && ttsErr == nil {
			ttsErr = fmt.Errorf("save history: %w", err)
		}
	}
	sink.Event(Event{Type: "assistant_done", Turn: turn, Text: reply, Final: !interrupted})
	sink.Event(Event{Type: "timing", Turn: turn, MS: timing})

	switch {
	case interrupted:
		return context.Canceled
	case llmErr != nil:
		return llmErr
	default:
		return ttsErr
	}
}
