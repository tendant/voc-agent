// Package server exposes the HTTP and WebSocket endpoints the browser talks to.
//
// WebSocket protocol (see PLAN.md):
//
//	up   text   {"type":"ptt_start"} / {"type":"ptt_end"} / {"type":"text","text":...}
//	up   binary PCM16 LE mono @ 16 kHz, any frame size (browser sends 20 ms)
//	down text   agent.Event JSON: turn_start, transcript, llm_delta, tool_call,
//	            tool_result, assistant_done, timing, reminder_fired, error
//	down binary [uint32 LE turn][uint32 LE sample rate][PCM16 LE mono]
//	            turn = NotifyTurn for reminder announcements (always played)
//
// A new ptt_start or text message cancels the turn in progress; the client
// drops audio for turns other than the latest turn_start.
package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/voice"
)

// NotifyTurn tags audio that isn't part of a conversation turn (reminders).
const NotifyTurn = ^uint32(0)

type Server struct {
	ASR       voice.ASR
	VAD       voice.VAD
	TTS       voice.TTS      // for reminder announcements; may be nil
	Agent     *agent.Session // single-user: one conversation shared by all tabs
	Store     *store.Store   // for /api/reminders; may be nil
	OnConnect func()         // e.g. wake the reminder scheduler
	Static    http.Handler   // built web UI; nil in dev (Vite serves it)
	Log       *slog.Logger
	Cloud     CloudStatus // reported by /api/status

	turns     atomic.Uint32
	mu        sync.Mutex
	clients   map[*websocket.Conn]wsSink
	approvals map[string]*pendingApproval
}

// CloudStatus describes cloud escalation for the UI.
type CloudStatus struct {
	Enabled     bool   `json:"enabled"`
	Model       string `json:"model,omitempty"`
	Destination string `json:"destination,omitempty"`
	Why         string `json:"why,omitempty"` // when disabled
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /api/reminders", s.handleReminders)
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	if s.Static != nil {
		mux.Handle("/", s.Static)
	}
	return mux
}

type upMsg struct {
	Type string `json:"type"`
	Text string `json:"text"`
	ID   string `json:"id"` // approve / deny
}

// maxUtterance caps buffered audio per push-to-talk press (60 s).
const maxUtterance = 60 * voice.SampleRate * 2

// wsSink sends turn output over the connection. websocket.Conn.Write is safe
// for concurrent use.
type wsSink struct {
	ctx context.Context
	c   *websocket.Conn
}

func (w wsSink) Event(e agent.Event) error {
	b, _ := json.Marshal(e)
	return w.c.Write(w.ctx, websocket.MessageText, b)
}

func (w wsSink) Audio(turn uint32, a voice.Audio) error {
	return w.c.Write(w.ctx, websocket.MessageBinary, audioFrame(turn, a.SampleRate, voice.FloatToPCM16(a.Samples)))
}

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
	sink := wsSink{ctx: ctx, c: c}
	log := s.Log.With("remote", r.RemoteAddr)
	log.Info("ws connected")
	s.addClient(c, sink)
	defer s.removeClient(c)
	if s.OnConnect != nil {
		s.OnConnect()
	}

	var (
		buf       []byte
		recording bool
		cancel    context.CancelFunc
		done      chan struct{}
	)
	stopTurn := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stopTurn()
	startTurn := func(pcm []byte, text string) {
		stopTurn()
		var tctx context.Context
		tctx, cancel = context.WithCancel(ctx)
		done = make(chan struct{})
		turn := s.turns.Add(1)
		go func(done chan struct{}) {
			defer close(done)
			s.runTurn(tctx, turn, pcm, text, sink, log)
		}(done)
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
		var m upMsg
		if err := json.Unmarshal(data, &m); err != nil {
			sink.Event(agent.Event{Type: "error", Error: "bad json"})
			continue
		}
		// While an approval is pending, the next utterance answers it.
		// "approve"/"cancel" resolve it; anything else cancels it and starts
		// a new turn.
		answer := func(text string) (handled bool) {
			p := s.pending()
			if p == nil {
				return false
			}
			if ok, decided := voiceDecision(text); decided {
				s.resolve(p.id, ok)
				return true
			}
			if isShortReply(text) {
				s.reprompt(p, sink) // probably misheard; keep waiting
				return true
			}
			s.resolve(p.id, false) // a new request replaces the pending one
			return false
		}
		switch m.Type {
		case "ptt_start":
			if s.pending() == nil {
				stopTurn() // talking over the assistant interrupts it
			}
			buf, recording = nil, true
		case "ptt_end":
			recording = false
			pcm := buf
			buf = nil
			if len(pcm) < 2 {
				continue
			}
			if p := s.pending(); p != nil {
				tr, _, err := s.transcribe(pcm)
				if err != nil {
					sink.Event(agent.Event{Type: "error", Error: err.Error()})
					continue
				}
				sink.Event(agent.Event{Type: "transcript", Turn: p.turn, Final: true, Text: tr.Text, Lang: tr.Lang})
				if tr.Text == "" || answer(tr.Text) {
					continue
				}
				startTurn(nil, tr.Text)
				continue
			}
			startTurn(pcm, "")
		case "text":
			if t := strings.TrimSpace(m.Text); t != "" && !answer(t) {
				startTurn(nil, t)
			}
		case "approve", "deny":
			if !s.resolve(m.ID, m.Type == "approve") {
				sink.Event(agent.Event{Type: "error", Error: "no pending approval " + m.ID})
			}
		default:
			sink.Event(agent.Event{Type: "error", Error: "unknown type " + m.Type})
		}
	}
}

