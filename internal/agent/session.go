// Package agent runs conversation turns: user text → LLM → sentences → TTS.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/voice"
)

// Event is a JSON message for the client. Turn identifies the turn so the
// client can drop output from turns it has abandoned.
type Event struct {
	Type  string         `json:"type"`
	Turn  uint32         `json:"turn,omitempty"`
	Text  string         `json:"text,omitempty"`
	Lang  string         `json:"lang,omitempty"`
	Final bool           `json:"final,omitempty"`
	MS    map[string]int `json:"ms,omitempty"`
	Error string         `json:"error,omitempty"`
}

// Sink receives a turn's output. Implementations must be safe for concurrent
// use: events and audio are sent from different goroutines.
type Sink interface {
	Event(Event) error
	Audio(turn uint32, a voice.Audio) error
}

const defaultSystem = `You are voc, a private voice assistant running entirely on the user's own computer.
Your replies are spoken aloud by a text-to-speech voice, so:
- Answer in one to three short sentences unless the user asks for more detail.
- Use plain conversational sentences. No markdown, lists, headings, emojis, or code.
- Write numbers, dates, and units the way you would say them.
- Always reply in English.
You cannot take actions yet (no reminders, notes, calendar, or internet). If asked, say that isn't available yet.
Never claim to have done something you have not done.`

// Session holds one user's conversation. Turns are serialized.
type Session struct {
	LLM        llm.Chat
	TTS        voice.TTS
	System     string // defaults to defaultSystem
	MaxHistory int    // messages kept, excluding the system prompt; default 20
	Now        func() time.Time

	turnMu  sync.Mutex // one turn at a time
	mu      sync.Mutex // guards history
	history []llm.Message
}

func (s *Session) systemPrompt() string {
	p := s.System
	if p == "" {
		p = defaultSystem
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

func (s *Session) remember(msgs ...llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msgs...)
	max := s.MaxHistory
	if max == 0 {
		max = 20
	}
	if n := len(s.history) - max; n > 0 {
		s.history = append([]llm.Message(nil), s.history[n:]...)
	}
}

// Turn answers one user utterance, streaming text deltas and speech to sink.
// start is when the user finished speaking; timings are relative to it.
// Cancelling ctx stops generation and speech; the partial reply is kept in
// history so the conversation stays coherent.
func (s *Session) Turn(ctx context.Context, turn uint32, user string, start time.Time, sink Sink) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	ms := func() int { return int(time.Since(start).Milliseconds()) }
	timing := map[string]int{}

	msgs := append([]llm.Message{{Role: "system", Content: s.systemPrompt()}}, s.History()...)
	msgs = append(msgs, llm.Message{Role: "user", Content: user})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sentences := make(chan string, 32)
	// Written only by the producer goroutine; read after sentences closes.
	var (
		reply           string
		llmErr          error
		llmTTFT, llmEnd = -1, -1
	)
	go func() {
		defer close(sentences)
		ch := &Chunker{MinLen: 12}
		emit := func(ss []string) {
			for _, x := range ss {
				select {
				case sentences <- x:
				case <-ctx.Done():
				}
			}
		}
		reply, llmErr = s.LLM.Chat(ctx, msgs, func(d string) {
			if llmTTFT < 0 {
				llmTTFT = ms()
			}
			sink.Event(Event{Type: "llm_delta", Turn: turn, Text: d})
			emit(ch.Push(d))
		})
		llmEnd = ms()
		emit(ch.Flush())
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

	interrupted := errors.Is(llmErr, context.Canceled) || ctx.Err() != nil
	if reply != "" || !interrupted {
		content := reply
		if interrupted {
			content += " [interrupted]"
		}
		s.remember(llm.Message{Role: "user", Content: user}, llm.Message{Role: "assistant", Content: content})
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
