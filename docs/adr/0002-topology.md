# ADR 0002 — Topology: bot on the services box, inference on the Mac Mini over LAN

- Status: accepted
- Date: 2026-09-29

## Context

Two constraints from Colby, both of which push against the obvious design:

1. **The Mac Mini is reserved for inference.** It runs Ollama with Metal and is not to be
   loaded with other workloads.
2. **The Obsidian vault lives on the Mac Mini**, and the bot writes into it over a network
   share.

The original instinct — put everything in one Docker Compose stack — is unavailable, because
"one stack" would mean the inference host is also the bot host.

## Decision

**The bot is a single small Go container on the services machine. The Mac Mini is a plain HTTP
server to it, reached over the LAN.**

```
Discord ──▶ yt-digest (Go, CPU-only, services box)
               │
               ├─▶ yt-dlp (in-container) ──▶ captions.vtt
               ├─▶ http://<mac-mini>:11434/v1/chat/completions   ← LAN, OpenAI-compatible
               ├─▶ http://<whisper-host>:8000                     ← LAN, fallback only
               └─▶ //<mac-mini>/vault/Video Notes/*.md             ← SMB/NFS network share
```

The vault path is mounted into the container as a bind mount. From inside the container it is
a normal filesystem path; the network protocol is the host's problem, not the bot's.

Ollama's OpenAI-compatible `/v1/chat/completions` endpoint means **no client library**. A
handful of structs and one `http.Post` keeps the binary and dependency list minimal, which is
the same reasoning that made whisper-receiver's Go port worth doing.

## Consequences

- **The bot image stays small and has no GPU passthrough concerns** — it never needed any.
  Apple's lack of Metal passthrough into containers is irrelevant to this design, which is the
  main reason the split is comfortable rather than a compromise.
- **A network partition between the box and the Mac Mini is a new failure mode.** The bot must
  distinguish "Ollama unreachable" from "Ollama returned an error", and must not write a
  partial or empty note for either. Connection refused gets a distinct in-channel reply.
- **Model choice is configuration, not code.** `OLLAMA_BASE_URL`, `OLLAMA_MODEL`, and
  `WHISPER_URL` are env vars. Colby wants to benchmark 2–3 models on real videos before
  committing, and that must not require a rebuild.
- **Latency is bounded by the slowest link, not the model.** A 2-hour video's map-reduce over
  a LAN is I/O-cheap; the model is the bottleneck, as intended.
- **The network share is the durability risk** (see ADR 0004). A write that reports success
  locally may not be visible to Obsidian until the SMB oplock breaks.
