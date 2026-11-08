# Local Personal Voice Agent — Product & Engineering Roadmap

## Vision

Build a private, local-first personal voice agent that can listen, remember, reason, execute tasks, and proactively help. Run routine inference on hardware you own, with explicit policy-controlled cloud escalation for difficult tasks.

The product is not just a voice chatbot: it combines voice, persistent memory, reliable tools, durable execution, and transparent privacy boundaries.

## 12-week roadmap

| Phase | Timeline | Focus | Exit criterion |
|---|---|---|---|
| 1. Voice MVP | Day 1–2 | Local ASR → LLM → TTS | Spoken conversation works offline |
| 2. Useful assistant | Weeks 1–2 | Memory, reminders, notes, calendar | Multi-step personal task succeeds |
| 3. Hybrid intelligence | Weeks 3–4 | Cloud fallback, privacy, audit | Private-only policy is enforced |
| 4. Reliable agent | Weeks 5–6 | Persistence, approvals, recovery | Restart does not duplicate actions |
| 5. Always available | Weeks 7–8 | Background service, mobile, proactive tasks | Reach home agent from phone |
| 6. Personal AI platform | Weeks 9–12 | Shared memory, skills, evaluation | Multiple clients share reliable state |

## Phase 1 — Hackathon MVP (Day 1–2)

**Goal:** Have a real conversation with a completely local voice assistant.

- Browser push-to-talk and live transcription display.
- Local ASR: SenseVoiceSmall.
- Local LLM: Qwen3-8B via Ollama.
- Local TTS: Piper.
- Go WebSocket backend and basic conversation history.
- Demonstrate operation without internet access.

**Exit criterion:** Speak a request and hear a response while offline.

## Phase 2 — Useful personal assistant (Weeks 1–2)

**Goal:** Move beyond conversation into reliable personal actions.

- Typed Go tools for reminders and notes.
- Read-only calendar integration.
- Local file and note search with SQLite FTS5.
- Persist conversation context and useful memories with source references.
- Show actual tool outcomes; never claim an action completed before confirmation.

**Exit criterion:** “Find my deployment notes and remind me to review them tomorrow” retrieves relevant notes and creates a reminder.

## Phase 3 — Hybrid local/cloud intelligence (Weeks 3–4)

**Goal:** Get frontier-model capability without unnecessarily exposing personal data.

- Router based on task type, capability, latency, cost, and privacy.
- Deterministic privacy restrictions as hard constraints.
- Explicit approval for cloud escalation where needed.
- Redact or minimize cloud-bound context.
- Audit decisions, destinations, and actual outbound payloads.
- Benchmark local-only, cloud-only, and hybrid performance.

**Exit criterion:** Difficult tasks can use cloud help, while local-only data remains local.

## Phase 4 — Reliable agent execution (Weeks 5–6)

**Goal:** Make actions safe, auditable, and recoverable.

- Persist tasks, transitions, and tool outcomes.
- Idempotency keys for side-effecting actions.
- Explicit approvals for sensitive actions.
- Resume after crashes and network failures.
- Display execution history, pending approvals, and failure states.
- Consider DBOS only if it reduces complexity over a small Go task journal.

**Exit criterion:** Restart mid-task without duplicating completed actions.

## Phase 5 — Always-available assistant (Weeks 7–8)

**Goal:** Make the assistant usable throughout the day.

- Run as a background service on a single host.
- Secure remote access for a mobile-friendly web app.
- Scheduled morning briefings and reminders.
- Optional wake word and improved turn taking.
- Health checks, logs, and automatic service restart.

**Exit criterion:** Speak to the home-hosted assistant from a phone and get a useful response.

## Phase 6 — Personal AI platform (Weeks 9–12)

**Goal:** Create a reusable personal AI runtime.

- Synchronize memory across authorized devices.
- Add retention, provenance, correction, and deletion controls for memory.
- Extensible skill registry with permission boundaries.
- Proactive triggers and user-controlled notifications.
- Evaluation harness for task success, latency, cost, privacy, and recovery.
- Keep the voice interface independent of the agent runtime.

**Exit criterion:** Multiple clients access one reliable agent and consistent task/memory state.

## Target architecture

```text
Desktop / Mobile / Wearable
            |
       Voice Gateway
    VAD · ASR · TTS · WS
            |
     Go Agent Runtime
 Context · Planner · Tasks
       /     |      \
Local Models Memory    Tools
ASR/LLM/TTS SQLite    Go/MCP
       \     |      /
       Policy Router
            |
  Approved Cloud Fallback

Cross-cutting: permissions · audit · durable execution
```

Initially deploy one Go application with separate local inference processes; avoid Kubernetes and unnecessary microservices.

## Technology decisions

| Component | Initial choice | Later evolution |
|---|---|---|
| Backend | Go | Keep Go |
| UI | React | PWA, optional native app |
| Audio transport | WebSocket | Optional LiveKit |
| ASR | SenseVoiceSmall | Streaming ASR |
| LLM | Ollama + Qwen3-8B | vLLM/SGLang or Liquid models |
| TTS | Piper | Higher-quality streaming TTS |
| VAD | Silero VAD | Better turn-taking |
| Storage | SQLite | Postgres if needed |
| Search | SQLite FTS5 | Hybrid semantic retrieval |
| Tools | Typed Go tools | MCP adapters |
| Execution | Persisted task journal | DBOS if warranted |
| Deployment | Single host | Optional distributed inference |

Reuse existing agent-runner ideas and components for permissions, tool execution, and persistence instead of creating a second agent framework.

## Proposed performance targets

These are engineering goals, not measured results.

| Metric | MVP | Mature |
|---|---:|---:|
| ASR finalization after speech ends | < 1 s | < 300 ms |
| Time to first spoken response | < 3 s | < 1 s |
| Simple local task success | 80%+ | 95%+ |
| Duplicate reminders after retry | 0 | 0 |
| Private-data cloud policy violations | 0 | 0 |
| Offline basic voice commands | Supported | Supported |

Instrument VAD, ASR, LLM time-to-first-token, TTS time-to-first-audio, and playback separately.

## Hackathon demo checklist

- [ ] **Offline voice conversation:** microphone → ASR → local LLM → TTS.
- [ ] **Private memory retrieval:** answer from saved notes with source reference.
- [ ] **Real task execution:** create a reminder through a typed Go tool.
- [ ] **Safe cloud escalation:** show approval, policy decision, and exact outbound data.

Prioritize the first three; make safe escalation the differentiating fourth demo if time permits.

## Deliberately defer

- Wake-word detection and always-listening mode.
- Voice cloning and complex audio personas.
- Multi-user support.
- Large RAG infrastructure before basic search proves insufficient.
- Autonomous browser operation.
- Cross-device synchronization.
- Full-duplex interruption and natural turn-taking optimization.

## Product direction

The long-term opportunity is a **personal AI runtime that owns memory, permissions, and task execution**, with voice as its primary interface. Potential directions include a consumer personal assistant, a developer platform for local-first agents, and privacy-focused professional voice workflows.

**Recommendation:** Build a genuinely useful single-user assistant first. Within two weeks, use it daily for reminders, private knowledge retrieval, and productivity. Improve real task completion before expanding the infrastructure.
