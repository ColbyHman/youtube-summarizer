# Roadmap — yt-digest

## Now

**1. Plumbing with a stub summarizer.**
The full path working against real services: Discord command, in-memory queue, worker, yt-dlp
caption fetch, a stub summarizer that returns a fixed placeholder, atomic note write to the
vault, in-channel reply. Every real component except the LLM prompt.

Rationale: the caption round-trip is already verified and the Samba share is the most likely
source of surprise. Getting those exercised early — before prompt tuning makes changes
harder to attribute — is worth more than a working summary that arrives later.

Exit criteria: paste a link in Discord, get a note on the Mac Mini vault, visible in Obsidian,
with correct frontmatter including `transcript_source`.

**2. Real summarization.**
Hierarchical map-reduce per the architecture doc. Fixed JSON schema per chunk, rounds of
merge, deduplication of key points by meaning, timestamps preserved throughout.

Exit criteria: a 20-minute video produces a note with 5–10 key points, each with a verbatim
quote and a timestamp that lands on the right moment when clicked.

## Next

**3. Model benchmark.**
2–3 candidates over a handful of videos Colby actually wants to watch. Runs on the Mac Mini,
not here. Model is env config, so no rebuild. Note quality is the metric, not speed —
inference is on a dedicated box with Metal.

**4. whisper fallback.**
Only for videos with no caption track. Wire `WHISPER_URL` to whisper-receiver; it already
exists in the Go receiver's env. Small surface, and it only activates on the minority path.

**5. `--force` and prompt versioning.**
Re-summarize a video after a prompt improvement, stamping the new `prompt_version`. Only
worth building once prompts are actually being revised.

## Later

**6. Second skill.**
Not started, and deliberately not designed. The realistic candidates are things lecture-digest
already does. Whether ADR 0005's boundary holds is only testable against a second
implementation.

**7. Timestamp verification pass.**
Have the LLM re-check its own quotes against the transcript at the cited timestamp, and drop
or flag points that don't match. A summary that cites a timestamp where the quote doesn't
appear is worse than no timestamp, and this is the cheapest way to catch it.

## Revisit triggers

Specific conditions that would mean a decision above was wrong:

- **A second user, or batch/queue mode** (ADR 0003). The in-memory queue stops being enough
  and the database comes back.
- **Unattended processing** — links arriving while nobody's watching. Same trigger, and the
  reason it is written down: it is the one plausible future that would justify the state
  machine I removed.
- **Videos routinely needing whisper** (ADR 0001). If auto-captions prove too poor on real
  content, the latency argument that justified captions-over-whisper weakens considerably.
- **A second skill needing a different note shape** (ADR 0005). The seam is wrong, and
  cheaper to learn from one counterexample than to pre-build for.

## Explicitly not doing

- Audio download or local transcription on the happy path (ADR 0001)
- Channel/playlist batch processing
- A general-purpose bot framework (ADR 0005)
- Anything requiring a cloud API
