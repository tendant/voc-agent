// voicespike checks the sherpa-onnx engines end to end without a browser:
// it synthesizes a sentence with Piper, resamples it to 16 kHz, runs it
// through VAD and SenseVoice, and prints timings.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/tendant/voc-agent/internal/voice"
)

func main() {
	models := flag.String("models", "models", "model directory")
	text := flag.String("text", "Find my deployment notes and remind me to review them tomorrow.", "text to round-trip")
	wav := flag.String("wav", "", "optional 16 kHz wav to transcribe instead of TTS output")
	out := flag.String("out", "spike.wav", "where to save the synthesized audio")
	flag.Parse()
	m := voice.ModelPaths{Dir: *models}

	t0 := time.Now()
	asr, err := voice.NewSherpaASR(m)
	must(err)
	defer asr.Close()
	tts, err := voice.NewSherpaTTS(m)
	must(err)
	defer tts.Close()
	vad, err := voice.NewSherpaVAD(m)
	must(err)
	defer vad.Close()
	fmt.Printf("load models      %v\n", time.Since(t0).Round(time.Millisecond))

	var samples []float32
	if *wav != "" {
		w := sherpa.ReadWave(*wav)
		if w == nil {
			log.Fatalf("cannot read %s", *wav)
		}
		samples = w.Samples
	} else {
		t := time.Now()
		a, err := tts.Synthesize(*text)
		must(err)
		dur := float64(len(a.Samples)) / float64(a.SampleRate)
		fmt.Printf("tts              %v for %.2fs audio @ %d Hz (RTF %.3f)\n",
			time.Since(t).Round(time.Millisecond), dur, a.SampleRate, time.Since(t).Seconds()/dur)
		f, err := os.Create(*out)
		must(err)
		must(voice.WriteWAV(f, a))
		f.Close()
		samples = voice.Resample(a.Samples, a.SampleRate, voice.SampleRate)
	}

	t := time.Now()
	trimmed := vad.Trim(samples)
	fmt.Printf("vad              %v (%.2fs → %.2fs)\n", time.Since(t).Round(time.Millisecond),
		float64(len(samples))/voice.SampleRate, float64(len(trimmed))/voice.SampleRate)
	if len(trimmed) == 0 {
		trimmed = samples
	}

	t = time.Now()
	tr, err := asr.Transcribe(trimmed)
	must(err)
	fmt.Printf("asr              %v\n", time.Since(t).Round(time.Millisecond))
	fmt.Printf("transcript       %q lang=%s emotion=%s event=%s\n", tr.Text, tr.Lang, tr.Emotion, tr.Event)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
