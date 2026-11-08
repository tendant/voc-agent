package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/tendant/voc-agent/internal/voice"
)

const modelDir = "../../models"

// TestRoundTrip streams a real recording over the WebSocket in 20 ms frames,
// as the browser does, and expects the echo frame plus a correct transcript.
func TestRoundTrip(t *testing.T) {
	wavPath := modelDir + "/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/test_wavs/en.wav"
	if _, err := os.Stat(wavPath); err != nil {
		t.Skip("models not downloaded; run make models")
	}
	m := voice.ModelPaths{Dir: modelDir}
	asr, err := voice.NewSherpaASR(m)
	if err != nil {
		t.Fatal(err)
	}
	defer asr.Close()
	vad, err := voice.NewSherpaVAD(m)
	if err != nil {
		t.Fatal(err)
	}
	defer vad.Close()

	srv := &Server{ASR: asr, VAD: vad, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(4 << 20) // echo frame is the whole utterance

	pcm := voice.FloatToPCM16(sherpa.ReadWave(wavPath).Samples)
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"ptt_start"}`))
	for i := 0; i < len(pcm); i += 640 {
		if err := c.Write(ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))]); err != nil {
			t.Fatal(err)
		}
	}
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"ptt_end"}`))

	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		t.Fatalf("want echo frame, got %v %v", typ, err)
	}
	if rate := binary.LittleEndian.Uint32(data[4:]); rate != voice.SampleRate {
		t.Errorf("echo rate = %d", rate)
	}
	if got := len(data) - 8; got != len(pcm) {
		t.Errorf("echo has %d bytes, sent %d", got, len(pcm))
	}

	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m2 msg
	json.Unmarshal(data, &m2)
	if m2.Type != "transcript" || !strings.Contains(strings.ToLower(m2.Text), "chieftain") {
		t.Fatalf("unexpected transcript: %+v", m2)
	}
	t.Logf("transcript: %q", m2.Text)
}
