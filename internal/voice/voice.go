// Package voice defines the speech engines (ASR, TTS, VAD) behind small
// interfaces so the sherpa-onnx implementation can be swapped for a sidecar.
package voice

// SampleRate is the PCM rate the browser sends and ASR/VAD expect.
const SampleRate = 16000

// Transcript is the result of recognizing one utterance.
type Transcript struct {
	Text    string
	Lang    string
	Emotion string
	Event   string
}

// ASR turns a complete utterance (mono float32 at SampleRate) into text.
type ASR interface {
	Transcribe(samples []float32) (Transcript, error)
	Close()
}

// Audio is synthesized speech: mono float32 samples at SampleRate.
type Audio struct {
	Samples    []float32
	SampleRate int
}

// TTS turns text into speech.
type TTS interface {
	Synthesize(text string) (Audio, error)
	SampleRate() int
	Close()
}

// VAD extracts the voiced part of a buffered utterance.
type VAD interface {
	// Trim returns the samples covered by detected speech segments, or nil
	// if no speech was found.
	Trim(samples []float32) []float32
	Close()
}

// PCM16ToFloat converts little-endian signed 16-bit PCM to float32 in [-1,1].
func PCM16ToFloat(b []byte) []float32 {
	out := make([]float32, len(b)/2)
	for i := range out {
		v := int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
		out[i] = float32(v) / 32768
	}
	return out
}

// FloatToPCM16 converts float32 samples to little-endian 16-bit PCM, clipping.
func FloatToPCM16(s []float32) []byte {
	out := make([]byte, 2*len(s))
	for i, f := range s {
		if f > 1 {
			f = 1
		} else if f < -1 {
			f = -1
		}
		v := int16(f * 32767)
		out[2*i] = byte(v)
		out[2*i+1] = byte(uint16(v) >> 8)
	}
	return out
}
