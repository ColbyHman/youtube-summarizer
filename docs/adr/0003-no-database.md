# ADR 0003 — No database: the note file is the record

- Status: accepted
- Date: 2026-09-29
- Supersedes the original ADR 0003 (SQLite job queue), which was proposed and rejected on review

## Context

The first draft of this design carried SQLite as a job queue: a `jobs` table keyed on
`video_id`, with status, timestamps, note path, error, transcript source, model, and prompt
version. WAL mode, a bounded worker pool, restart recovery.

That was lecture-digest's pattern applied without checking whether it fit, and it doesn't.

lecture-digest and the actual-import pipeline need a database because they process *many*
items with history, deduplication, and full-text search over a growing corpus. yt-digest
handles **one link at a time, at human pace, with concurrency 2.** A durable job queue is a
lot of machinery for a workload where a dropped job costs one re-paste.

Worse, the design was carrying a redundancy. Every column in the `jobs` table — `video_id`,
`generated`, `model`, `prompt_version`, `transcript_source` — is already in the note's
frontmatter. The database was a second, weaker copy of information the output artifact
already holds authoritatively.

## Decision

**No database. The note file is the only persistent state.**

- **Job queue:** in-memory channel plus graceful shutdown. Enqueue, drain, reply.
- **Idempotency:** the note path is derived deterministically from the video ID, so
  re-pasting a link is a file-existence check. Exists → reply with that path, do no work.
- **Provenance:** frontmatter, which is where it belongs. The note is the record, and it is
  readable by the human who needs it.
- **Status history:** not tracked. `ls` in the vault plus the `generated` timestamp answers
  "did that work, and when".

## Consequences

Losing things, and what it costs:

- **A job in flight when the process dies is lost.** The cost is one re-paste by a human who
  is present and watching for a reply. Mitigated, not eliminated: on boot, log any job that
  was mid-flight when the process exited, by video ID, so it is visible rather than silently
  dropped. That log line is the entire recovery mechanism, and it is worth the two lines.
- **Malformed LLM output is no longer parked** in a `needs_review` state. The bot reports the
  failure in-channel and writes no note. For a single-user home bot the user *is* the review
  process, and a channel message reaches them faster than a database row would.
- **No queue-position or failure history across restarts.** Not a real loss — there is no
  second user whose in-flight work might be disturbed by the answer.
- **No rate limiting or abuse protection.** A paste-spam would spawn duplicate concurrent
  jobs, throttled only by the worker pool. Irrelevant for a private home server.

Gained:

- **No schema, no migrations, no state to corrupt.** The most durable component is the one
  that cannot get into an inconsistent state.
- **The bot is stateless across restarts.** Deploys are free. `docker compose up -d` and
  nothing else.
- **Nothing to back up that isn't already in the vault.**
- **Testable without fixtures.** A test creates a temp directory and a fake HTTP server. There
  is no database to seed.

## What this costs that a database would have given

Honest accounting, since this is a real trade and not a free win:

A database would have given crash-recovery of in-flight work, and a durable audit trail of
attempts. Neither is load-bearing for a single-user bot that is watched in real time by the only
person who would file a complaint. If yt-digest ever gains a second user, or a batch mode that
accepts a queue of links unattended, **revisit this decision** — that is the trigger, and
ADR 0003 was the wrong answer for the current scope.

## Queueing multiple videos

The bot is single-user, which makes in-memory queueing unambiguously sufficient. Several links
pasted at once enqueue normally: two workers run, the rest wait, each job is independent and
contends with nothing, and results post in-channel as they finish. Order of completion is not
submission order, which is fine because notes are independent and frontmatter records real
timestamps.

Two things bound it:

- **The channel is bounded** (10). Beyond that the bot replies "queue full" rather than
  accepting work that will not finish for hours. The bound protects the *Mac Mini*, which is
  shared: only 2 jobs ever run concurrently no matter how long the queue is, so a full queue
  never turns into an inference-host problem.
- **2 workers regardless of queue depth** (ADR 0002 — the inference box is reserved and
  oversubscribing it degrades everything else running there).

## Known accepted defect: duplicate concurrent jobs

Pasting the same video twice in quick succession enqueues it twice. The idempotency check is a
file-existence test on the note path, and the first job has not written that file yet, so both
jobs are accepted and both eventually write the same path. The result is a redundant
summarization cost and a note written twice — no corruption, since the writes are atomic and
the second overwrites the first with equivalent content.

**Accepted deliberately.** The fix is an in-memory `inflight` set of video IDs cleared on job
completion, roughly ten lines. Skipped because the bot is single-user, the cost of the defect is
a duplicated inference call rather than a wrong result, and the state is not worth carrying.
If this ever becomes annoying in practice, that set is the fix.

