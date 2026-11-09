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

### Browser agent

If Google Chrome is installed, web pages open in **voc's own Chrome window**
(a separate profile in `~/.voc/chrome-profile`, not logged into your
accounts), so voc can also act on them:
- "Play lo-fi jazz" plays the first YouTube result; "pause", "next",
  "louder", "skip ahead" control it.
- "What's this page about?" / "How long do I boil the pasta on this page?"
  answer from the page text, read by the local model.
- "Click next page" clicks a link or button by its text. voc refuses buttons
  that buy, send, post, subscribe or delete.
- "Search this site for tomato soup", "type 94110 in the zip code box"
  (Enter is pressed only when you say search, submit or press enter; password
  and payment fields are refused).
- "Open the second result", "play the third video".
- "Go back", "scroll down", "go to the top", "reload", also chained: "go
  back, go back", "scroll down twice".

Simple commands ("pause", "next", "louder", "scroll down", "go back"…) run
instantly without the model (≈0.2 s), even phrased politely ("can you go back
please"). Page actions are offered to the model only while voc's browser
window is open.

Page text is treated as untrusted. After voc reads a page, the tools that
could carry data out (cloud, searches, links, playing) run only if your own
words asked for them, so a page can't instruct it to leak your notes.

With hands-free on while something plays, the speakers reach the mic, so voc
only accepts media commands ("pause", "next", "turn it down") or anything
starting with "hey voc". `-agent-browser=false` turns the browser agent off
(pages then open in your normal browser).

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

### Hands-free listening

Click **Hands-free** (next to the text box) to stop holding the button. The
mic streams continuously and the server ends an utterance after about 0.7 s
of silence. Say "stop listening" (or click again) to turn it off.
- **Half-duplex:** voc doesn't hear you while it is talking (the mic is muted
  during its speech plus 0.35 s), so you can't talk over it yet.
- **Falls asleep after 10 s without you talking.** Then only **"Hey
  Victor"** wakes it ("Yes?"), or say it in one breath: "Hey Victor, what
  time is it?". The countdown pauses while voc is thinking or talking.
  "Go to sleep" sleeps it right away; "stop listening" turns hands-free off.
  `-wake-word` and `-sleep-after` change these (`-sleep-after=0` never sleeps).
- While asleep, nothing is answered; transcripts are only checked for the
  wake word, locally. Noise and filler ("um", "hmm") are always ignored.
- Push-to-talk (button or space bar) still works while it is on.

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
