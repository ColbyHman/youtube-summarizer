package youtube

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/llm"
	"github.com/ColbyHman/yt-digest/internal/notes"
)

// Skill is the one capability this bot has (ADR 0005). Adding a second is a matter of
// implementing Run alongside it, not restructuring anything.
type Skill struct {
	Cfg     Config
	Fetcher *captions.Fetcher
	LLM     llm.Summarizer
	// Log is optional.
	Log func(msg string, kv ...any)
}

func (s *Skill) log(msg string, kv ...any) {
	if s.Log != nil {
		s.Log(msg, kv...)
	}
}

// The URL forms a user is likely to paste. All of them reduce to the same 11-character ID, and
// that ID is the job's identity — the same video pasted three ways is one video (ADR 0003).
var (
	watchRe  = regexp.MustCompile(`(?:youtube\.com/watch\?(?:.*&)?v=)([0-9A-Za-z_-]{11})`)
	shortRe  = regexp.MustCompile(`youtu\.be/([0-9A-Za-z_-]{11})`)
	shortsRe = regexp.MustCompile(`youtube\.com/shorts/([0-9A-Za-z_-]{11})`)
	embedRe  = regexp.MustCompile(`youtube\.com/embed/([0-9A-Za-z_-]{11})`)
	bareIDRe = regexp.MustCompile(`^\s*([0-9A-Za-z_-]{11})\s*$`)
)

// ParseVideoID extracts the video ID from whatever the user pasted. An empty result with no
// error means the input was not recognisably a video, which is a user error worth a friendly
// message rather than a stack trace.
func ParseVideoID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, re := range []*regexp.Regexp{watchRe, shortRe, shortsRe, embedRe, bareIDRe} {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// NotePathFor is the deterministic path a video's note will occupy, so idempotency is a
// file-existence check with no state to keep.
func (s *Skill) NotePathFor(videoID string) string {
	// The title isn't known until captions are fetched, so the pre-check uses the ID and the
	// real path is computed after. This is a best-effort "already done?" probe.
	return notes.NotePath(s.Cfg.VaultDir, s.Cfg.NoteDir, videoID, videoID)
}

// Run executes the full pipeline for one video.
//
// It reports progress through the reply callback and returns the path of the note it wrote.
// A failure at any stage leaves no note behind — an empty or partial note in the vault is worse
// than no note, because a missing one is obvious (ADR 0004).
func (s *Skill) Run(ctx context.Context, videoID, channel, user string, reply func(string)) (string, error) {
	track, err := s.Fetcher.Fetch(ctx, videoID)
	if err != nil {
		var noCaptions *captions.ErrNoCaptions
		if errors.As(err, &noCaptions) {
			// Whisper fallback is a later milestone. Reporting honestly beats a stub that
			// pretends to transcribe.
			return "", fmt.Errorf("no captions for this video, and the whisper fallback is not wired up yet: %w", err)
		}
		return "", err
	}

	s.log("captions fetched",
		"video_id", videoID, "cues", len(track.Cues), "source", track.Source)

	reply(fmt.Sprintf("📝 %s — %d cues (%s), summarizing…",
		track.Title, len(track.Cues), track.Source))

	sum := &Summarizer{
		LLM: s.LLM,
		Progress: func(done, total int) {
			if total > 1 {
				reply(fmt.Sprintf("⏳ section %d/%d", done, total))
			}
		},
	}

	digest, err := sum.Summarize(ctx, track)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}

	path, err := Render(s.Cfg, videoID, track, digest, s.LLM.ModelName())
	if err != nil {
		return "", fmt.Errorf("render: %w", err)
	}

	s.log("note written", "video_id", videoID, "path", path, "model", s.LLM.ModelName())
	return path, nil
}

// EstimatedDuration is a rough upper bound on how long a job will take, for the acknowledgement
// message. Deliberately vague: a wrong estimate is worse than none.
func EstimatedDuration(track *captions.Track) time.Duration {
	if track == nil {
		return 0
	}
	// Roughly 1s of local inference per 10s of video, on top of a fixed overhead.
	return time.Duration(track.Duration)/10*time.Second + 15*time.Second
}
