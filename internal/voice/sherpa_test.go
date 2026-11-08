package voice

import (
	"os"
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
