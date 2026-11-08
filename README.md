# voc-agent

A private, local-first personal voice agent. See
[the roadmap](local-personal-voice-agent-roadmap.md) and [PLAN.md](PLAN.md).

## Requirements

macOS arm64 (Linux should work too), Go 1.24+, Node 20+, and
[Ollama](https://ollama.com) with `qwen3:latest` (needed from M1 on).

## Run

```sh
make models   # one-time: ~400 MB of speech models into ./models
make dev      # Go server on :8080 + Vite on :5173 → open http://localhost:5173
```

Or build a single binary that serves the UI itself:

```sh
make build && ./bin/voc   # http://127.0.0.1:8080
```

Hold the button (or the space bar), speak, and release. Right now (M0) the
server plays your audio back and shows the transcript plus VAD/ASR timings.

## Other targets

- `make test` runs the Go tests, including a WebSocket round trip on a real recording.
- `make spike` runs a TTS → VAD → ASR round trip with timings, no browser needed.

Speech runs in-process through [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx):
SenseVoiceSmall for ASR, Silero for VAD, and Piper (lessac, medium) for TTS.
