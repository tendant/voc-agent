package server

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/voice"
)

// TestLiveVoiceLoop runs the full offline stack: a question is synthesized
// with Piper, streamed in as mic audio, recognized by SenseVoice, answered by
// local Qwen3 and spoken back. Run with VOC_LIVE=1 (needs Ollama + models).
func TestLiveVoiceLoop(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	m := voice.ModelPaths{Dir: modelDir}
	asr, err := voice.NewSherpaASR(m)
	if err != nil {
		t.Fatal(err)
	}
	vad, _ := voice.NewSherpaVAD(m)
	tts, _ := voice.NewSherpaTTS(m)
	ol := &llm.Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", Temperature: 0.3, KeepAlive: "30m"}
	srv := &Server{ASR: asr, VAD: vad, Agent: &agent.Session{LLM: ol, TTS: tts},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := dial(t, srv)
	t.Cleanup(func() { asr.Close(); vad.Close(); tts.Close() })
	if err := ol.Warm(c.ctx); err != nil {
		t.Fatal(err)
	}

	for i, q := range []string{"What is the capital of France?", "And what river runs through it?"} {
		a, _ := tts.Synthesize(q)
		pcm := voice.FloatToPCM16(voice.Resample(a.Samples, a.SampleRate, voice.SampleRate))
		c.send(`{"type":"ptt_start"}`)
		for j := 0; j < len(pcm); j += 640 {
			c.c.Write(c.ctx, websocket.MessageBinary, pcm[j:min(j+640, len(pcm))])
		}
		c.send(`{"type":"ptt_end"}`)
		turn := uint32(i + 1)
		evs, audio := c.until("timing", turn)
		evs2, audio2 := c.until("timing", turn)
		evs, audio = append(evs, evs2...), append(audio, audio2...)
		done := find(evs, "assistant_done")
		t.Logf("Q%d heard %q", turn, find(evs, "transcript").Text)
		t.Logf("   reply %q (%d audio frames)", done.Text, len(audio))
		t.Logf("   asr=%v agent=%v", evs[findIdx(evs, "timing")].MS, evs[len(evs)-1].MS)
		want := []string{"paris", "seine"}[i]
		if !strings.Contains(strings.ToLower(done.Text), want) || len(audio) == 0 {
			t.Errorf("Q%d: want %q in spoken reply", turn, want)
		}
	}
}

func findIdx(evs []agent.Event, typ string) int {
	for i := range evs {
		if evs[i].Type == typ {
			return i
		}
	}
	return -1
}
