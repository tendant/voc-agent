package voice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// ModelPaths locates the files downloaded by scripts/fetch-models.sh.
type ModelPaths struct {
	Dir string // usually ./models
}

func (m ModelPaths) senseVoice() string {
	return filepath.Join(m.Dir, "sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17")
}
func (m ModelPaths) piper() string  { return filepath.Join(m.Dir, "vits-piper-en_US-lessac-medium") }
func (m ModelPaths) silero() string { return filepath.Join(m.Dir, "silero_vad.onnx") }

func mustExist(paths ...string) error {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("model file missing (run scripts/fetch-models.sh): %w", err)
		}
	}
	return nil
}

func threads() int { return min(4, runtime.NumCPU()) }

// --- ASR ---

type sherpaASR struct {
	mu  sync.Mutex // recognizer is not safe for concurrent Decode
	rec *sherpa.OfflineRecognizer
}

// NewSherpaASR loads SenseVoiceSmall (int8).
func NewSherpaASR(m ModelPaths) (ASR, error) {
	dir := m.senseVoice()
	model, tokens := filepath.Join(dir, "model.int8.onnx"), filepath.Join(dir, "tokens.txt")
	if err := mustExist(model, tokens); err != nil {
		return nil, err
	}
	cfg := sherpa.OfflineRecognizerConfig{}
	cfg.FeatConfig = sherpa.FeatureConfig{SampleRate: SampleRate, FeatureDim: 80}
	cfg.ModelConfig.SenseVoice = sherpa.OfflineSenseVoiceModelConfig{
		Model: model, Language: "auto", UseInverseTextNormalization: 1,
	}
	cfg.ModelConfig.Tokens = tokens
	cfg.ModelConfig.NumThreads = threads()
	cfg.ModelConfig.Provider = "cpu"
	cfg.DecodingMethod = "greedy_search"
	rec := sherpa.NewOfflineRecognizer(&cfg)
	if rec == nil {
		return nil, errors.New("sherpa: failed to create SenseVoice recognizer")
	}
	return &sherpaASR{rec: rec}, nil
}

// SenseVoice tags look like <|en|>; strip them for a clean label.
var tagRe = regexp.MustCompile(`<\|([^|]*)\|>`)

func clean(s string) string { return strings.TrimSpace(tagRe.ReplaceAllString(s, "$1")) }

// ErrClosed is returned when an engine is used after Close. Close can race
// with in-flight requests on shutdown (hijacked WebSocket connections outlive
// http.Server.Shutdown), so every engine guards its C handle with its mutex.
var ErrClosed = errors.New("voice: engine closed")

func (a *sherpaASR) Transcribe(samples []float32) (Transcript, error) {
	if len(samples) == 0 {
		return Transcript{}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rec == nil {
		return Transcript{}, ErrClosed
	}
	st := sherpa.NewOfflineStream(a.rec)
	defer sherpa.DeleteOfflineStream(st)
	st.AcceptWaveform(SampleRate, samples)
	a.rec.Decode(st)
	r := st.GetResult()
	return Transcript{
		Text: strings.TrimSpace(r.Text), Lang: clean(r.Lang),
		Emotion: clean(r.Emotion), Event: clean(r.Event),
	}, nil
}

func (a *sherpaASR) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rec != nil {
		sherpa.DeleteOfflineRecognizer(a.rec)
		a.rec = nil
	}
}

// --- TTS ---

type sherpaTTS struct {
	mu   sync.Mutex
	tts  *sherpa.OfflineTts
	rate int
}

// NewSherpaTTS loads the Piper (VITS) voice.
func NewSherpaTTS(m ModelPaths) (TTS, error) {
	dir := m.piper()
	model := filepath.Join(dir, "en_US-lessac-medium.onnx")
	tokens := filepath.Join(dir, "tokens.txt")
	data := filepath.Join(dir, "espeak-ng-data")
	if err := mustExist(model, tokens, data); err != nil {
		return nil, err
	}
	cfg := sherpa.OfflineTtsConfig{MaxNumSentences: 1}
	cfg.Model.Vits = sherpa.OfflineTtsVitsModelConfig{
		Model: model, Tokens: tokens, DataDir: data,
		NoiseScale: 0.667, NoiseScaleW: 0.8, LengthScale: 1.0,
	}
	cfg.Model.NumThreads = threads()
	cfg.Model.Provider = "cpu"
	t := sherpa.NewOfflineTts(&cfg)
	if t == nil {
		return nil, errors.New("sherpa: failed to create Piper TTS")
	}
	return &sherpaTTS{tts: t, rate: t.SampleRate()}, nil
}

func (t *sherpaTTS) Synthesize(text string) (Audio, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tts == nil {
		return Audio{}, ErrClosed
	}
	g := t.tts.Generate(text, 0, 1.0)
	if g == nil {
		return Audio{}, errors.New("sherpa: TTS generation failed")
	}
	return Audio{Samples: g.Samples, SampleRate: g.SampleRate}, nil
}

func (t *sherpaTTS) SampleRate() int { return t.rate }

func (t *sherpaTTS) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tts != nil {
		sherpa.DeleteOfflineTts(t.tts)
		t.tts = nil
	}
}

// --- VAD ---

type sherpaVAD struct {
	mu  sync.Mutex
	vad *sherpa.VoiceActivityDetector
}

const vadWindow = 512 // Silero window at 16 kHz

// NewSherpaVAD loads Silero VAD.
func NewSherpaVAD(m ModelPaths) (VAD, error) {
	if err := mustExist(m.silero()); err != nil {
		return nil, err
	}
	cfg := sherpa.VadModelConfig{SampleRate: SampleRate, NumThreads: 1, Provider: "cpu"}
	cfg.SileroVad = sherpa.SileroVadModelConfig{
		Model: m.silero(), Threshold: 0.5, MinSilenceDuration: 0.5,
		MinSpeechDuration: 0.25, WindowSize: vadWindow, MaxSpeechDuration: 30,
	}
	v := sherpa.NewVoiceActivityDetector(&cfg, 60)
	if v == nil {
		return nil, errors.New("sherpa: failed to create Silero VAD")
	}
	return &sherpaVAD{vad: v}, nil
}

func (v *sherpaVAD) Trim(samples []float32) []float32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vad == nil {
		return nil
	}
	v.vad.Reset()
	for i := 0; i < len(samples); i += vadWindow {
		v.vad.AcceptWaveform(samples[i:min(i+vadWindow, len(samples))])
	}
	v.vad.Flush()
	var out []float32
	for !v.vad.IsEmpty() {
		out = append(out, v.vad.Front().Samples...)
		v.vad.Pop()
	}
	return out
}

func (v *sherpaVAD) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vad != nil {
		sherpa.DeleteVoiceActivityDetector(v.vad)
		v.vad = nil
	}
}
