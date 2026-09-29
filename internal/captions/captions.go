// Package captions fetches and parses YouTube caption tracks via yt-dlp.
//
// No audio is downloaded and no transcription is performed (ADR 0001). yt-dlp pulls YouTube's
// own caption track — including auto-generated ASR captions — as a .vtt file, which carries
// per-cue timestamps. Those timestamps are what make clickable links into the video possible,
// and they survive every downstream stage because they are attached to the text from the start.
package captions

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Cue is one caption line with its position in the video.
type Cue struct {
	Start time.Duration
	Text  string
}

// Track is a fetched transcript plus the metadata needed to render a note.
type Track struct {
	Cues    []Cue
	Title   string
	Channel string
	// Duration is the video length in seconds, 0 if unknown.
	Duration int
	// Source is the provenance: auto-captions, manual-captions, or whisper-receiver:<model>.
	// This is the load-bearing field in the note frontmatter (ADR 0001).
	Source string
	// Language of the fetched track.
	Language string
}

// Fetcher retrieves caption tracks by shelling out to yt-dlp.
type Fetcher struct {
	YTDLPPath string
	Timeout   time.Duration
	// Langs is the --sub-langs pattern. "en.*" prefers English and its regional variants
	// while still allowing an English track when that's all there is.
	Langs string
}

// NewFetcher returns a Fetcher with sensible defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{
		YTDLPPath: envOr("YTDLP_PATH", "yt-dlp"),
		Timeout:   3 * time.Minute,
		Langs:     envOr("SUB_LANGS", "en.*,en"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ErrNoCaptions is returned when a video has no caption track in any requested language. It is
// distinct from an extraction failure: the video is fine, there is just no text to summarize.
// The caller decides whether to fall back to whisper or to report it.
type ErrNoCaptions struct {
	VideoID string
}

func (e *ErrNoCaptions) Error() string {
	return fmt.Sprintf("no captions available for %s", e.VideoID)
}

// Fetch retrieves and parses the caption track for a video.
//
// Manual captions are preferred over auto-generated ones when both exist, because auto-captions
// of music, crosstalk, or heavy accents are frequently unusable — and a summary of garbage
// looks exactly as authoritative as a good one.
func (f *Fetcher) Fetch(ctx context.Context, videoID string) (*Track, error) {
	if videoID == "" {
		return nil, fmt.Errorf("empty video ID")
	}
	dir, err := os.MkdirTemp("", "ytdigest-captions-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	url := "https://www.youtube.com/watch?v=" + videoID
	outTmpl := filepath.Join(dir, "subs.%(ext)s")

	// --skip-download is the point of the whole design: no media, just text.
	args := []string{
		"--skip-download",
		"--write-subs",
		"--write-auto-subs",
		"--sub-langs", f.Langs,
		"--sub-format", "vtt",
		"--no-playlist",
		"--no-warnings",
		"--print", "TITLE\t%(title)s\nCHANNEL\t%(channel)s\nDURATION\t%(duration)s",
		"-o", outTmpl,
		url,
	}

	cmd := exec.CommandContext(ctx, f.YTDLPPath, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("yt-dlp timed out after %s: %w", f.Timeout, ctx.Err())
		}
		return nil, fmt.Errorf("extraction failed: %w: %s", err, lastLine(stderr.String()))
	}

	meta := parseMeta(stdout.String())

	track, err := f.bestTrackIn(dir)
	if err != nil {
		return nil, err
	}

	track.Title = meta["TITLE"]
	track.Channel = meta["CHANNEL"]
	track.Duration = atoiSafe(meta["DURATION"])

	if track.Title == "" {
		// The note is unusable without a title, and a video ID is a poor substitute. Fail
		// rather than write a note nobody can find later.
		return nil, fmt.Errorf("extraction produced no title for %s", videoID)
	}
	return track, nil
}

// bestTrackIn picks the best .vtt in dir: manual captions beat auto-generated ones, and an
// English track beats a translated one.
func (f *Fetcher) bestTrackIn(dir string) (*Track, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read caption dir: %w", err)
	}

	var auto, manual *Track
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".vtt") {
			continue
		}
		path := filepath.Join(dir, name)
		cues, lang, err := ParseVTTFile(path)
		if err != nil || len(cues) == 0 {
			continue
		}

		track := &Track{
			Cues:     cues,
			Source:   sourceFor(name),
			Language: lang,
		}
		// yt-dlp marks translated tracks with a "-orig" or non-source suffix; an "orig" track is
		// the uploader's own language, which is the better one.
		switch {
		case sourceFor(name) == "manual-captions" && !strings.Contains(name, "-orig"):
			manual = track
		case auto == nil:
			auto = track
		}
	}

	if manual != nil {
		return manual, nil
	}
	if auto != nil {
		return auto, nil
	}
	return nil, &ErrNoCaptions{}
}

