package agent

import (
	"context"
	"fmt"
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
	firstSystem := f.got[0].Content
	if f.got[0].Role != "system" || strings.Contains(firstSystem, "It is ") {
		t.Fatalf("system prompt should be static (no clock): %+v", f.got[0])
	}
	// The situation (with the time) travels in the user message, before the words.
	if u := f.got[len(f.got)-1]; u.Role != "user" || !strings.Contains(u.Content, "Current situation") || !strings.Contains(u.Content, "It is ") ||
		UserText(u.Content) != "hydration tips?" {
		t.Fatalf("situation missing: %+v", u)
	}

	// Second turn sees the first in history, with the same system prompt.
	f.parts = []string{"Sure."}
	s.Turn(context.Background(), 2, "thanks", time.Now(), &rec{})
	if len(f.got) != 4 || f.got[2].Role != "assistant" || f.got[0].Content != firstSystem {
		t.Fatalf("history not passed or prompt changed: %+v", f.got)
	}
	// History holds the user message exactly as sent, so the next prompt
	// starts with the same bytes the model server has cached.
	if f.got[1].Content != s.History()[0].Content || !strings.Contains(f.got[1].Content, "Current situation") {
		t.Fatal("history must keep the message as sent")
	}
}

func TestSituationContents(t *testing.T) {
	f := &fakeLLM{parts: []string{"ok"}}
	s := &Session{LLM: f, TTS: fakeTTS{}, Situation: func(ctx context.Context) []string {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("situation must be gathered under a deadline")
		}
		return []string{"Open in voc's browser: \"Lo-fi beats\" (youtube.com); a video is playing.", "", "Next reminder: Stretch, today at 8 PM."}
	}}
	s.Turn(WithInputMode(context.Background(), ModeHandsFree), 1, "pause it", time.Now(), &rec{})
	sit := f.got[len(f.got)-1].Content
	for _, want := range []string{"hands-free voice", "may contain recognition errors", "Lo-fi beats", "Next reminder: Stretch"} {
		if !strings.Contains(sit, want) {
			t.Errorf("situation lacks %q:\n%s", want, sit)
		}
	}
	if strings.Contains(sit, "\n- \n") {
		t.Error("blank line kept")
	}
	s.Turn(WithInputMode(context.Background(), ModeTyped), 2, "and now?", time.Now(), &rec{})
	if !strings.Contains(f.got[len(f.got)-1].Content, "The user typed this message.") {
		t.Error("typed mode not said")
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
	s := &Session{LLM: &fakeLLM{parts: []string{"ok"}}, TTS: fakeTTS{}, MaxHistory: 8}
	cuts, prev := 0, 0
	for i := 0; i < 12; i++ {
		s.Turn(context.Background(), uint32(i), fmt.Sprintf("hi %d", i), time.Now(), &rec{})
		h := s.History()
		if len(h) > 8 || h[0].Role != "user" {
			t.Fatalf("turn %d: len %d, starts with %s", i, len(h), h[0].Role)
		}
		if len(h) < prev {
			cuts++
		}
		prev = len(h)
	}
	// Trimmed in a few big cuts (each costs one full prompt re-read), not every turn.
	if cuts == 0 || cuts > 4 {
		t.Fatalf("%d cuts in 12 turns", cuts)
	}
	if UserText(s.History()[len(s.History())-2].Content) != "hi 11" {
		t.Fatal("newest turn must survive the cut")
	}
}