func (s *Server) addClient(c *websocket.Conn, sink wsSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients == nil {
		s.clients = map[*websocket.Conn]wsSink{}
	}
	s.clients[c] = sink
}

func (s *Server) removeClient(c *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

// Listening reports whether any client is connected.
func (s *Server) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients) > 0
}

func (s *Server) sinks() []wsSink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wsSink, 0, len(s.clients))
	for _, k := range s.clients {
		out = append(out, k)
	}
	return out
}

// Notify announces a fired reminder on every connected client, as an event
// and as speech.
func (s *Server) Notify(r store.Reminder) {
	ev := agent.Event{Type: "reminder_fired", Text: r.Text}
	var audio *voice.Audio
	if s.TTS != nil {
		if a, err := s.TTS.Synthesize("Reminder: " + strings.TrimRight(r.Text, ".") + "."); err == nil {
			audio = &a
		} else {
			s.Log.Error("reminder tts", "err", err)
		}
	}
	for _, k := range s.sinks() {
		k.Event(ev)
		if audio != nil {
			k.Audio(NotifyTurn, *audio)
		}
	}
}

func (s *Server) handleReminders(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "no store", http.StatusNotFound)
		return
	}
	rs, err := s.Store.RecentReminders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rs == nil {
		rs = []store.Reminder{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rs)
}

// runTurn transcribes pcm (unless text is given) and runs the agent turn.
func (s *Server) runTurn(ctx context.Context, turn uint32, pcm []byte, text string, sink agent.Sink, log *slog.Logger) {
	start := time.Now()
	sink.Event(agent.Event{Type: "turn_start", Turn: turn})
	if pcm != nil {
		tr, ms, err := s.transcribe(pcm)
		if err != nil {
			log.Error("transcribe", "err", err)
			sink.Event(agent.Event{Type: "error", Turn: turn, Error: err.Error()})
			return
		}
		sink.Event(agent.Event{Type: "transcript", Turn: turn, Final: true, Text: tr.Text, Lang: tr.Lang})
		sink.Event(agent.Event{Type: "timing", Turn: turn, MS: ms})
		text = tr.Text
		if text == "" {
			sink.Event(agent.Event{Type: "assistant_done", Turn: turn, Final: true})
			return
		}
	}
	if s.Agent == nil {
		return
	}
	err := s.Agent.Turn(ctx, turn, text, start, sink)
	switch {
	case errors.Is(err, context.Canceled):
		log.Info("turn interrupted", "turn", turn)
	case err != nil:
		log.Error("turn", "turn", turn, "err", err)
		sink.Event(agent.Event{Type: "error", Turn: turn, Error: err.Error()})
	default:
		log.Info("turn done", "turn", turn, "ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) transcribe(pcm []byte) (voice.Transcript, map[string]int, error) {
	samples := voice.PCM16ToFloat(pcm)
	t0 := time.Now()
	speech := s.VAD.Trim(samples)
	tVAD := time.Since(t0)
	if len(speech) == 0 {
		return voice.Transcript{}, map[string]int{"vad": int(tVAD.Milliseconds())}, nil
	}
	t1 := time.Now()
	tr, err := s.ASR.Transcribe(speech)
	if err != nil {
		return tr, nil, err
	}
	tASR := time.Since(t1)
	s.Log.Info("utterance", "audio_s", float64(len(samples))/voice.SampleRate,
		"vad_ms", tVAD.Milliseconds(), "asr_ms", tASR.Milliseconds(), "text", tr.Text)
	return tr, map[string]int{"vad": int(tVAD.Milliseconds()), "asr": int((tVAD + tASR).Milliseconds())}, nil
}

// audioFrame builds a downstream binary audio message.
func audioFrame(turn uint32, rate int, pcm []byte) []byte {
	out := make([]byte, 8+len(pcm))
	binary.LittleEndian.PutUint32(out[0:], turn)
	binary.LittleEndian.PutUint32(out[4:], uint32(rate))
	copy(out[8:], pcm)
	return out
}
