// Package server exposes the HTTP and WebSocket endpoints the browser talks to.
//
// WebSocket protocol (see PLAN.md):
//
//	up   text   {"type":"ptt_start"} / {"type":"ptt_end"} / {"type":"text","text":...}
//	            {"type":"listen_start"} / {"type":"listen_stop"} (hands-free: the
//	            server finds utterances in the continuous stream)
//	up   binary PCM16 LE mono @ 16 kHz, any frame size (browser sends 20 ms)
//	down text   agent.Event JSON: turn_start, transcript, llm_delta, tool_call,
//	            tool_result, assistant_done, timing, reminder_fired, error,
//	            speech (hands-free: ok=true when speech starts, false when it ends),
//	            ignored (hands-free: noise or filler that didn't become a turn),
//	            listen_stopped (hands-free turned off by voice)
//	down binary [uint32 LE turn][uint32 LE sample rate][PCM16 LE mono]
//	            turn = NotifyTurn for reminder announcements (always played)
//
// A new ptt_start or text message cancels the turn in progress; the client
// drops audio for turns other than the latest turn_start.
package server

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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
	// NewSegmenter creates a per-connection utterance detector for
	// hands-free listening; nil disables hands-free mode.
	NewSegmenter func() (voice.Segmenter, error)
	// MediaPlaying reports whether voc's browser is playing audio. Its sound
	// reaches the mic, so hands-free then accepts only media commands (or
	// anything starting with "hey voc"). May be nil.
	MediaPlaying func() bool
	// WakeWord wakes hands-free listening after it has gone to sleep
	// (DefaultWakeWord if empty). SleepAfter is how much listening time
	// without the user talking puts it to sleep; 0 means never.
	WakeWord   string
	SleepAfter time.Duration
	Static     http.Handler // built web UI; nil in dev (Vite serves it)
	Log        *slog.Logger
	Cloud      CloudStatus // reported by /api/status

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
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
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
		seg       voice.Segmenter // hands-free; nil when off
		speaking  bool
		awake     = true // hands-free: false = only the wake word is heard
		// idle counts mic samples since the user last spoke or voc last
		// finished. It is an audio clock: the browser sends no mic audio
		// while voc talks, so the countdown pauses during replies.
		idle int
		// pos is the hands-free stream position in samples; sleptAt is where
		// it fell asleep, so speech that began before that still counts.
		pos, sleptAt int
		// recent is the last few seconds of hands-free audio, so a wake word
		// the voice detector skipped can still be found while asleep.
		recent []byte
	)
	const lookBack = 3 * voice.SampleRate / 2 // 1.5 s before an utterance
	wake := cmp.Or(s.WakeWord, DefaultWakeWord)
	setAwake := func(a bool) {
		idle = 0
		if a == awake {
			return
		}
		awake = a
		if !a {
			sleptAt = pos
		}
		state := map[bool]string{true: "awake", false: "asleep"}[a]
		sink.Event(agent.Event{Type: "listen_state", Text: state, Name: wake})
		log.Info("hands-free", "state", state)
	}
	turnBusy := func() bool {
		if cancel == nil {
			return false
		}
		select {
		case <-done:
			return false
		default:
			return true
		}
	}
	sayNotice := func(text string) {
		if s.TTS != nil {
			if a, err := s.TTS.Synthesize(text); err == nil {
				sink.Audio(NotifyTurn, a)
			}
		}
	}
	defer func() {
		if seg != nil {
			seg.Close()
		}
	}()
	stopTurn := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stopTurn()
	// startTurn runs a turn from audio (pcm), typed text, or an utterance
	// already transcribed (heard).
	startTurn := func(pcm []byte, text string, heard *heardUtterance) {
		stopTurn()
		var tctx context.Context
		tctx, cancel = context.WithCancel(ctx)
		done = make(chan struct{})
		turn := s.turns.Add(1)
		go func(done chan struct{}) {
			defer close(done)
			s.runTurn(tctx, turn, pcm, text, heard, sink, log)
		}(done)
	}
	// While an approval is pending, the next utterance answers it.
	// "approve"/"cancel" resolve it; a short unclear reply re-prompts;
	// anything longer cancels it and becomes a new request.
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
		s.resolve(p.id, false)
		return false
	}
	stopListening := func(spoken bool) {
		if seg != nil {
			seg.Close()
			seg, speaking = nil, false
		}
		if spoken {
			sink.Event(agent.Event{Type: "listen_stopped"})
			if s.TTS != nil {
				if a, err := s.TTS.Synthesize("Okay, I've stopped listening."); err == nil {
					sink.Audio(NotifyTurn, a)
				}
			}
		}
	}
	// utterance handles one complete utterance. Push-to-talk audio goes
	// straight to a turn; hands-free audio is transcribed first so noise,
	// filler and "stop listening" never become turns.
	utterance := func(pcm []byte, handsFree bool) {
		if len(pcm) < 2 {
			return
		}
		p := s.pending()
		if !handsFree && p == nil {
			startTurn(pcm, "", nil)
			return
		}
		start := time.Now()
		// Hands-free segments are already trimmed (with pre-roll) by the
		// segmenter; trimming again can drop a short "Hey Victor".
		tr, ms, err := s.transcribe(pcm, !handsFree)
		if err != nil {
			sink.Event(agent.Event{Type: "error", Error: err.Error()})
			return
		}
		if handsFree && p == nil {
			if isNoise(tr.Text) {
				sink.Event(agent.Event{Type: "ignored", Text: tr.Text})
				return
			}
			if isStopListening(tr.Text) {
				sink.Event(agent.Event{Type: "transcript", Final: true, Text: tr.Text, Lang: tr.Lang})
				stopListening(true)
				return
			}
			rest, woke := wakeMatch(wake, tr.Text)
			// An utterance that started before voc fell asleep (it fell
			// asleep while the user was starting to talk) is still heard.
			began := pos - len(pcm)/2 - int(voice.SegmentSilence*voice.SampleRate)
			if !awake && !woke && began < sleptAt {
				setAwake(true)
			}
			if !awake && !woke {
				// "Hey Victor" may have been too short for the voice detector
				// and left out of the segment: listen again with the moments
				// just before it.
				if n := len(pcm) + 2*lookBack + 2*int(voice.SegmentSilence*voice.SampleRate); len(recent) > len(pcm) {
					wide := recent[max(0, len(recent)-n):]
					if wtr, _, err := s.transcribe(wide, false); err == nil {
						if r, ok := wakeFind(wake, wtr.Text); ok {
							rest, woke = r, true
						}
					}
				}
			}
			if !awake {
				if !woke {
					sink.Event(agent.Event{Type: "ignored", Text: tr.Text, Error: "asleep: say \"Hey " + wake + "\""})
					return
				}
				setAwake(true)
				if isNoise(rest) {
					sink.Event(agent.Event{Type: "transcript", Final: true, Text: tr.Text, Lang: tr.Lang})
					sayNotice("Yes?")
					return
				}
				tr.Text = rest // "Hey Victor, what time is it?" in one breath
			} else if woke {
				if isNoise(rest) {
					sayNotice("Yes?")
					idle = 0
					return
				}
				tr.Text = rest
			}
			if goToSleepRe.MatchString(normalize(tr.Text)) {
				sink.Event(agent.Event{Type: "transcript", Final: true, Text: tr.Text, Lang: tr.Lang})
				setAwake(false)
				sayNotice("Okay. Say hey " + wake + " when you need me.")
				return
			}
			if s.MediaPlaying != nil && s.MediaPlaying() && !woke && !mediaCommandRe.MatchString(normalize(tr.Text)) {
				// Probably the video talking, not the user.
				sink.Event(agent.Event{Type: "ignored", Text: tr.Text, Error: "media is playing: say a media command or start with \"Hey " + wake + "\""})
				return
			}
			idle = 0
		}
		if p != nil {
			sink.Event(agent.Event{Type: "transcript", Turn: p.turn, Final: true, Text: tr.Text, Lang: tr.Lang})
			if tr.Text == "" || answer(tr.Text) {
				return
			}
		}
		if tr.Text == "" {
			return
		}
		startTurn(nil, "", &heardUtterance{tr: tr, ms: ms, start: start, handsFree: handsFree})
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
			if seg != nil && !recording {
				pos += len(data) / 2
				recent = append(recent, data...)
				if keep := 2 * 12 * voice.SampleRate; len(recent) > keep { // 12 s
					recent = append([]byte(nil), recent[len(recent)-keep:]...)
				}
				for _, u := range seg.Accept(voice.PCM16ToFloat(data)) {
					utterance(voice.FloatToPCM16(u), true)
				}
				// Sleep after SleepAfter of listening with nothing going on.
				if seg != nil && awake && s.SleepAfter > 0 {
					if turnBusy() || s.pending() != nil || seg.Speaking() {
						idle = 0
					} else if idle += len(data) / 2; idle > int(s.SleepAfter.Seconds()*voice.SampleRate) {
						setAwake(false)
					}
				}
				if seg != nil && seg.Speaking() != speaking {
					speaking = seg.Speaking()
					sp := speaking
					sink.Event(agent.Event{Type: "speech", OK: &sp})
				}
			}
			continue
		}
		var m upMsg
		if err := json.Unmarshal(data, &m); err != nil {
			sink.Event(agent.Event{Type: "error", Error: "bad json"})
			continue
		}
		switch m.Type {
		case "ptt_start":
			if s.pending() == nil {
				stopTurn() // talking over the assistant interrupts it
			}
			if seg != nil {
				seg.Reset() // push-to-talk takes over from hands-free
			}
			buf, recording = nil, true
		case "ptt_end":
			recording = false
			pcm := buf
			buf = nil
			utterance(pcm, false)
		case "listen_start":
			if s.NewSegmenter == nil {
				sink.Event(agent.Event{Type: "error", Error: "hands-free listening is not available"})
				continue
			}
			if seg == nil {
				if seg, err = s.NewSegmenter(); err != nil {
					seg = nil
					sink.Event(agent.Event{Type: "error", Error: err.Error()})
					continue
				}
			}
			awake = false // force the event: turning it on starts awake
			setAwake(true)
		case "listen_stop":
			stopListening(false)
		case "text":
			if t := strings.TrimSpace(m.Text); t != "" && !answer(t) {
				startTurn(nil, t, nil)
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

// heardUtterance is speech already transcribed before its turn started.
type heardUtterance struct {
	tr        voice.Transcript
	ms        map[string]int
	start     time.Time
	handsFree bool
}

// While media plays in voc's browser, hands-free only accepts these, or
// anything after the wake word.
var (
	mediaCommandRe = regexp.MustCompile(`^(please )?(pause|stop|play|resume|continue|next|skip|mute|unmute|louder|quieter|softer|volume (up|down)|turn (it )?(up|down)|go back|go forward|back|forward|rewind|fast forward|restart|start over|stop (the )?(video|music)|pause (the )?(video|music|it)|scroll|page (up|down)|reload|refresh|(go to )?(the )?(top|bottom))\b`)
)

var stopListeningRe = regexp.MustCompile(`^(please )?(stop|pause|quit|end) (listening|hands free|hands-free)\b`)

// isStopListening also accepts a misheard first word ("Dtop listening",
// "Dotop listening"): a short phrase ending in "listening" whose first word
// is close to stop/pause/quit/end. "Don't stop listening" doesn't match.
func isStopListening(text string) bool {
	n := normalize(text)
	if stopListeningRe.MatchString(n) {
		return true
	}
	w := strings.Fields(strings.NewReplacer(",", " ", ".", " ").Replace(n))
	if len(w) < 2 || len(w) > 3 || w[len(w)-1] != "listening" {
		return false
	}
	first := w[0]
	if first == "please" && len(w) == 3 {
		first = w[1]
	} else if len(w) == 3 {
		return false
	}
	// "X listening" alone is almost certainly the command, so allow a
	// rougher match there ("Dotop listening").
	maxDist := 1
	if len(w) == 2 {
		maxDist = 2
	}
	for _, v := range []string{"stop", "pause", "quit", "end"} {
		if len(first) >= 3 && editDistance(first, v) <= maxDist {
			return true
		}
	}
	return false
}

// fillers are utterances that are never requests on their own.
var fillers = map[string]bool{"um": true, "uh": true, "umm": true, "uhh": true, "hmm": true, "hm": true,
	"mm": true, "mhm": true, "ah": true, "oh": true, "huh": true, "er": true, "eh": true, "the": true, "a": true}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), ".!?,;:")))
}

