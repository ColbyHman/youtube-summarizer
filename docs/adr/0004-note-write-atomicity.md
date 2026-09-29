# ADR 0004 — Note output: write-then-rename over the Samba share

- Status: accepted
- Date: 2026-09-29

## Context

The vault is a **Samba share on the Mac Mini**, which Colby already mounts on a laptop and a
desktop. The bot writes into that same share from the services box.

This reframes the risk. The first draft of this ADR treated SMB as an exotic hazard the bot
was introducing into Colby's setup. It is not — it is the mechanism his vault already uses
every day, on two machines, through Obsidian. The bot is one more writer on a share with
existing, working semantics. That lowers the stakes considerably, and it means the right
response is ordinary hygiene rather than a redesign.

Two failure modes remain, both of which are real regardless of how many other writers exist.

**Torn reads.** Obsidian's filesystem watcher can fire while a file is still being written. A
note read at 60% of its length gets indexed as a 60%-length note, and depending on the
watcher may never be re-read. The fix is standard: write a temp file in the same directory,
then `rename()`. Rename is atomic, so a watcher sees either nothing or the complete file.

**Writes that lie about success.** `write()` returning nil means the bytes reached the kernel
socket buffer, not that they landed on the Mac Mini's disk. And `rename()` over SMB can fail
transiently under a stale oplock in a way that is indistinguishable from a real error.

## Decision

**Write to a hidden temp file in the destination directory, `fsync`, rename, then verify by
stat. Treat failed verification as a failed job, not a warning.**

1. `os.CreateTemp(noteDir, ".ytdigest-*.md.tmp")` — same directory, so the rename is
   intra-filesystem.
2. Write, then `f.Sync()` before close. Without the fsync the rename can be reordered ahead
   of the data on a network filesystem, producing a correctly-named **zero-length note** — the
   worst outcome available, because a missing note is obvious and an empty one is not.
3. `os.Rename(tmp, final)`, retrying a small number of times with a short backoff.
4. `os.Stat(final)`; confirm non-zero and matching size. A mismatch is a job failure with the
   path logged, because a truncated note is worse than no note.

The temp name is a dotfile, so a partial file is invisible to Obsidian's index even if the
rename never happens.

## Reasoning

- **Retry transient rename failures.** A single `EACCES` on a Samba share is usually a locking
  artifact. Treating the first one as final produces spurious failures that look like data
  loss and aren't. Retrying is correct; failing fast is not.
- **The temp file must be in the destination directory,** not `/tmp`. A cross-filesystem rename
  is not atomic, and would reintroduce exactly the problem this ADR exists to prevent.
- **Verify rather than trust.** The stat costs one network round trip. Irrelevant next to a
  multi-minute summarization, and it is the difference between "the note is there" and "the
  note is there *and is not empty*".
- **This is the same discipline any well-behaved writer to a shared filesystem uses.** It is
  not exotic, which is the point — the bot should not be the first program on that share to
  break atomicity for everyone else's watcher.

## Consequences

- **Obsidian never indexes a partial or empty note.** The entire point of the ADR.
- **Extra disk churn:** one full-size temp file per note, briefly. Irrelevant at markdown
  scale; would matter for media, which this bot does not write.
- **Leftover temp files** if the process dies between create and rename. Sweep
  `.ytdigest-*.md.tmp` older than an hour on boot — cheap, and it keeps the vault clean for a
  human who will definitely notice stray dotfiles.
- **Colby's existing mount setup is unchanged.** No sync service added, no second copy of the
  vault, nothing new to reason about on the laptop or desktop.
