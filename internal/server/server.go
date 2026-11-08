// Package server exposes the HTTP and WebSocket endpoints the browser talks to.
//
// WebSocket protocol (M0 subset, see PLAN.md):
//
//	up   text   {"type":"ptt_start"} / {"type":"ptt_end"}
//	up   binary PCM16 LE mono @ 16 kHz, any frame size (browser sends 20 ms)
//	down text   {"type":"transcript","text":...,"final":true}, {"type":"timing",...}, {"type":"error",...}
//	down binary [uint32 LE seq][uint32 LE sample rate][PCM16 LE mono]
package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/tendant/voc-agent/internal/voice"
)

type Server struct {
	ASR    voice.ASR
	VAD    voice.VAD
	Static http.Handler // built web UI; nil in dev (Vite serves it)
	Log    *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /ws", s.handleWS)
	if s.Static != nil {
		mux.Handle("/", s.Static)
	}
	return mux
}

type msg struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	Final bool           `json:"final,omitempty"`
	Lang  string         `json:"lang,omitempty"`
	MS    map[string]int `json:"ms,omitempty"`
	Error string         `json:"error,omitempty"`
}

// maxUtterance caps buffered audio per push-to-talk press (60 s).
const maxUtterance = 60 * voice.SampleRate * 2

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Vite dev server proxies from another port; this is a local-only app.
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*"},
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx := r.Context()
	log := s.Log.With("remote", r.RemoteAddr)
	log.Info("ws connected")

	var (
		buf       []byte
		recording bool
		seq       uint32
	)
	send := func(m msg) error {
		b, _ := json.Marshal(m)
		return c.Write(ctx, websocket.MessageText, b)
	}

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled) {
				log.Warn("ws read", "err", err)
			}
			log.Info("ws closed")
			return
		}
		if typ == websocket.MessageBinary {
			if recording && len(buf)+len(data) <= maxUtterance {
				buf = append(buf, data...)
			}
			continue
		}
		var m msg
		if err := json.Unmarshal(data, &m); err != nil {
			send(msg{Type: "error", Error: "bad json"})
			continue
		}
		switch m.Type {
		case "ptt_start":
			buf, recording = buf[:0], true
		case "ptt_end":
			recording = false
			if len(buf) < 2 {
				continue
			}
			pcm := buf
			// M0 round trip: echo what we heard, then transcribe it.
			seq++
			if err := c.Write(ctx, websocket.MessageBinary, audioFrame(seq, voice.SampleRate, pcm)); err != nil {
				return
			}
			if err := s.transcribe(pcm, send); err != nil {
				log.Error("transcribe", "err", err)
				send(msg{Type: "error", Error: err.Error()})
			}
		default:
			send(msg{Type: "error", Error: "unknown type " + m.Type})
		}
	}
}

func (s *Server) transcribe(pcm []byte, send func(msg) error) error {
	samples := voice.PCM16ToFloat(pcm)
	t0 := time.Now()
	speech := s.VAD.Trim(samples)
	tVAD := time.Since(t0)
	if len(speech) == 0 {
		return send(msg{Type: "transcript", Final: true, Text: ""})
	}
	t1 := time.Now()
	tr, err := s.ASR.Transcribe(speech)
	if err != nil {
		return err
	}
	tASR := time.Since(t1)
	s.Log.Info("utterance", "audio_s", float64(len(samples))/voice.SampleRate,
		"vad_ms", tVAD.Milliseconds(), "asr_ms", tASR.Milliseconds(), "text", tr.Text)
	if err := send(msg{Type: "transcript", Final: true, Text: tr.Text, Lang: tr.Lang}); err != nil {
		return err
	}
	return send(msg{Type: "timing", MS: map[string]int{
		"vad": int(tVAD.Milliseconds()), "asr": int(tASR.Milliseconds()),
	}})
}

// audioFrame builds a downstream binary audio message.
func audioFrame(seq uint32, rate int, pcm []byte) []byte {
	out := make([]byte, 8+len(pcm))
	binary.LittleEndian.PutUint32(out[0:], seq)
	binary.LittleEndian.PutUint32(out[4:], uint32(rate))
	copy(out[8:], pcm)
	return out
}