// isNoise reports whether a hands-free transcript is not worth a turn:
// empty, no letters, or just a filler sound.
func isNoise(text string) bool {
	t := normalize(text)
	letters := 0
	for _, r := range t {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 2 {
		return true
	}
	words := strings.Fields(t)
	if len(words) <= 2 {
		all := true
		for _, w := range words {
			all = all && fillers[strings.Trim(w, ".,!?")]
		}
		return all
	}
	return false
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

// runTurn transcribes pcm (unless text or heard is given) and runs the
// agent turn.
func (s *Server) runTurn(ctx context.Context, turn uint32, pcm []byte, text string, heard *heardUtterance, sink agent.Sink, log *slog.Logger) {
	start := time.Now()
	switch { // tell the agent how the request arrived
	case heard != nil && heard.handsFree:
		ctx = agent.WithInputMode(ctx, agent.ModeHandsFree)
	case pcm != nil || heard != nil:
		ctx = agent.WithInputMode(ctx, agent.ModeVoice)
	default:
		ctx = agent.WithInputMode(ctx, agent.ModeTyped)
	}
	sink.Event(agent.Event{Type: "turn_start", Turn: turn})
	if heard != nil {
		start, text = heard.start, heard.tr.Text
		sink.Event(agent.Event{Type: "transcript", Turn: turn, Final: true, Text: heard.tr.Text, Lang: heard.tr.Lang})
		sink.Event(agent.Event{Type: "timing", Turn: turn, MS: heard.ms})
	}
	if pcm != nil {
		tr, ms, err := s.transcribe(pcm, true) // push-to-talk: trim silence
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

func (s *Server) transcribe(pcm []byte, trim bool) (voice.Transcript, map[string]int, error) {
	samples := voice.PCM16ToFloat(pcm)
	t0 := time.Now()
	speech := samples
	if trim {
		speech = s.VAD.Trim(samples)
	}
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

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "no store", http.StatusNotFound)
		return
	}
	ts, err := s.Store.RecentTasks(r.Context(), 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ts == nil {
		ts = []store.Task{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ts)
}
