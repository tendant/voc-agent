# Demo script (about 5 minutes)

Covers the four items on the roadmap's hackathon checklist. Every line below
is also run by voice in `make offline-check`; rehearse with that first.

## Before you start

1. `make offline-check`: dress rehearsal by voice on a fresh database, plus
   proof that only loopback connections are used. It should end with
   `OK: only loopback connections` and `rehearsal OK`.
2. `make demo-reset` for a clean conversation, reminder list and outbound log.
3. Start the server:
   - Offline demo: `make build && ./bin/voc -cloud=fake`. The approval flow
     goes to a clearly labeled local stand-in API.
   - With real Claude: `ANTHROPIC_API_KEY=... ./bin/voc`. Do one test request
     beforehand; it is the only part that needs the network.
4. Open http://127.0.0.1:8080 in Chrome, allow the microphone, and send one
   throwaway message (this warms up audio playback).
5. **Turn Wi-Fi off** (except when using real Claude).

Hold the big button or the space bar while speaking. Speak, then release.

## 1. Offline voice conversation

> **Say:** "Hi! Give me one quick tip for staying focused."

Point at: the transcript, the spoken answer, and the timing bar. `heard` is
the delay from releasing the button to hearing audio (about 0.6–1.7 s). Wi-Fi
is off: speech recognition, the LLM and the voice all run on this laptop.

## 2. Private memory with sources

> **Say:** "What's the rollback plan?"

The answer comes from `notes/work/deployment.md`. Point at the "Looked in
notes" card showing which note was used. Every turn searches the local notes
automatically, so the 8B model answers from them instead of guessing.

## 3. Real task execution

> **Say:** "Find my deployment notes and remind me to review them tomorrow."

Point at:
- the "One moment." acknowledgement while the tools run
- the tool cards (`search_notes`, then "Reminder created … tomorrow at 9:00 AM")
- the Reminders panel; the reminder is a row in SQLite, not just words

> **Say:** "Remind me to stretch in one minute."

Keep talking. A minute later it says "Reminder: Stretch." on its own.

The safety point: the model is never trusted to say it did something. A
sentence like "I've set a reminder" is checked against the tool calls before
it is spoken. If the tool didn't run, the claim is dropped and the model must
call the tool or admit it hasn't done it.

## 4. Safe cloud escalation (the differentiator)

> **Say:** "Ask the cloud to review my billing deployment plan for risks."

The approval dialog opens. Point at:
- **Destination and model**
- **Included:** the deployment note, with "redacted 1 email" (Priya's address
  became `[EMAIL]`)
- **Exact request body**: this exact JSON is what gets sent, nothing else

> **Say:** "Approve."

The answer is spoken. Open the **Outbound log**: the entry shows the bytes
actually transmitted and their SHA-256, captured at the HTTP layer.

> **Say:** "Ask the cloud for a weekly workout plan that fits my knee recovery."

Point at:
- **Stays on this device: Health (local_only)**. Notes under `notes/private/`
  never leave, whatever the model does. If the model copies their text into
  the question, the request is blocked before you're even asked.
- **Check before sending:** words that otherwise appear only in private notes
  are flagged, because paraphrase can't be caught automatically.

> **Say:** "Cancel."

The Outbound log shows `denied` with "Nothing left this device."

## If something goes wrong

| Symptom | Fix |
|---|---|
| No audio | Click anywhere on the page once (browser autoplay rule), then retry. |
| "Server offline — retrying" | Restart `./bin/voc`; the page reconnects by itself. |
| First answer is slow (~2 s extra) | The model was unloaded; the server preloads it at startup, so wait for `llm ready` in the log. |
| Misheard words | Speak a bit slower, or type the line in the text box; everything else behaves the same. |
| "approve" not understood | It asks again; say "yes, send it", or click the button. |
| Real Claude errors | Check `ANTHROPIC_API_KEY`; fall back to `-cloud=fake` for the demo. |
