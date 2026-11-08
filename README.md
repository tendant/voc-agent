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

Hold the button (or the space bar), speak, and release, or type in the box.
Try:

- "Find my deployment notes and remind me to review them tomorrow."
- "What's the rollback plan?" / "When does the trash go out?"
- "Remind me to stretch in two minutes." (it speaks the reminder when due)
- "What reminders do I have?"

Notes are the Markdown/text files under `./notes` (sample notes included;
point `-notes` at your own folder). They are re-indexed every 2 s. Reminders,
tool calls, and conversation history live in `voc.db` (SQLite).

## Other targets

- `make test` runs the Go tests, including a WebSocket round trip on a real recording.
- `VOC_LIVE=1 go test ./internal/agent ./internal/server -run Live -v` runs
  end-to-end checks against local Ollama (voice loop, notes, reminders).
- `make spike` runs a TTS → VAD → ASR round trip with timings, no browser needed.

Speech runs in-process through [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx):
SenseVoiceSmall for ASR, Silero for VAD, and Piper (lessac, medium) for TTS.
