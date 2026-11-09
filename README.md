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
- "Ask the cloud to review my billing deployment plan for risks." (needs `-cloud`)
- "Open YouTube and search for lo-fi jazz." / "Open the calculator." / "Find coffee shops near me on the map."

### Desktop actions

voc can open web searches (YouTube, Google, Wikipedia, Maps, DuckDuckGo,
GitHub), web pages, and allowlisted apps in your browser and on your Mac. It
can't click or read anything on the page. Limits:
- Only http(s) links to public sites; no `file:`, app schemes, localhost or IPs.
- Only allowlisted apps; Terminal and System Settings are deliberately absent.
- Searches and links containing text from `local_only` notes are blocked.

Flags: `-desktop=false` to turn it off, `-browser=Firefox` to pick a browser
(default: the system default), `-apps=...` for a different allowlist.

### Cloud escalation (optional)

```sh
ANTHROPIC_API_KEY=... ./bin/voc      # -cloud=auto turns it on when a key is set
./bin/voc -cloud=fake                # demo the approval flow offline (local stand-in API)
```

Say "ask the cloud …" or "ask Claude …". Before anything leaves the machine
you see the destination, the exact request body, which notes are withheld or
redacted, and any warnings, then approve or cancel (click, or say it). Notes
under `notes/private/`, or with `privacy: local_only` front matter, never
leave the device. Every decision and the exact bytes sent are in the
"Outbound log" (`/api/audit`).

Notes are the Markdown/text files under `./notes` (sample notes included;
point `-notes` at your own folder). They are re-indexed every 2 s. Reminders,
tool calls, and conversation history live in `voc.db` (SQLite).

## Demo

See [DEMO.md](DEMO.md). `make offline-check` rehearses it by voice and checks
that nothing but loopback is used; `make demo-reset` clears state between runs.

## Other targets

- `make test` runs the Go tests, including a WebSocket round trip on a real recording.
- `VOC_LIVE=1 go test ./internal/agent ./internal/server -run Live -v` runs
  end-to-end checks against local Ollama (voice loop, notes, reminders).
- `make spike` runs a TTS → VAD → ASR round trip with timings, no browser needed.

Speech runs in-process through [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx):
SenseVoiceSmall for ASR, Silero for VAD, and Piper (lessac, medium) for TTS.