func sourceFor(filename string) string {
	if strings.Contains(filename, "auto") {
		return "auto-captions"
	}
	return "manual-captions"
}

var (
	// 00:01:02.500 --> 00:01:05.000, or the short form 01:02.500 --> 01:05.000
	cueTimeRe = regexp.MustCompile(`(\d{1,2}:\d{2}:\d{2}\.\d{3}|\d{1,2}:\d{2}\.\d{3})\s*-->\s*(\d{1,2}:\d{2}:\d{2}\.\d{3}|\d{1,2}:\d{2}\.\d{3})`)
	// Inline cue settings such as "00:00:01.000 align:start position:0%"
	cueSettingsRe = regexp.MustCompile(`(?i)\b(align|line|position|size|region|vertical):`)
	// WEBVTT header and any NOTE / STYLE / REGION blocks
	headerRe = regexp.MustCompile(`^(WEBVTT|NOTE|STYLE|REGION)\b`)
	tagRe    = regexp.MustCompile(`<[^>]*>`)
	// YouTube auto-captions repeat each line twice as it scrolls; consecutive identical cues
	// are a rolling window, not two separate statements.
	entityRe = regexp.MustCompile(`&(amp|lt|gt|quot|apos|#39|nbsp);`)
)

// ParseVTTFile parses a WebVTT file into cues.
func ParseVTTFile(path string) ([]Cue, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read vtt: %w", err)
	}
	cues, lang := ParseVTT(string(b))
	return cues, lang, nil
}

// ParseVTT parses WebVTT text into deduplicated cues.
//
// The deduplication matters more than it looks. YouTube's auto-captions emit each phrase
// repeatedly as the caption scrolls, so a naive parse yields a transcript where every sentence
// appears two or three times — which inflates the token count, skews chunking, and makes the
// model summarize the same point three times.
func ParseVTT(s string) ([]Cue, string) {
	var (
		cues  []Cue
		lang  string
		start time.Duration
		hasTS bool
	)

	flush := func(text string) {
		text = cleanText(text)
		if !hasTS || text == "" {
			return
		}
		// Drop a cue identical to the one before it: a scroll artifact, not new content.
		if n := len(cues); n > 0 && cues[n-1].Text == text {
			return
		}
		cues = append(cues, Cue{Start: start, Text: text})
	}

	var buf []string
	inBlock := false

	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(strings.TrimSpace(line), "\r")
		if line == "" {
			// A blank line ends both a cue and any header/comment block. Forgetting to clear
			// inBlock here silently swallows the entire rest of the file.
			flush(strings.Join(buf, " "))
			buf = buf[:0]
			hasTS = false
			inBlock = false
			continue
		}
		if headerRe.MatchString(line) {
			// A NOTE block can span lines; skip until the blank line that ends it.
			inBlock = true
			continue
		}
		if inBlock {
			continue
		}

		if m := cueTimeRe.FindStringSubmatch(line); m != nil {
			flush(strings.Join(buf, " "))
			buf = buf[:0]
			hasTS = true
			start, _ = parseVTTTime(m[1])
			continue
		}

		// A bare "NOTE ..." line is a comment; anything before the first timestamp is a
		// header, not cue text.
		if !hasTS && len(cues) == 0 && !strings.Contains(line, "-->") {
			if lang == "" {
				lang = detectLang(line)
			}
			continue
		}
		if cueSettingsRe.MatchString(line) && !strings.Contains(line, " --> ") {
			continue
		}
		buf = append(buf, line)
	}
	flush(strings.Join(buf, " "))

	return cues, lang
}

