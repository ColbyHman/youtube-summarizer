# Architecture — yt-digest

A Discord bot that turns a YouTube link into an Obsidian note. Runs on the services box,
infers on the Mac Mini, writes to a Samba share holding the vault.

## Goals

- **Decide, don't browse.** The primary output is a summary good enough to skip a video on.
- **Clickable timestamps.** Every key point links back into YouTube at the moment it was said.
- **Notes are durable and verifiable.** Every claim carries its provenance — which transcript,
  which model, which prompt version.
- **Nothing leaves the house.** No cloud APIs. Inference is local, storage is local.

## Non-goals

- Video/audio download (ADR 0001)
- Channel or playlist batch processing — one link at a time, deliberately
- A general-purpose bot framework (ADR 0005)
- A database. The note file is the record (ADR 0003)

## Components

### The bot

Single Go binary, single container, CPU-only. Depends on yt-dlp and deno, both pinned in the
image. No GPU passthrough is needed or wanted — see ADR 0002.

Responsibilities: authenticate to Discord, acknowledge within 3 seconds, enqueue, and post the
result in-channel when the job finishes. It holds no summarization logic of its own and no
persistent state.

### The queue

In-memory, bounded, with graceful shutdown (ADR 0003). Two workers by default — the Mac Mini is
shared, and oversubscribing its Ollama makes it unresponsive for other work.

On boot, log any job that was in flight when the process exited. That log line is the entire
crash-recovery story, and it is worth the two lines it costs.

**No database.** Idempotency is a file-existence check on a path derived deterministically
from the video ID. Provenance lives in note frontmatter. Status history is `ls`.

### The transcript

yt-dlp with `--skip-download --write-auto-subs --write-subs`, output `.vtt`, parsed into
`[]Cue{Start time.Duration, Text string}`. Manual captions are preferred over auto when both
exist; the origin is recorded either way.

whisper-receiver is the fallback when no caption track exists in any language, reached at
`WHISPER_URL`.

### The summarizer

Hierarchical map-reduce over timestamped cues, structurally the same approach as
lecture-digest's [ADR 0004](../lecture-digest/docs/adr/0004-summarization.md), adapted for
short-form video:

1. **Chunk** on cue boundaries, target ~2500 tokens, never mid-cue, with 10% overlap. There
   are no slide changes or speaker turns to key on, so token packing with a cue-aligned
   boundary is the honest cut.
2. **Map** each chunk to a fixed schema: topic, summary, key_points (each with a verbatim
   quote and a timestamp), definitions, concepts.
3. **Reduce in rounds** — 6 chunks to 2, then 2 to 1 — deduplicating key points by meaning
   and unioning timestamps. A single flat merge overflows context the same way it does for
   lectures.
4. **Render** the final note fresh from the reduced summaries, in the video's own order.

Timestamps survive every stage because cues carry them from the start. That is the main
inheritance from the caption decision: a transcript without timestamps could not produce
clickable links at all, and would be a strictly worse product than the old whisper path was.

**Currently stubbed.** The first implementation ships the full Discord → queue → Ollama →
vault path with a summarizer that returns a placeholder, so the plumbing is exercised against
real services before any prompt tuning.

### The note writer

Write to a hidden temp file in the destination directory, `fsync`, `rename` with retry, then
verify by stat (ADR 0004). This is because the vault is a Samba share Colby already mounts on
two other machines — the bot is one more writer on a share with existing semantics, so this is
ordinary hygiene rather than a novel hazard.

### The note format

Reuses `lecture-digest/docs/OUTPUT-FORMAT.md` — abstract callout, timestamped key points,
verbatim quotes, full transcript in a collapsed section. Not reinvented. Only the timestamp
link format differs (YouTube URLs instead of local media paths).

Frontmatter records the provenance that makes a summary checkable:

```yaml
---
source: youtube
video_id: aircAruvnKk
url: https://www.youtube.com/watch?v=aircAruvnKk
channel: 3Blue1Brown
duration: 1120
transcript_source: auto-captions    # auto-captions | manual-captions | whisper-receiver:medium
model: qwen2.5:7b
prompt_version: v1
generated: 2026-09-29T14:22:03Z
---
```

`transcript_source` is the load-bearing field. Auto-captions of music or crosstalk are garbage,
and without this stamp a summary built on garbage is indistinguishable from one built on a
manual transcript.

## Data flow

```
1. User: /summarize <url>
2. Bot:   parse video_id, derive note path, check existence
         └─ exists → reply with that path, done
3. Bot:   reply "working on it — <title>" immediately      (< 3s, always)
4. Worker: yt-dlp → captions.vtt → []Cue
5. Worker: no captions? → whisper-receiver, if configured
6. Worker: chunk → map → reduce → summary
7. Worker: render note → write .tmp → fsync → rename → verify
8. Bot:   post follow-up in-channel with the note path and a one-line gist
```

Failures at any step report in-channel and write **no** note. A `needs_review` database state
is not needed — the channel message reaches the only user faster than a database row would
(ADR 0003).

## Failure modes

| Failure | Behaviour |
|---|---|
| Extraction failed (yt-dlp breakage) | Reply "extraction failed", no note. Rebuild the image. |
| No captions, no whisper configured | Reply explaining, no note |
| Ollama unreachable | Distinct reply — a network partition, not a model error |
| Malformed LLM output | Report in-channel, write no note |
| Samba rename fails | Retry with backoff, then report (ADR 0004) |
| Duplicate link | Reply with the existing note path, do no work |
| Process dies mid-job | Logged on next boot; user re-pastes |

## Roadmap

1. Stub summarizer, full path working against real Discord, Ollama, and vault
2. Real map-reduce summarization with schema validation
3. Model benchmark: 2–3 candidates over a handful of real videos, on the Mac Mini
4. whisper fallback
5. `--force` re-summarize, prompt version bump
6. Second skill, when one exists
