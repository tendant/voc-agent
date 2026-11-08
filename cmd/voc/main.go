// voc is the local personal voice agent server.
package main

import (
	"context"
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
	"github.com/tendant/voc-agent/internal/server"
	"github.com/tendant/voc-agent/internal/voice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (localhost only by default)")
	models := flag.String("models", "models", "model directory")
	static := flag.String("static", "web/dist", "built web UI to serve; empty to disable")
	ollamaURL := flag.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	model := flag.String("model", "qwen3:latest", "Ollama model")
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

	srv := &server.Server{ASR: asr, VAD: vad, Agent: &agent.Session{LLM: ol, TTS: tts}, Log: log}
	if *static != "" {
		if _, err := os.Stat(*static); err == nil {
			srv.Static = http.FileServer(http.Dir(*static))
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
