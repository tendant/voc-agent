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

	"github.com/tendant/voc-agent/internal/server"
	"github.com/tendant/voc-agent/internal/voice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (localhost only by default)")
	models := flag.String("models", "models", "model directory")
	static := flag.String("static", "web/dist", "built web UI to serve; empty to disable")
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
	log.Info("models loaded", "ms", time.Since(t0).Milliseconds())

	srv := &server.Server{ASR: asr, VAD: vad, Log: log}
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
