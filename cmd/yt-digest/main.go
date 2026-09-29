// Command yt-digest is a Discord bot that turns a YouTube link into an Obsidian note.
//
// Pipeline: yt-dlp pulls YouTube's own caption track (no audio download, ADR 0001) → a local
// model on the Mac Mini summarizes it over the LAN (ADR 0002) → the note is written atomically
// to the Samba-mounted vault (ADR 0004). No database: the note file is the record (ADR 0003).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/discord"
	"github.com/ColbyHman/yt-digest/internal/llm"
	"github.com/ColbyHman/yt-digest/internal/notes"
	"github.com/ColbyHman/yt-digest/internal/queue"
	"github.com/ColbyHman/yt-digest/internal/youtube"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		return fallback
	}
	return n
}

func run(log *slog.Logger) error {
	// --- configuration ---
	vaultDir := os.Getenv("VAULT_DIR")
	if vaultDir == "" {
		return errors.New("VAULT_DIR is required — the path to the mounted vault")
	}
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		return errors.New("DISCORD_TOKEN is required")
	}
	appID := os.Getenv("DISCORD_APP_ID")
	if appID == "" {
		return errors.New("DISCORD_APP_ID is required (Developer Portal → General Information)")
	}

	noteDir := env("NOTE_SUBDIR", "Video Notes")
	noteRoot := filepath.Join(vaultDir, noteDir)
	if err := os.MkdirAll(noteRoot, 0o755); err != nil {
		return fmt.Errorf("create note dir %s: %w", noteRoot, err)
	}
	if n, err := notes.SweepStaleTemps(noteRoot); err != nil {
		log.Warn("sweep stale temp files", "err", err)
	} else if n > 0 {
		log.Info("swept stale temp files", "count", n)
	}

	// --- components ---
	llmClient := llm.New(
		env("OLLAMA_BASE_URL", "http://host.docker.internal:11434/v1"),
		env("OLLAMA_MODEL", "qwen2.5:7b"),
		envDuration("OLLAMA_TIMEOUT", 10*time.Minute),
	)

	skill := &youtube.Skill{
		Cfg:     youtube.Config{VaultDir: vaultDir, NoteDir: noteDir},
		Fetcher: captions.NewFetcher(),
		LLM:     llmClient,
		Log:     log.Info,
	}

	// A channel ID can arrive before the gateway finishes connecting, so replies are routed
	// through a small indirection that resolves lazily.
	var bot *discord.Bot
	replyTo := func(channelID, msg string) {
		if bot == nil {
			log.Warn("dropping reply, gateway not connected yet", "channel", channelID)
			return
		}
		bot.Reply(channelID, msg)
	}

	// --- queue ---
	workers := envInt("MAX_CONCURRENT_JOBS", 2)
	depth := envInt("QUEUE_DEPTH", 10)

	q := queue.New(depth, workers, func(ctx context.Context, job queue.Job) error {
		return handleJob(ctx, log, skill, job)
	}, log)

	// --- gateway ---
	var err error
	bot, err = discord.New(discord.Options{
		Token:      token,
		AppID:      appID,
		GuildID:    os.Getenv("DISCORD_GUILD_ID"),
		Log:        log,
		QueueDepth: depth,
		Workers:    workers,
		OnVideo: func(arg, channelID, userID string) error {
			videoID, ok := youtube.ParseVideoID(arg)
			if !ok {
				return errors.New("that doesn't look like a YouTube link or video ID")
			}
			return q.Submit(queue.Job{
				VideoID: videoID,
				RawArg:  arg,
				Channel: channelID,
				User:    userID,
				Reply:   func(msg string) { replyTo(channelID, msg) },
			})
		},
	})
	if err != nil {
		return err
	}

	log.Info("starting",
		"vault", vaultDir,
		"note_dir", noteDir,
		"model", llmClient.ModelName(),
		"workers", workers,
		"queue_depth", depth)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- bot.Open(ctx) }()

	select {
	case err := <-errCh:
		q.Close(30 * time.Second)
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining queue",
			"pending", q.Pending())
		q.Close(envDuration("DRAIN_TIMEOUT", 5*time.Minute))
		return nil
	}
}

// handleJob runs the skill for one video and reports the outcome in-channel.
func handleJob(ctx context.Context, log *slog.Logger, skill *youtube.Skill,
	job queue.Job) error {

	path, err := skill.Run(ctx, job.VideoID, job.Channel, job.User, job.Reply)
	if err != nil {
		return err
	}

	// The note path is reported verbatim: it is what the user needs to open the note, and a
	// shortened or reworded path is worse than the real one.
	job.Reply(fmt.Sprintf("✅ Saved `%s`\n`%s`", filepath.Base(path), path))
	return nil
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