// langInName matches the language code in a yt-dlp subtitle filename. Auto-caption tracks are
// named "subs.en.auto.vtt" — the ".auto" suffix is what sourceFor keys off, so it has to be
// tolerated here too.
var langInName = regexp.MustCompile(`\.([a-z]{2,3}(?:-[A-Za-z]{2,4})?)(?:\.auto)?\.vtt$`)

// detectLang pulls a language code out of a "Kind: en" style header line.
func detectLang(line string) string {
	if i := strings.Index(strings.ToLower(line), "kind:"); i >= 0 {
		rest := strings.TrimSpace(line[i+5:])
		return strings.Fields(rest)[0]
	}
	return ""
}

// LanguageFromFilename extracts the language code from a yt-dlp subtitle filename.
func LanguageFromFilename(name string) string {
	if m := langInName.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

func parseVTTTime(s string) (time.Duration, error) {
	var h, m, sec float64
	var err error

	parts := strings.SplitN(s, ":", 3)
	switch len(parts) {
	case 3:
		if _, err = fmt.Sscanf(parts[0], "%g", &h); err != nil {
			return 0, fmt.Errorf("parse hours: %w", err)
		}
		if _, err = fmt.Sscanf(parts[1], "%g", &m); err != nil {
			return 0, fmt.Errorf("parse minutes: %w", err)
		}
		if _, err = fmt.Sscanf(parts[2], "%g", &sec); err != nil {
			return 0, fmt.Errorf("parse seconds: %w", err)
		}
	case 2:
		if _, err = fmt.Sscanf(parts[0], "%g", &m); err != nil {
			return 0, fmt.Errorf("parse minutes: %w", err)
		}
		if _, err = fmt.Sscanf(parts[1], "%g", &sec); err != nil {
			return 0, fmt.Errorf("parse seconds: %w", err)
		}
	default:
		return 0, fmt.Errorf("unrecognised timestamp %q", s)
	}

	total := time.Duration(h*float64(time.Hour)) +
		time.Duration(m*float64(time.Minute)) +
		time.Duration(sec*float64(time.Second))
	return total, nil
}

// cleanText strips caption markup and collapses whitespace.
func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&apos;", "'", "&#39;", "'", "&nbsp;", " ",
	).Replace(s)
	// Auto-captions insert music and sound notes; they are noise in a summary.
	for _, tag := range []string{"[Music]", "[MUSIC]", "[Applause]", "[APPLAUSE]", "[Laughter]"} {
		s = strings.ReplaceAll(s, tag, "")
	}
	return strings.Join(strings.Fields(s), " ")
}

var metaRe = strings.NewReplacer("\r", "")

// parseMeta reads the TITLE/CHANNEL/DURATION lines yt-dlp was asked to print.
func parseMeta(out string) map[string]string {
	meta := map[string]string{}
	for _, line := range strings.Split(metaRe.Replace(out), "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"TITLE", "CHANNEL", "DURATION"} {
			if strings.HasPrefix(line, key+"\t") {
				meta[key] = strings.TrimSpace(strings.TrimPrefix(line, key+"\t"))
			}
		}
	}
	return meta
}

func atoiSafe(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return int(f)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// Seconds converts a cue start to whole seconds, for building YouTube timestamp links.
func (c Cue) Seconds() int { return int(c.Start.Seconds()) }

// TimestampLink builds a YouTube URL that jumps to this cue.
func (c Cue) TimestampLink(videoID string) string {
	return fmt.Sprintf("https://www.youtube.com/watch?v=%s&t=%ds", videoID, c.Seconds())
}

// TotalDuration reports the span covered by the cues, which is more reliable than the metadata
// duration when captions are sparse at the end of a video.
func TotalDuration(cues []Cue) time.Duration {
	if len(cues) == 0 {
		return 0
	}
	return cues[len(cues)-1].Start
}
