# ADR 0001 — Transcript source: YouTube auto-captions by default

- Status: accepted
- Date: 2026-09-29
- Supersedes nothing

## Context

The original CLI tool Colby used downloaded audio and ran it through a local whisper model.
On an M-series Mac that meant: pull ~40 MB per video, wait for transcription, wait again for
summarization. The whole point of the tool is deciding whether a video is worth watching, so
the latency sat directly on top of the use case.

whisper-receiver (Python/FastAPI, `main` branch) is the existing transcription service, and
the Go port (`feature/convert-to-go`) fronts `ghcr.io/mutablelogic/go-whisper` with
`WHISPER_URL` already env-configurable at `main.go:24`.

YouTube, however, publishes a caption track for the large majority of spoken content, including
auto-generated ASR captions for videos with captions disabled by the uploader.

## Verified

In the sandbox (yt-dlp 2026.08.19, deno 2.9.7, video `aircAruvnKk`):

```
yt-dlp --skip-download --write-auto-subs --write-subs --sub-langs en -o 't.%(ext)s' <url>
→ t.en.vtt   27333 bytes   286 cues
```

Round-trip confirmed: the `.vtt` file is written, cue count is non-zero, and no media was
downloaded. Extractor metadata reports the track as `caps=asr` (auto-generated).

One warning appears and is benign for subtitles: yt-dlp wants an impersonation target for the
*media* extractor, which is not installed. It did not block the caption write.

## Decision

**Fetch captions with yt-dlp. Do not download audio, do not run whisper, unless there is no
caption track at all.**

The LLM consumes timestamped cues directly. Timestamp fidelity is preserved end to end, which
is what makes the clickable `youtube.com/watch?v=ID&t=Ns` links in the note work.

whisper-receiver remains wired as a **fallback** for the minority of videos with no caption
track in any language. It is not on the happy path.

## Consequences

- **The bot needs no GPU and no Metal.** It runs on the services machine as a plain CPU
  container; inference is a LAN call to the Mac Mini (see ADR 0002).
- **Latency collapses** from minutes to the length of one LLM call.
- **Auto-captions are frequently wrong.** Music, crosstalk, heavy accents, and technical terms
  produce garbage. Every note stamps `transcript_source:` with the actual origin
  (`auto-captions`, `manual-captions`, or `whisper-receiver:<model>`) so a shaky summary is
  visibly shaky. Same reasoning as lecture-digest stamping its whisper model.
- **yt-dlp breaks regularly.** YouTube changes extraction without notice. The image pins a
  version; the fix for a break is a rebuild. On extraction failure the bot replies
  "extraction failed" in-channel and writes **no** note. An empty note is worse than no note.
- **A JS runtime is now a hard dependency.** yt-dlp needs deno (or node) for YouTube
  extraction and silently degrades format coverage without it. It goes in the Dockerfile, not
  in a comment.
- **Signing the note with caption provenance is not optional.** A summary of a 20-minute
  video built on auto-captions with no provenance stamp is a liability: it will look as
  authoritative as one built on a manual transcript, and it is not.
