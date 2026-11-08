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

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/voice"
)

const modelDir = "../../models"

// echoLLM replies "You said: <last user message>." with a delay per word.
type echoLLM struct{ delay time.Duration }

func (e echoLLM) Chat(ctx context.Context, msgs []llm.Message, on func(string)) (string, error) {
	words := strings.Fields("You said: " + msgs[len(msgs)-1].Content)
	var b strings.Builder
	for _, w := range words {
		select {
		case <-ctx.Done():
			return b.String(), ctx.Err()
		case <-time.After(e.delay):
		}
		b.WriteString(w + " ")
		on(w + " ")
	}
	return b.String(), nil
}

type toneTTS struct{}

func (toneTTS) Synthesize(text string) (voice.Audio, error) {
	return voice.Audio{Samples: make([]float32, 100*len(text)), SampleRate: 22050}, nil
}
func (toneTTS) SampleRate() int { return 22050 }
func (toneTTS) Close()          {}

type client struct {
	t   *testing.T
	ctx context.Context
	c   *websocket.Conn
}

func dial(t *testing.T, srv *Server) *client {
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(4 << 20)
	t.Cleanup(func() { c.CloseNow() })
	return &client{t, ctx, c}
}

func (c *client) send(s string) { c.c.Write(c.ctx, websocket.MessageText, []byte(s)) }

// until reads messages until an event of type stop for turn arrives.
func (c *client) until(stop string, turn uint32) (evs []agent.Event, audioTurns []uint32) {
	for {
		typ, data, err := c.c.Read(c.ctx)
		if err != nil {
			c.t.Fatalf("read: %v (events so far %+v)", err, evs)
		}
		if typ == websocket.MessageBinary {
			audioTurns = append(audioTurns, binary.LittleEndian.Uint32(data))
			continue
		}
		var e agent.Event
		json.Unmarshal(data, &e)
		evs = append(evs, e)
		if e.Type == stop && e.Turn == turn {
			return
		}
	}
}

func find(evs []agent.Event, typ string) *agent.Event {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

// TestVoiceTurn streams a real recording in 20 ms frames, as the browser
// does, and expects a transcript, an assistant reply, and audio.
func TestVoiceTurn(t *testing.T) {
	wavPath := modelDir + "/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/test_wavs/en.wav"
	if _, err := os.Stat(wavPath); err != nil {
		t.Skip("models not downloaded; run make models")
	}
	m := voice.ModelPaths{Dir: modelDir}
	asr, err := voice.NewSherpaASR(m)
	if err != nil {
		t.Fatal(err)
	}
	vad, err := voice.NewSherpaVAD(m)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{ASR: asr, VAD: vad, Agent: &agent.Session{LLM: echoLLM{}, TTS: toneTTS{}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := dial(t, srv)
	// Close models only after the connection (and its turn) is gone.
	t.Cleanup(func() { asr.Close(); vad.Close() })

	pcm := voice.FloatToPCM16(sherpa.ReadWave(wavPath).Samples)
	c.send(`{"type":"ptt_start"}`)
	for i := 0; i < len(pcm); i += 640 {
		c.c.Write(c.ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))])
	}
	c.send(`{"type":"ptt_end"}`)

	evs, audio := c.until("timing", 1)
	evs2, audio2 := c.until("timing", 1) // second timing event comes from the agent
	evs, audio = append(evs, evs2...), append(audio, audio2...)
	tr := find(evs, "transcript")
	if tr == nil || !strings.Contains(strings.ToLower(tr.Text), "chieftain") {
		t.Fatalf("transcript: %+v", tr)
	}
	done := find(evs, "assistant_done")
	if done == nil || !strings.HasPrefix(done.Text, "You said: The tribal chieftain") || !done.Final {
		t.Fatalf("assistant_done: %+v", done)
	}
	if len(audio) == 0 || audio[0] != 1 {
		t.Fatalf("audio frames: %v", audio)
	}
	t.Logf("reply %q, %d audio frames", done.Text, len(audio))
}

// TestInterrupt checks that a new message cancels the turn in progress.
func TestInterrupt(t *testing.T) {
	sess := &agent.Session{LLM: echoLLM{delay: 40 * time.Millisecond}, TTS: toneTTS{}}
	srv := &Server{Agent: sess, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := dial(t, srv)

	c.send(`{"type":"text","text":"one two three four five six seven eight nine ten"}`)
	c.until("llm_delta", 1)
	c.send(`{"type":"text","text":"stop"}`)
	evs, _ := c.until("assistant_done", 2)
	first := find(evs, "assistant_done")
	if first == nil || first.Turn != 1 || first.Final {
		t.Fatalf("turn 1 should end interrupted first: %+v", evs)
	}
	h := sess.History()
	if len(h) != 4 || !strings.HasSuffix(h[1].Content, "[interrupted]") || h[2].Content != "stop" {
		t.Fatalf("history: %+v", h)
	}
}
