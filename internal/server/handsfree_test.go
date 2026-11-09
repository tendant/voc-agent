package server

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/voice"
)

func TestMediaGate(t *testing.T) {
	for in, want := range map[string]bool{"Pause.": true, "pause the video": true, "Next.": true, "turn it down": true,
		"louder": true, "Scroll down.": true, "Go back.": true, "and then he said we should go": false, "subscribe to my channel": false} {
		if got := mediaCommandRe.MatchString(normalize(in)); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	for in, want := range map[string]string{"Hey voc, what's the weather": "what's the weather", "ok vox remind me at 5": "remind me at 5"} {
		n := normalize(in)
		if !wakeRe.MatchString(n) || wakeRe.ReplaceAllString(n, "") != want {
			t.Errorf("%q → %q", in, wakeRe.ReplaceAllString(n, ""))
		}
	}
}

func TestIsNoise(t *testing.T) {
	for in, want := range map[string]bool{"": true, ".": true, "Um.": true, "uh, hmm": true, "嗯": true, "Oh.": true,
		"Hi there": false, "Stop": false, "What's the time?": false, "um what is the time": false} {
		if got := isNoise(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	for in, want := range map[string]bool{"Stop listening.": true, "please stop listening": true, "Pause listening": true,
		"Dtop listening.": true, "Dotop listening.": true, "Top listening.": true, "Stopp listening": true, "please dtop listening": true,
		"stop listing my reminders": false, "don't stop listening": false, "keep listening": false, "listening": false,
		"I am listening": false} {
		if got := isStopListening(in); got != want {
			t.Errorf("stop %q: %v", in, got)
		}
	}
}

// TestHandsFree streams a continuous mic signal (silence, speech, silence,
// "stop listening", silence) with no push-to-talk: the first utterance
// becomes a turn and the second turns hands-free off.
func TestHandsFree(t *testing.T) {
	wav := modelDir + "/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/test_wavs/en.wav"
	if _, err := os.Stat(wav); err != nil {
		t.Skip("models not downloaded")
	}
	m := voice.ModelPaths{Dir: modelDir}
	asr, _ := voice.NewSherpaASR(m)
	vad, _ := voice.NewSherpaVAD(m)
	tts, _ := voice.NewSherpaTTS(m)
	srv := &Server{ASR: asr, VAD: vad, TTS: tts, Agent: &agent.Session{LLM: echoLLM{}, TTS: toneTTS{}},
		NewSegmenter: func() (voice.Segmenter, error) { return voice.NewSherpaSegmenter(m) },
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := dial(t, srv)
	t.Cleanup(func() { asr.Close(); vad.Close(); tts.Close() })

	// Piper's voice is randomized, so the same text can come out as audio the
	// recognizer mishears. This test is about the server, not ASR: use a take
	// that is recognized as the command.
	var stopPCM []float32
	for try := 0; try < 5 && stopPCM == nil; try++ {
		a, _ := tts.Synthesize("Stop listening.")
		pcm := voice.Resample(a.Samples, a.SampleRate, voice.SampleRate)
		if tr, _ := asr.Transcribe(vad.Trim(pcm)); isStopListening(tr.Text) {
			stopPCM = pcm
		}
	}
	if stopPCM == nil {
		t.Fatal("no recognizable take of \"Stop listening.\" in 5 tries")
	}
	silence := make([]float32, voice.SampleRate)
	var stream []float32
	for _, part := range [][]float32{silence, sherpa.ReadWave(wav).Samples, silence, stopPCM, silence} {
		stream = append(stream, part...)
	}
	pcm := voice.FloatToPCM16(stream)
	c.send(`{"type":"listen_start"}`)
	for i := 0; i < len(pcm); i += 640 {
		c.c.Write(c.ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))])
	}

	evs, _ := c.until("assistant_done", 1)
	tr := find(evs, "transcript")
	if tr == nil || !strings.Contains(strings.ToLower(tr.Text), "chieftain") || find(evs, "speech") == nil {
		t.Fatalf("hands-free turn: %+v", evs)
	}
	evs, _ = c.until("listen_stopped", 0)
	if find(evs, "turn_start") != nil {
		t.Fatalf("'stop listening' became a turn: %+v", evs)
	}
}
