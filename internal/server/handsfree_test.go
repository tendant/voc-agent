package server

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

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
}

func TestWakeMatch(t *testing.T) {
	for in, want := range map[string]string{
		"Hey Victor.": "", "Hey, Victor, what time is it?": "what time is it", "Okay Victor, play some jazz.": "play some jazz",
		"Victor, scroll down": "scroll down", "Heyvictor what's up": "what's up", "Hey Viktor": "", "hey victer pause": "pause",
	} {
		rest, ok := wakeMatch("Victor", in)
		if !ok || rest != want {
			t.Errorf("%q: %q %v", in, rest, ok)
		}
	}
	for _, in := range []string{"What time is it?", "I talked to Victoria yesterday", "hey vic", "Hey, O.", "the victory was great"} {
		if rest, ok := wakeMatch("Victor", in); ok {
			t.Errorf("%q should not wake (rest %q)", in, rest)
		}
	}
	for in, want := range map[string]string{"gold hey victor what time is it": "what time is it", "pieces of gold. Hey, Victor.": ""} {
		if rest, ok := wakeFind("Victor", in); !ok || rest != want {
			t.Errorf("find %q: %q %v", in, rest, ok)
		}
	}
	if _, ok := wakeFind("Victor", "the tribal chieftain called for the boy"); ok {
		t.Error("false find")
	}
	for in, want := range map[string]bool{"Go to sleep.": true, "that's all, thanks": true, "that's all thanks": true, "Good night": true, "sleep is important": false} {
		if got := goToSleepRe.MatchString(normalize(strings.ReplaceAll(in, ",", ""))); got != want {
			t.Errorf("sleep %q: %v", in, got)
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

// takeOf synthesizes text until a take is recognized as wanted, since Piper's
// voice is randomized; the tests are about the server, not ASR.
// The take is checked through the hands-free segmenter, the same path the
// stream will take.
func takeOf(t *testing.T, tts voice.TTS, asr voice.ASR, vad voice.VAD, text string, ok func(string) bool) []float32 {
	for try := 0; try < 8; try++ {
		a, _ := tts.Synthesize(text)
		pcm := voice.Resample(a.Samples, a.SampleRate, voice.SampleRate)
		seg, err := voice.NewSherpaSegmenter(voice.ModelPaths{Dir: modelDir})
		if err != nil {
			t.Fatal(err)
		}
		stream := append(append(make([]float32, voice.SampleRate/2), pcm...), make([]float32, voice.SampleRate)...)
		var segs [][]float32
		for i := 0; i < len(stream); i += 320 {
			segs = append(segs, seg.Accept(stream[i:min(i+320, len(stream))])...)
		}
		seg.Close()
		if len(segs) == 1 {
			if tr, _ := asr.Transcribe(segs[0]); ok(tr.Text) {
				return pcm
			}
		}
	}
	t.Fatalf("no recognizable take of %q", text)
	return nil
}

// TestSleepAndWake streams: a request (awake) → 2.5 s silence (sleeps after
// 2 s) → a request without the wake word (ignored) → "Hey Victor, …" (wakes
// and answers) → "Go to sleep" (sleeps).
func TestSleepAndWake(t *testing.T) {
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
		SleepAfter:   2 * time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := dial(t, srv)
	t.Cleanup(func() { asr.Close(); vad.Close(); tts.Close() })

	wakeTake := takeOf(t, tts, asr, vad, "Hey Victor, what time is it?", func(s string) bool {
		r, ok := wakeMatch("Victor", s)
		return ok && strings.Contains(r, "time")
	})
	sleepTake := takeOf(t, tts, asr, vad, "Go to sleep.", func(s string) bool { return goToSleepRe.MatchString(normalize(s)) })
	speech := sherpa.ReadWave(wav).Samples
	sil := func(sec float64) []float32 { return make([]float32, int(sec*voice.SampleRate)) }
	send := func(parts ...[]float32) {
		var x []float32
		for _, p := range parts {
			x = append(x, p...)
		}
		pcm := voice.FloatToPCM16(x)
		for i := 0; i < len(pcm); i += 640 {
			c.c.Write(c.ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))])
		}
	}

	c.send(`{"type":"listen_start"}`)
	if evs, _ := c.until("listen_state", 0); find(evs, "listen_state").Text != "awake" {
		t.Fatal("should start awake")
	}
	send(sil(0.5), speech, sil(0.8))
	c.until("assistant_done", 1) // answered while awake
	send(sil(2.5))
	evs, _ := c.until("listen_state", 0)
	if e := find(evs, "listen_state"); e.Text != "asleep" || e.Name != "Victor" {
		t.Fatalf("should sleep: %+v", e)
	}
	send(speech, sil(1.5)) // no wake word: ignored
	evs, _ = c.until("ignored", 0)
	if find(evs, "turn_start") != nil {
		t.Fatal("a request was answered while asleep")
	}
	send(wakeTake, sil(1.5))
	evs, _ = c.until("assistant_done", 2)
	if e := find(evs, "listen_state"); e == nil || e.Text != "awake" {
		t.Fatalf("did not wake: %+v", evs)
	}
	if d := find(evs, "assistant_done"); !strings.Contains(strings.ToLower(d.Text), "what time is it") || strings.Contains(strings.ToLower(d.Text), "victor") {
		t.Fatalf("request after the wake word: %+v", d)
	}
	send(sleepTake, sil(1.5))
	evs, _ = c.until("listen_state", 0)
	if find(evs, "listen_state").Text != "asleep" || find(evs, "turn_start") != nil {
		t.Fatalf("go to sleep: %+v", evs)
	}
}
