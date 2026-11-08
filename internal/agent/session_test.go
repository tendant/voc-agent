package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/voice"
)

type fakeLLM struct {
	parts []string
	delay time.Duration
	got   []llm.Message
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.Request, on func(string)) (llm.Message, error) {
	f.got = req.Messages
	var b strings.Builder
	for _, p := range f.parts {
		select {
		case <-ctx.Done():
			return llm.Message{Content: b.String()}, ctx.Err()
		case <-time.After(f.delay):
		}
		b.WriteString(p)
		on(p)
	}
	return llm.Message{Role: "assistant", Content: b.String()}, nil
}

type fakeTTS struct{}

func (fakeTTS) Synthesize(text string) (voice.Audio, error) {
	return voice.Audio{Samples: make([]float32, len(text)), SampleRate: 22050}, nil
}
func (fakeTTS) SampleRate() int { return 22050 }
func (fakeTTS) Close()          {}

type rec struct {
	mu     sync.Mutex
	events []Event
	audio  []int
}

func (r *rec) Event(e Event) error {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return nil
}
func (r *rec) Audio(_ uint32, a voice.Audio) error {
	r.mu.Lock()
	r.audio = append(r.audio, len(a.Samples))
	r.mu.Unlock()
	return nil
}

func TestTurnSpeaksSentencesAndRemembers(t *testing.T) {
	f := &fakeLLM{parts: []string{"Drink a glass ", "of water now. ", "Then keep a bottle nearby."}}
	s := &Session{LLM: f, TTS: fakeTTS{}}
	r := &rec{}
	if err := s.Turn(context.Background(), 1, "hydration tips?", time.Now(), r); err != nil {
		t.Fatal(err)
	}
	if want := []int{len("Drink a glass of water now."), len("Then keep a bottle nearby.")}; len(r.audio) != 2 || r.audio[0] != want[0] || r.audio[1] != want[1] {
		t.Fatalf("audio chunks %v, want %v", r.audio, want)
	}
	last := r.events[len(r.events)-1]
	if last.Type != "timing" || last.MS["llm_ttft"] > last.MS["tts_first"] {
		t.Fatalf("bad timing event %+v", last)
	}
	if f.got[0].Role != "system" || !strings.Contains(f.got[0].Content, "current local time") {
		t.Fatalf("system prompt missing: %+v", f.got[0])
	}

	// Second turn sees the first in history.
	f.parts = []string{"Sure."}
	s.Turn(context.Background(), 2, "thanks", time.Now(), &rec{})
	if len(f.got) != 4 || f.got[2].Role != "assistant" {
		t.Fatalf("history not passed: %+v", f.got)
	}
}

func TestTurnCancel(t *testing.T) {
	f := &fakeLLM{parts: []string{"One sentence here. ", "Two ", "three ", "four."}, delay: 30 * time.Millisecond}
	s := &Session{LLM: f, TTS: fakeTTS{}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := s.Turn(ctx, 1, "talk", time.Now(), &rec{})
	if err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	h := s.History()
	if len(h) != 2 || !strings.HasSuffix(h[1].Content, "[interrupted]") {
		t.Fatalf("history = %+v", h)
	}
}

func TestHistoryCap(t *testing.T) {
	s := &Session{LLM: &fakeLLM{parts: []string{"ok"}}, TTS: fakeTTS{}, MaxHistory: 4}
	for i := 0; i < 5; i++ {
		s.Turn(context.Background(), uint32(i), "hi", time.Now(), &rec{})
	}
	if n := len(s.History()); n != 4 {
		t.Fatalf("history len %d", n)
	}
}
