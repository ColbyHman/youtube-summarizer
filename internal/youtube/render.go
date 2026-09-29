package youtube

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/notes"
)

// Config holds what the skill needs to write a note.
type Config struct {
	VaultDir string
	NoteDir  string
}

// Render writes the note for a track and its digest, returning the path written.
//
// The note format follows lecture-digest's OUTPUT-FORMAT.md — abstract callout, timestamped
// key points, verbatim quotes, full transcript in a collapsed section. Only the timestamp link
// format differs: YouTube URLs rather than local media paths.
func Render(cfg Config, videoID string, track *captions.Track, digest *Digest, model string) (string, error) {
	path := notes.NotePath(cfg.VaultDir, cfg.NoteDir, videoID, track.Title)
	content := renderNote(videoID, track, digest, model, path)
	if err := notes.Write(path, []byte(content)); err != nil {
		return "", fmt.Errorf("write note: %w", err)
	}
	return path, nil
}

func renderNote(videoID string, track *captions.Track, digest *Digest, model, path string) string {
	var b strings.Builder

	url := "https://www.youtube.com/watch?v=" + videoID

	fm := map[string]string{
		"source":            "youtube",
		"video_id":          videoID,
		"url":               url,
		"title":             track.Title,
		"channel":           track.Channel,
		"duration":          fmt.Sprint(track.Duration),
		"transcript_source": track.Source,
		"model":             model,
		"prompt_version":    PromptVersion,
		"generated":         time.Now().UTC().Format(time.RFC3339),
	}
	b.WriteString(notes.Frontmatter(fm))
	b.WriteString("\n")

	fmt.Fprintf(&b, "# %s\n\n", track.Title)

	if track.Channel != "" {
		fmt.Fprintf(&b, "**%s** · %s\n\n", track.Channel, humanDuration(track.Duration))
	}

	// The abstract is the point of the whole exercise: enough to decide whether to watch.
	if digest.Abstract != "" {
		b.WriteString("> [!abstract] Abstract\n")
		for _, line := range wrap(digest.Abstract, 76) {
			fmt.Fprintf(&b, "> %s\n", line)
		}
		b.WriteString("\n")
	}

	if len(digest.KeyPoints) > 0 {
		b.WriteString("## Key points\n\n")
		for _, kp := range digest.KeyPoints {
			b.WriteString(renderKeyPoint(kp, videoID))
		}
		b.WriteString("\n")
	}

	for _, sec := range digest.Sections {
		fmt.Fprintf(&b, "## %s\n\n", sec.Heading)
		for _, line := range wrap(sec.Summary, 76) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		if len(sec.Points) > 0 {
			for _, kp := range sec.Points {
				b.WriteString(renderKeyPoint(kp, videoID))
			}
		}
		b.WriteString("\n")
	}

	// A summary that doesn't say what it left out reads as complete, so a genuine gaps note is
	// surfaced. The converse matters too: "nothing" or "complete" must not render a warning
	// callout, which would be noise on every well-covered video.
	if gaps := strings.ToLower(strings.TrimSpace(digest.Gaps)); gaps != "" &&
		!strings.Contains(gaps, "none") &&
		!strings.Contains(gaps, "nothing") &&
		!strings.Contains(gaps, "complete") &&
		!strings.Contains(gaps, "all ") {
		b.WriteString("> [!warning] Not covered\n> ")
		for i, line := range wrap(digest.Gaps, 72) {
			if i > 0 {
				b.WriteString("> ")
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(digest.Concepts) > 0 {
		fmt.Fprintf(&b, "**Concepts:** %s\n\n", strings.Join(digest.Concepts, ", "))
	}

	// The transcript is attached so the summary can be checked, and so the note is useful even
	// if the summary is wrong.
	b.WriteString("## Transcript\n\n")
	fmt.Fprintf(&b, "> [!info]- Full transcript (%s source, %d cues)\n",
		track.Source, len(track.Cues))
	for _, cue := range track.Cues {
		fmt.Fprintf(&b, "> **%s** %s\n", clock(cue.Start), cue.Text)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "---\n[Watch on YouTube](%s) · [Open note](%s)\n",
		url, filepath.Base(path))

	return b.String()
}

// renderKeyPoint renders one point with its timestamp link and verbatim quote. The quote is
// what makes the timestamp checkable — a claim without evidence is just an assertion.
func renderKeyPoint(kp KeyPoint, videoID string) string {
	var b strings.Builder
	link := fmt.Sprintf("https://www.youtube.com/watch?v=%s&t=%ds", videoID, int(kp.T))
	fmt.Fprintf(&b, "- **%s** ([%s](%s))\n", kp.Point, clock(time.Duration(kp.T)*time.Second), link)
	if kp.Quote != "" {
		fmt.Fprintf(&b, "  > %q\n", kp.Quote)
	}
	return b.String()
}

func humanDuration(sec int) string {
	if sec <= 0 {
		return ""
	}
	d := time.Duration(sec) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d", m, int(d.Seconds())%60)
}

// wrap breaks text into lines of at most width, on word boundaries.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	return append(lines, cur)
}

// Discard removes a note that failed verification, so a bad write doesn't linger as an empty
// or partial file in the vault.
func Discard(path string) {
	if path != "" {
		os.Remove(path)
	}
}
