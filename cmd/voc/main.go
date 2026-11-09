// voc is the local personal voice agent server.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/apple"
	browserpkg "github.com/tendant/voc-agent/internal/browser"
	"github.com/tendant/voc-agent/internal/cloud"
	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/reminders"
	"github.com/tendant/voc-agent/internal/server"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
	"github.com/tendant/voc-agent/internal/voice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (localhost only by default)")
	models := flag.String("models", "models", "model directory")
	static := flag.String("static", "web/dist", "built web UI to serve; empty to disable")
	ollamaURL := flag.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	model := flag.String("model", "qwen3:latest", "Ollama model")
	dbPath := flag.String("db", "voc.db", "SQLite database")
	notesDir := flag.String("notes", "notes", "folder of markdown/text notes to index")
	cloudMode := flag.String("cloud", "auto", "cloud escalation: auto (on if ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN is set), on, off, or fake (local stand-in API for demos)")
	cloudModel := flag.String("cloud-model", cloud.DefaultModel, "Claude model for escalations")
	desktop := flag.Bool("desktop", true, "let voc open web searches, links and allowlisted apps on this computer")
	browser := flag.String("browser", "", "browser for opened links, e.g. Firefox (default: system default browser)")
	apps := flag.String("apps", strings.Join(tools.DefaultApps, ","), "comma-separated apps voc may open")
	wakeWord := flag.String("wake-word", server.DefaultWakeWord, "hands-free: name that wakes voc after it falls asleep (\"Hey Victor\")")
	sleepAfter := flag.Duration("sleep-after", 10*time.Second, "hands-free: fall asleep after this long without you talking; 0 = never")
	appleApps := flag.Bool("apple", runtime.GOOS == "darwin", "let voc use Apple Notes (search, read, create) and Music (play from your library, control); macOS asks for permission on first use")
	agentBrowser := flag.Bool("agent-browser", true, "use voc's own Chrome window for web pages, so it can play videos, read and click (needs Google Chrome)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	m := voice.ModelPaths{Dir: *models}
	t0 := time.Now()
	asr, err := voice.NewSherpaASR(m)
	fatal(log, err)
	defer asr.Close()
	vad, err := voice.NewSherpaVAD(m)
	fatal(log, err)
	defer vad.Close()
	tts, err := voice.NewSherpaTTS(m)
	fatal(log, err)
	defer tts.Close()
	log.Info("speech models loaded", "ms", time.Since(t0).Milliseconds())

	ol := &llm.Ollama{BaseURL: *ollamaURL, Model: *model, Temperature: 0.3, KeepAlive: "30m"}
	go func() {
		// Preload so the first turn doesn't pay ~2 s of model load time.
		t := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := ol.Warm(ctx); err != nil {
			log.Warn("llm warm-up failed; turns will fail until Ollama is reachable", "err", err)
			return
		}
		log.Info("llm ready", "model", *model, "ms", time.Since(t).Milliseconds())
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(*dbPath)
	fatal(log, err)
	defer st.Close()
	n, err := st.SyncNotes(ctx, *notesDir)
	fatal(log, err)
	log.Info("notes indexed", "dir", *notesDir, "changed", n)
	go st.WatchNotes(ctx, *notesDir, 2*time.Second, func(err error) { log.Warn("notes sync", "err", err) })

	sched := (&reminders.Scheduler{Store: st, Log: log}).Init()
	toolset := tools.Personal(st, nil, sched.Wake)
	var agentB *browserpkg.Browser
	if *desktop {
		var allowed []string
		for _, a := range strings.Split(*apps, ",") {
			if a = strings.TrimSpace(a); a != "" {
				allowed = append(allowed, a)
			}
		}
		var opener tools.Opener = &tools.SystemOpener{Browser: *browser}
		if *agentBrowser {
			if b := browserpkg.New(browserpkg.Config{}); b.Available() {
				defer b.Close()
				agentB = b
				opener = &tools.BrowserOpener{B: b, Apps: opener}
				toolset = append(toolset, tools.Browse(st, b)...)
			} else {
				log.Warn("Google Chrome not found; web pages open in the system browser and voc can't act on them")
			}
		}
		toolset = append(toolset, tools.Desktop(st, opener, allowed)...)
		log.Info("desktop actions enabled", "browser", cmp.Or(*browser, "default"), "apps", len(allowed), "agent_browser", agentB != nil)
	}
	var music *apple.Music
	if *appleApps {
		if apple.Available("Notes") {
			toolset = append(toolset, tools.AppleNotes(st, apple.Notes{R: apple.Osascript{}})...)
		}
		if apple.Available("Music") {
			music = &apple.Music{R: apple.Osascript{}}
			toolset = append(toolset, tools.AppleMusic(st, music)...)
		}
		log.Info("apple apps enabled", "notes", apple.Available("Notes"), "music", music != nil)
	}
	if agentB != nil || music != nil {
		toolset = append(toolset, tools.Media(agentB, music)) // pause/next/… for whatever plays
	}
	cl, cloudStatus := setupCloud(*cloudMode, *cloudModel)
	if cl != nil {
		toolset = append(toolset, tools.Escalate(st, cl))
		log.Info("cloud escalation enabled (every request needs approval)", "model", cl.Model, "destination", cl.Destination())
	} else {
		log.Info("cloud escalation disabled", "why", cloudStatus.Why)
	}
	reg := tools.NewRegistry(toolset...)
	reg.Log = func(c tools.Call, name string, args, result json.RawMessage, ok bool, d time.Duration) {
		log.Info("tool", "name", name, "args", string(args), "ok", ok, "ms", d.Milliseconds())
		if err := st.LogToolCall(context.WithoutCancel(ctx), c.TurnKey, name, string(args), string(result), ok, d); err != nil {
			log.Error("log tool call", "err", err)
		}
	}
	sess := &agent.Session{LLM: ol, TTS: tts, Tools: reg, Memory: st, Recall: agent.StoreRecall(st)}
	fatal(log, sess.Restore(ctx))

	srv := &server.Server{ASR: asr, VAD: vad, TTS: tts, Agent: sess, Store: st, OnConnect: sched.Wake, Log: log, Cloud: cloudStatus,
		NewSegmenter: func() (voice.Segmenter, error) { return voice.NewSherpaSegmenter(m) },
		WakeWord:     *wakeWord, SleepAfter: *sleepAfter}
	if agentB != nil || music != nil {
		srv.MediaPlaying = func() bool {
			return (agentB != nil && agentB.Playing(context.Background())) || (music != nil && music.Playing(context.Background()))
		}
	}
	sess.Approver = srv.Approve
	sched.CanDeliver, sched.Deliver = srv.Listening, srv.Notify
	go sched.Run(ctx)
	if *static != "" {
		if _, err := os.Stat(*static); err == nil {
			srv.Static = http.FileServer(http.Dir(*static))
		}
	}

	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", "http://"+*addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(log, err)
	}
}

func fatal(log *slog.Logger, err error) {
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func setupCloud(mode, model string) (*cloud.Claude, server.CloudStatus) {
	switch mode {
	case "off":
		return nil, server.CloudStatus{Why: "turned off with -cloud=off"}
	case "fake":
		api := cloud.NewFakeAPI("This is a canned answer from the local fake cloud API. With a real key, Claude would answer here.")
		c := cloud.New(model, option.WithBaseURL(api.URL()), option.WithAPIKey("fake")).WithHost("FAKE local test API (nothing leaves this machine)")
		return c, server.CloudStatus{Enabled: true, Model: c.Model, Destination: c.Destination()}
	case "auto":
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			return nil, server.CloudStatus{Why: "no ANTHROPIC_API_KEY set (use -cloud=on to rely on another credential source)"}
		}
	case "on":
	default:
		return nil, server.CloudStatus{Why: "unknown -cloud mode " + mode}
	}
	c := cloud.New(model)
	return c, server.CloudStatus{Enabled: true, Model: c.Model, Destination: c.Destination()}
}
