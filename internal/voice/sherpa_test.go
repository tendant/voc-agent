package voice

import (
	"os"
	"strings"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const testModels = "../../models"

func testWav(t *testing.T) []float32 {
	p := testModels + "/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/test_wavs/en.wav"
	if _, err := os.Stat(p); err != nil {
		t.Skip("models not downloaded; run make models")
	}
	return sherpa.ReadWave(p).Samples
}

func TestVADSameGoroutine(t *testing.T) {
	s := testWav(t)
	v, err := NewSherpaVAD(ModelPaths{Dir: testModels})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if got := v.Trim(s); len(got) == 0 {
		t.Fatal("no speech")
	}
}

func TestVADOtherGoroutine(t *testing.T) {
	s := testWav(t)
	v, err := NewSherpaVAD(ModelPaths{Dir: testModels})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	done := make(chan int)
	go func() { done <- len(v.Trim(s)) }()
	if <-done == 0 {
		t.Fatal("no speech")
	}
}

func TestVADAfterASR(t *testing.T) {
	s := testWav(t)
	m := ModelPaths{Dir: testModels}
	a, err := NewSherpaASR(m)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	v, err := NewSherpaVAD(m)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	speech := v.Trim(s)
	if len(speech) == 0 {
		t.Fatal("no speech")
	}
	tr, err := a.Transcribe(speech)
	if err != nil || tr.Text == "" {
		t.Fatalf("transcribe: %+v %v", tr, err)
	}
}

func TestUseAfterClose(t *testing.T) {
	s := testWav(t)
	m := ModelPaths{Dir: testModels}
	a, _ := NewSherpaASR(m)
	v, _ := NewSherpaVAD(m)
	a.Close()
	v.Close()
	a.Close() // idempotent
	if _, err := a.Transcribe(s); err != ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if v.Trim(s) != nil {
		t.Fatal("trim after close should return nil")
	}
}

// TestSegmenterSplitsContinuousStream: two utterances separated by silence,
// fed in 20 ms frames like a live mic, come out as two segments.
func TestSegmenterSplitsContinuousStream(t *testing.T) {
	speech := testWav(t)
	seg, err := NewSherpaSegmenter(ModelPaths{Dir: testModels})
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	silence := make([]float32, SampleRate) // 1 s
	stream := append(append(append(append([]float32{}, silence...), speech...), silence...), speech...)
	stream = append(stream, silence...)
	var segs [][]float32
	sawSpeech := false
	for i := 0; i < len(stream); i += 320 {
		segs = append(segs, seg.Accept(stream[i:min(i+320, len(stream))])...)
		sawSpeech = sawSpeech || seg.Speaking()
	}
	if len(segs) < 2 || !sawSpeech {
		t.Fatalf("got %d segments (speaking seen: %v)", len(segs), sawSpeech)
	}
	// Each segment should be most of the utterance and transcribe correctly.
	a, _ := NewSherpaASR(ModelPaths{Dir: testModels})
	defer a.Close()
	for _, s := range segs {
		tr, _ := a.Transcribe(s)
		t.Logf("%.2fs %q", float64(len(s))/SampleRate, tr.Text)
	}
	tr, _ := a.Transcribe(segs[len(segs)-1])
	if !strings.Contains(strings.ToLower(tr.Text), "pieces of code") {
		t.Fatalf("last segment: %q", tr.Text)
	}
}

// TestSegmenterResetAndPreRoll: after a Reset mid-stream the next utterance
// still comes out whole, including its first word.
func TestSegmenterResetAndPreRoll(t *testing.T) {
	m := ModelPaths{Dir: testModels}
	speech := testWav(t)
	seg, err := NewSherpaSegmenter(m)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	silence := make([]float32, SampleRate)
	feed := func(x []float32) (out [][]float32) {
		for i := 0; i < len(x); i += 320 {
			out = append(out, seg.Accept(x[i:min(i+320, len(x))])...)
		}
		return
	}
	feed(append(append([]float32{}, silence...), speech[:len(speech)/2]...)) // cut off mid-utterance
	seg.Reset()
	segs := feed(append(append(append([]float32{}, silence...), speech...), silence...))
	if len(segs) != 1 {
		t.Fatalf("segments: %d", len(segs))
	}
	a, _ := NewSherpaASR(m)
	defer a.Close()
	tr, _ := a.Transcribe(segs[0])
	if !strings.HasPrefix(strings.ToLower(tr.Text), "the tribal") {
		t.Fatalf("first word lost: %q", tr.Text)
	}
}
