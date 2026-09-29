# ADR 0005 — One skill now, a skill interface for later

- Status: proposed
- Date: 2026-09-29

## Context

Colby's framing when he asked for this: *"I could either go with a bespoke bot for this use
case, or a more generic bot that can be reused."* Both were on the table, and the temptation
is to build the generic one now.

The generic option loses. A command router needs a command registry, a dispatcher,
per-command argument parsing, per-command permission handling, and help text — speculative
generality for capabilities that don't exist yet, and all of it maintained forever. The
failure mode is a framework with exactly one implementation, where the abstraction is pure
cost.

The lesson from ADR 0003 applies directly: **the database was lecture-digest's pattern applied
without checking whether it fit.** The same failure has a shape here, and the check is the
same one — what does the second implementation actually need that the first doesn't?

The reuse question is still real. Home bots that write Obsidian notes share an architecture:
the interesting part is not the queue plumbing or the Discord wiring, it's the per-skill
domain logic. lecture-digest already solves note rendering; whisper-receiver already solves
the fallback path.

## Decision

**Build the one skill. Structure the shell so that adding a second is ~200 lines, not a
rewrite. Do not build a router until a second skill exists.**

```
cmd/yt-digest/main.go
internal/discord     gateway, slash-command dispatch, permission check
internal/queue       in-memory bounded queue, graceful shutdown   (ADR 0003)
internal/llm         OpenAI-compatible client — one http.Post, no library
internal/notes       atomic write-then-rename, note frontmatter   (ADR 0004)
internal/skills      Skill interface + registry

skills/youtube/      the only implementation
  captions.go        yt-dlp invocation + vtt parsing
  summarize.go       chunk → map → reduce
  render.go          note rendering, reuses lecture-digest's OUTPUT-FORMAT.md
```

The interface, kept deliberately small:

```go
type Request struct {
    Arg     string   // whatever the user pasted or typed
    Channel string
    User    string
}

type Result struct {
    Reply    string   // in-channel text
    NotePath string   // "" if no note was written
    Source   string   // transcript provenance, stamped into the note
}

type Skill interface {
    Name() string
    Triggers() []string
    // Run must acknowledge within Discord's 3s deadline. Long work is
    // submitted to internal/queue and returns immediately.
    Run(ctx context.Context, req Request) (Result, error)
}
```

`Run` returning quickly is part of the contract, not an implementation detail — it is what
makes the 3-second deadline satisfiable without every skill author re-deriving why.

Note what is *not* here: no `Queue` interface, no `Store` interface, no job persistence
abstraction. ADR 0003 removed the database, and a `Store` interface over an in-memory channel
would be abstraction for its own sake.

## Reasoning

- **Reuse lives in `internal/`, not in the skill interface.** The note writer (ADR 0004) and
  the queue are what a second skill inherits. They are extracted *now* because this skill
  needs them, not because a future one might.
- **One interface, one implementation, is not speculative generality.** A single-method
  interface is the cheapest possible statement of a seam, and Go's implicit interfaces mean no
  registry ceremony and no init-order coupling to get wrong.
- **The trigger set is a string slice, not a DSL.** YAGNI. When two skills exist and their
  triggers actually collide, that is when routing earns its complexity.
- **The realistic second skills already have domain implementations elsewhere** —
  lecture-digest, therapy-journal, the actual-import classifier. Adapting one of those is the
  test of whether this boundary is in the right place.

## Consequences

- **The first skill costs ~450 lines**, down from ~600. The database was ~150 of it.
- **Adding a second skill later is cheap** *if* the boundary holds. If the second skill turns
  out to need a different note format, that's the signal the seam is wrong — and better to
  learn that from one counterexample than to pre-build for it.
- **The bot is stateless and the code is smaller.** No schema, no migrations, deploys are free.
- **A second user or a batch mode would change the queue story** (ADR 0003's revisit trigger).
  That is the signal the in-memory queue is no longer enough.
