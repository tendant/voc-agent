// voc is the local personal voice agent server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tendant/voc-agent/internal/agent"
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
	reg := tools.NewRegistry(tools.Personal(st, nil, sched.Wake)...)
	reg.Log = func(c tools.Call, name string, args, result json.RawMessage, ok bool, d time.Duration) {
		log.Info("tool", "name", name, "args", string(args), "ok", ok, "ms", d.Milliseconds())
		if err := st.LogToolCall(context.WithoutCancel(ctx), c.TurnKey, name, string(args), string(result), ok, d); err != nil {
			log.Error("log tool call", "err", err)
		}
	}
	sess := &agent.Session{LLM: ol, TTS: tts, Tools: reg, Memory: st, Recall: agent.StoreRecall(st)}
	fatal(log, sess.Restore(ctx))

	srv := &server.Server{ASR: asr, VAD: vad, TTS: tts, Agent: sess, Store: st, OnConnect: sched.Wake, Log: log}
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
