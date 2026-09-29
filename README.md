# yt-digest

A Discord bot for the home server. Paste a YouTube link, get an Obsidian note with an
abstract, timestamped key points, and the full transcript.

Built for the actual use case: deciding whether a video from your feed is worth the watch. A
summary is a filter; a good one saves the click, a bad one costs you.

## How it works

```
Discord ──▶ yt-digest (Go, CPU-only, on the services box)
              │
              ├─ yt-dlp (in-container) ──▶ captions.vtt ──▶ parse to [{start, text}]
              │                                                  │
              │                                    (fallback) whisper-receiver ──┐
              ├─ http://<mac-mini>:11434/v1/chat/completions  ◀─────────────────────┘
              │     chunk → map → reduce
              └─ //<mac-mini>/vault  (Samba share — the same one your laptop mounts)
                    write .tmp → fsync → rename → verify   (ADR 0004)
```

The Mac Mini runs inference and serves the vault. The bot itself is a small CPU container
elsewhere and has no GPU dependency at all (ADR 0002).

**Transcripts come from YouTube's own captions, not from transcription.** yt-dlp pulls the
`en` auto-caption track (`caps=asr`) as a `.vtt` with no audio download — verified at 27 KB /
286 cues on a real video (ADR 0001). whisper-receiver is wired as a fallback for the minority
of videos with no caption track, and is not on the happy path.

**No database.** The note file is the record. Idempotency is a file-existence check,
provenance is frontmatter, status is `ls` (ADR 0003).

## Status

- [x] Feasibility verified — captions round-trip, timestamped, no media download
- [x] Architecture + 5 ADRs written
- [x] Full path built: Discord → queue → captions → Ollama → vault, ~2,260 LOC
- [x] Map-reduce summarization over cues (hierarchical, timestamp-preserving)
- [x] 93 tests green (`-race`), incl. an end-to-end test on the real 286-cue caption file
- [ ] Model benchmark (2–3 candidates on real videos) — **needs your Ollama**
- [ ] Live run against real Discord + Mac Mini — **needs your token and vault mount**
- [ ] whisper fallback (for caption-less videos)
- [ ] Second skill, when one exists

### Verified end to end (sandbox)

Real caption file → real HTTP inference call against a stub Ollama → real atomic note on disk:
**22,948-byte note, 286 cues, 3 chunks, 5 inference calls, all provenance fields intact, no
temp files left on the vault.** A failing inference host leaves the vault verifiably clean.

Not yet exercised: the live Discord gateway and the real Mac Mini. Everything up to the HTTP
call is covered.

## Layout

```
cmd/yt-digest/main.go     wiring, config, shutdown
cmd/notecheck/            renders one note from a fixture — for eyeballing the format
internal/discord          gateway, dispatch, permissions
internal/queue            in-memory bounded queue, graceful shutdown
internal/captions         yt-dlp invocation + VTT parsing
internal/llm              OpenAI-compatible client (no library)
internal/notes            atomic write, frontmatter, slug
internal/youtube          chunking, map-reduce, note rendering
docs/adr/                 the decisions
```

## Setup

```
cp .env.example .env    # fill in DISCORD_TOKEN, DISCORD_APP_ID, VAULT_HOST_PATH
docker compose up -d
docker compose logs -f
```

Set `DISCORD_GUILD_ID` in `.env` and the `/summarize` command registers instantly; global
registration can take up to an hour to propagate.

## Configuration

All env vars. Model choice is configuration, not code, so it can be benchmarked without a
rebuild (ADR 0002).

| Var | Default | Notes |
|---|---|---|
| `DISCORD_TOKEN` | — | required |
| `DISCORD_APP_ID` | — | required, Developer Portal → General Information |
| `VAULT_DIR` | — | required, the mounted vault (compose sets this) |
| `DISCORD_GUILD_ID` | — | set it for instant command registration |
| `NOTE_SUBDIR` | `Video Notes` | created on boot if absent |
| `OLLAMA_BASE_URL` | `http://host.docker.internal:11434/v1` | Mac Mini over LAN |
| `OLLAMA_MODEL` | `qwen2.5:7b` | benchmark pending |
| `WHISPER_URL` | — | fallback only, unset means captions-only |
| `MAX_CONCURRENT_JOBS` | `2` | the Mac Mini is shared (ADR 0002) |
| `QUEUE_DEPTH` | `10` | bounds pending work, not concurrency |
| `YTDLP_PATH` | `yt-dlp` | pinned in the image |
| `LOG_LEVEL` | `info` | `debug` shows chunk progress |

## Development

```
go test ./... -race
go run ./cmd/notecheck      # render a note from the cached caption fixture
```

`cmd/notecheck` renders against a stub inference server and prints the note head, so the
output format can be reviewed without a model or a Discord token. It reads `/tmp/t.en.vtt`
if present.

## Risks

- **Auto-captions are often wrong.** Music, crosstalk, accents, jargon. Every note stamps
  `transcript_source:` so a shaky summary is visibly shaky. Never write a note from captions
  without it.
- **yt-dlp breaks.** YouTube changes extraction regularly. The image pins a version; the fix is
  a rebuild. On extraction failure the bot replies in-channel and writes *no* note.
- **Samba writes can lie about success.** See ADR 0004. A note that exists but is empty is
  worse than a missing note, so every write is verified after the rename.
- **A JS runtime is a hard dependency.** Without deno, yt-dlp silently loses format coverage.

## Decisions

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-captions-over-whisper.md) | Captions over whisper; whisper is fallback only |
| [0002](docs/adr/0002-topology.md) | Bot on the services box, inference on the Mac Mini over LAN |
| [0003](docs/adr/0003-no-database.md) | No database — the note file is the record |
| [0004](docs/adr/0004-note-write-atomicity.md) | Write-tmp-fsync-rename-verify over the Samba share |
| [0005](docs/adr/0005-skill-boundary.md) | One skill now, a minimal interface for later — no router |
