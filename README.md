# youtube-summarizer

A Discord bot for a home server. Paste a YouTube link, get an Obsidian note with an abstract,
timestamped key points, and the full transcript — so you can decide whether a video is worth
watching without watching it.

**Status: under construction.** The working implementation lives on the `feature/init` branch.
`main` is a placeholder so the repository isn't empty while you review.

## What it does

1. You run `/summarize <youtube link>` in Discord.
2. The bot pulls YouTube's own caption track — including auto-generated ones — via `yt-dlp`.
   **No audio is downloaded and nothing is transcribed.**
3. A local model on your Mac Mini summarizes the transcript, preserving per-cue timestamps.
4. The note is written atomically to your vault, and the path comes back in-channel.

Every key point links straight back into YouTube at the moment it was said, and every note
records which transcript, which model, and which prompt version produced it — so a summary
built on shaky auto-captions is visibly shaky.

## Design

Nothing leaves the house. Inference is local (Ollama), storage is local (your vault), and the
only external dependency is YouTube itself.

The decisions and their reasoning are in [`docs/adr/`](docs/adr/) on the `feature/init` branch.
The short version:

| Decision | Rationale |
|---|---|
| Captions over transcription | No audio download, no GPU, no wait. Whisper is a fallback for caption-less videos. |
| Inference on a separate host | The Mac Mini stays reserved for inference; the bot is a small CPU container with no GPU needs. |
| No database | The note file *is* the record. Idempotency is a file-existence check. |
| Atomic note writes | The vault is a network share; a note that lands empty is worse than one that never lands. |
| One skill, minimal interface | No speculative framework. A second skill should cost ~200 lines, not a rewrite. |
