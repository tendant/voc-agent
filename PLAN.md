# Implementation Plan — Hackathon MVP

Scope: Phase 1 plus the four items on the demo checklist in
[local-personal-voice-agent-roadmap.md](local-personal-voice-agent-roadmap.md).
It is set up so Phases 2–4 can grow from it without a rewrite.

## Key decision: speech processing inside the Go binary via sherpa-onnx

sherpa-onnx has official Go bindings (`github.com/k2-fsa/sherpa-onnx-go`, with
prebuilt macOS arm64 libraries). It runs all three speech models the roadmap
names:

- SenseVoiceSmall for speech recognition (ASR)
- Silero VAD for detecting when someone stops talking
- Piper voices for text-to-speech (TTS)

That leaves one Go process plus Ollama, which matches the roadmap's "one Go
application, no microservices" rule.

**Fallback:** if the bindings cause trouble, switch to a small Python sidecar
(FunASR SenseVoice plus `piper-tts`) that Go calls over local HTTP. Only one Go
file changes, because the speech engines sit behind an interface.

## Architecture (MVP)

```text
React (Vite) ── WS: binary PCM16 frames up, JSON events + PCM audio down ── Go server
                                                                              │
   ┌──────────────┬──────────────┬───────────────┬──────────────┬────────────┤
 voice/         agent/          llm/            tools/         store/       policy/
 VAD·ASR·TTS    turn loop,      Ollama client   search_notes   SQLite+FTS5  router, redaction,
 (sherpa-onnx)  tool calling    (+ cloud)       create/list    notes,msgs,  approvals, audit
                                                reminders      reminders,
                                                               audit, tasks
```

## Repo layout

```
cmd/voc/main.go            # wires everything, serves web/dist + /ws
internal/voice/            # asr.go, tts.go, vad.go (interfaces + sherpa impl)
internal/llm/              # ollama.go (streaming /api/chat, tools), cloud.go
internal/agent/            # session.go: turn loop, sentence chunker → TTS
internal/tools/            # registry.go, notes.go, reminders.go (typed structs → JSON schema)
internal/store/            # sqlite.go, migrations/*.sql
internal/policy/           # router.go, redact.go, audit.go
web/                       # React + Vite: PTT button, transcript, tool cards, approval modal
models/                    # gitignored; scripts/fetch-models.sh downloads them
notes/                     # sample markdown notes for the demo
```

## Milestones

### M0: Skeleton (~1h) — ✅ done 2026-11-08

Spike result: the sherpa-onnx Go bindings work, so the Python fallback isn't
needed. Measured on the M4 Max: models load in ~0.4–0.6 s, Piper RTF ≈ 0.02
(73 ms for 3.3 s of speech), Silero VAD ~15 ms, and SenseVoice ~40–60 ms for
3–7 s utterances. That leaves almost the whole latency budget for the LLM.


- Set up the Go module, Vite React app, and a `make dev` target.
- Write `scripts/fetch-models.sh` to download SenseVoiceSmall (int8), Silero
  VAD, and one Piper voice (e.g. `en_US-amy-medium`).
- Echo audio over the WebSocket to confirm the round trip works.
- Spike the sherpa-onnx Go bindings (ASR + TTS on a test file).

### M1: Offline voice loop (~half a day) → demo item 1

- **Browser:** a push-to-talk button. An AudioWorklet resamples the mic to
  16 kHz mono PCM16 and sends 20 ms frames, so Go never has to decode webm/opus.
- **Server:** gather frames until push-to-talk is released (VAD only trims
  silence for now). Run SenseVoice and send `{type:"transcript", final:true}`.
- **LLM:** stream from Ollama `/api/chat` with `qwen3:latest` and `think:false`
  (thinking adds too much latency for voice). Keep the last N turns as history.
- **Sentence chunker:** as soon as a sentence finishes, run Piper on it and
  stream the audio back. The browser queues playback with Web Audio. This is
  what makes time-to-first-audio under 3 s possible.
- **Instrumentation:** log timings for each stage
  (`vad_end → asr_done → llm_ttft → tts_first_audio → playback_start`) and
  show them in a debug panel.
- **Exit check:** switch Wi-Fi off and hold a conversation.

### M2: Memory and tools (~half a day) → demo items 2 and 3

- **SQLite:** `modernc.org/sqlite` (pure Go, has FTS5). Tables: `messages`,
  `notes(path, title, body)` with an FTS5 index,
  `reminders(id, idem_key UNIQUE, text, due_at, status)`, `tool_calls`.
- **Note ingestion:** scan `notes/*.md` on startup and whenever a file changes.
- **Typed tools:** each tool is a Go struct whose JSON schema is generated from
  the struct, then passed to Ollama's `tools` parameter:
  - `search_notes(query)` returns snippets plus the file path as a source
    reference.
  - `create_reminder(text, when)` resolves the time on the server. Relative
    times like "tomorrow 9am" are parsed in Go, not trusted from the LLM. The
    idempotency key is a hash of the turn ID and the arguments.
  - `list_reminders()`
- **Truthful outcomes:** tool results go back to the model, and the UI shows a
  card with the real outcome. The system prompt forbids claiming success
  without a tool result.
- **Scheduler:** a goroutine fires reminders that are due. They're spoken over
  the active WebSocket and stored as `fired`.
- **Exit check:** "Find my deployment notes and remind me to review them
  tomorrow" finds the note, cites it, and creates a single reminder row.

### M3: Safe cloud escalation (~half a day) → demo item 4

- **Data labels:** every piece of context carries a privacy label (`public`,
  `personal`, `local_only`). Notes in `notes/private/` and all reminders are
  `local_only`.
- **Router:** an `escalate_to_cloud(task)` tool, or the user saying "ask the
  cloud", proposes an escalation. Hard rules apply first, so `local_only`
  content can never be in the payload; then emails, phone numbers, and similar
  personal data in `personal` content are redacted.
- **Approval:** the UI shows a modal with the destination, the reason, and the
  **exact JSON payload** to be sent. Nothing leaves the machine until the user
  approves by button or by saying "approve".
- **Audit:** an `audit` table stores the decision, destination, payload hash,
  and full payload, and the UI can display it.
- **Cloud call:** the Claude API (default `claude-sonnet-5-5` for latency,
  configurable), with the key read from an env var. With no key, escalation is
  turned off and the UI says so.
- **Exit check:** a hard question that mentions a private note is escalated
  with that note's content stripped, and the audit log shows exactly what was
  sent.

### M4: Demo polish (~2–3h)

- Seed the demo notes, write a demo script, and add a README with setup steps.
- `make offline-check` confirms no outbound connections when cloud mode is off.
- Rehearse once fully offline.

## WebSocket protocol

| Direction | Message |
|---|---|
| Up | binary PCM16 frames; `{type:"ptt_start"\|"ptt_end"}`; `{type:"text", text}`; `{type:"approve"\|"deny", id}` |
| Down | `transcript`, `llm_delta`, `tool_call`/`tool_result`, `approval_request{id,dest,payload}`, binary TTS audio chunks with a sequence-number header, `timing`, `reminder_fired` |

## Deliberately deferred

- Wake word, barge-in, and full-duplex audio
- Calendar
- Semantic or vector search
- A full durable task journal; only the idempotency key and `tool_calls` table
  go in now
- Mobile and remote access

## Risks

- **sherpa-onnx Go bindings and cgo on macOS:** spiked in M0; Python sidecar is
  the fallback.
- **Qwen3-8B tool-calling reliability:** small tool set, `temperature` ≈ 0.3,
  validate arguments in Go, re-prompt once with the error on a bad call.
- **Ollama version mismatch:** client 0.20.0 warns the server differs; restart
  or upgrade Ollama before M1.
