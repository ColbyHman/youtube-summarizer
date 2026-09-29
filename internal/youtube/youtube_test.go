package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
)

// stubLLM returns canned JSON and records what it was asked, so the pipeline can be tested
// end to end without an inference host.
type stubLLM struct {
	mu       sync.Mutex
	calls    []string
	reply    func(call int, system, user string) string
	failOn   int // call number (1-based) to fail, 0 = never
	failWith error
}

func (s *stubLLM) CompleteJSON(ctx context.Context, system, user string, schema any, out any) error {
	s.mu.Lock()
	s.calls = append(s.calls, user)
	n := len(s.calls)
	reply := s.reply
	failOn, failWith := s.failOn, s.failWith
	s.mu.Unlock()

	if failOn == n {
		return failWith
	}

	var body string
	if reply != nil {
		body = reply(n, system, user)
	} else {
		body = `{"topic":"t","summary":"s","key_points":[],"concepts":[]}`
	}
	return json.Unmarshal([]byte(body), out)
}

func (s *stubLLM) ModelName() string { return "stub-model" }

func (s *stubLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// TestSummarizeEndToEnd drives the whole map-reduce with a stub, and asserts the transcript was
// actually fed to the model — the thing a stub could otherwise silently skip.
func TestSummarizeEndToEnd(t *testing.T) {
	var sawCueText bool
	llm := &stubLLM{reply: func(n int, system, user string) string {
		if strings.Contains(user, "[00:05]") {
			sawCueText = true
		}
		if strings.Contains(system, "merge") {
			return `{"abstract":"A short talk.","sections":[{"heading":"Intro","summary":"It opens.","start":0,"key_points":[]}],"key_points":[{"point":"A point","evidence_quote":"a quote","t":5}],"concepts":["x"],"gaps":"nothing"}`
		}
		return `{"topic":"Intro","summary":"The opening.","key_points":[{"point":"P1","evidence_quote":"q1","t":5}],"concepts":["c"]}`
	}}

	track := &captions.Track{
		Title: "Test Video", Channel: "Chan", Duration: 60,
		Source: "auto-captions",
		Cues: []captions.Cue{
			{Start: 0, Text: "welcome to the show"},
			{Start: 5 * time.Second, Text: "today we discuss neural networks"},
			{Start: 10 * time.Second, Text: "lets get started"},
		},
	}

	d, err := (&Summarizer{LLM: llm}).Summarize(context.Background(), track)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !sawCueText {
		t.Error("model was never shown the timestamped transcript")
	}
	if d.Abstract != "A short talk." {
		t.Errorf("abstract = %q", d.Abstract)
	}
	if len(d.KeyPoints) != 1 || d.KeyPoints[0].Point != "A point" {
		t.Errorf("key points = %+v", d.KeyPoints)
	}
	if len(d.Sections) != 1 || d.Sections[0].Heading != "Intro" {
		t.Errorf("sections = %+v", d.Sections)
	}
}

func TestSummarizePropagatesChunkFailure(t *testing.T) {
	llm := &stubLLM{failOn: 1, failWith: fmt.Errorf("model exploded")}
	track := &captions.Track{Title: "T", Cues: []captions.Cue{{Start: 0, Text: "text"}}}

	// Must fail rather than write a note with a silently-dropped section.
	if _, err := (&Summarizer{LLM: llm}).Summarize(context.Background(), track); err == nil {
		t.Fatal("want an error when the map stage fails")
	}
}

func TestSummarizeRejectsEmptyTranscript(t *testing.T) {
	llm := &stubLLM{}
	track := &captions.Track{Title: "T"}
	if _, err := (&Summarizer{LLM: llm}).Summarize(context.Background(), track); err == nil {
		t.Fatal("want an error on an empty transcript — an empty note is worse than none")
	}
}

func TestSummarizeReportsProgress(t *testing.T) {
	llm := &stubLLM{}
	// A long transcript that must chunk.
	var cues []captions.Cue
	for i := 0; i < 300; i++ {
		cues = append(cues, captions.Cue{
			Start: time.Duration(i) * 10 * time.Second,
			Text:  strings.Repeat("word ", 30),
		})
	}
	track := &captions.Track{Title: "Long", Cues: cues}

	var done, totalSeen int
	_, err := (&Summarizer{LLM: llm, Progress: func(d, t int) { done = d; totalSeen = t }}).
		Summarize(context.Background(), track)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if totalSeen < 2 {
		t.Fatalf("only %d chunks, expected the transcript to chunk", totalSeen)
	}
	if done != totalSeen {
		t.Errorf("final progress = %d, want %d", done, totalSeen)
	}
}

func TestSummarizeHonoursCancellation(t *testing.T) {
	cues := make([]captions.Cue, 500)
	for i := range cues {
		cues[i] = captions.Cue{Start: time.Duration(i) * 10 * time.Second, Text: strings.Repeat("w ", 50)}
	}
	track := &captions.Track{Title: "Long", Cues: cues}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	llm := &stubLLM{}
	if _, err := (&Summarizer{LLM: llm}).Summarize(ctx, track); err == nil {
		t.Fatal("want an error on a cancelled context")
	}
}

func TestChunkCuesSplitsLongTranscripts(t *testing.T) {
	var cues []captions.Cue
	for i := 0; i < 1000; i++ {
		cues = append(cues, captions.Cue{
			Start: time.Duration(i) * 3 * time.Second,
			Text:  strings.Repeat("a ", 40),
		})
	}
	chunks := ChunkCues(cues)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several for a long transcript", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Cues) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
		if c.Cues[0].Start > c.Last {
			t.Errorf("chunk %d has First after Last", i)
		}
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
	}
}

func TestChunkCuesKeepsShortTranscriptWhole(t *testing.T) {
	cues := []captions.Cue{
		{Start: 0, Text: "a short video"},
		{Start: 2 * time.Second, Text: "with two cues"},
	}
	chunks := ChunkCues(cues)
	if len(chunks) != 1 {
		t.Errorf("got %d chunks, want 1 for a short transcript", len(chunks))
	}
	if len(chunks[0].Cues) != 2 {
		t.Errorf("chunk has %d cues, want both", len(chunks[0].Cues))
	}
}

func TestChunkCuesEmpty(t *testing.T) {
	if got := ChunkCues(nil); got != nil {
		t.Errorf("ChunkCues(nil) = %v, want nil", got)
	}
}

func TestChunkCuesCoversEveryCue(t *testing.T) {
	// Overlap is intentional; every cue must still appear in at least one chunk or content
	// is silently lost at a boundary.
	var cues []captions.Cue
	for i := 0; i < 600; i++ {
		cues = append(cues, captions.Cue{
			Start: time.Duration(i) * 5 * time.Second,
			Text:  fmt.Sprintf("cue number %d", i),
		})
	}
	seen := map[string]bool{}
	for _, c := range ChunkCues(cues) {
		for _, cue := range c.Cues {
			seen[cue.Text] = true
		}
	}
	for _, cue := range cues {
		if !seen[cue.Text] {
			t.Errorf("cue %q lost across a chunk boundary", cue.Text)
		}
	}
}

func TestParseVideoID(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=aircAruvnKk":            "aircAruvnKk",
		"https://youtube.com/watch?v=aircAruvnKk":                "aircAruvnKk",
		"https://www.youtube.com/watch?v=aircAruvnKk&t=42s":      "aircAruvnKk",
		"https://www.youtube.com/watch?list=PL123&v=aircAruvnKk": "aircAruvnKk",
		"https://youtu.be/aircAruvnKk":                           "aircAruvnKk",
		"https://youtu.be/aircAruvnKk?t=10":                      "aircAruvnKk",
		"https://www.youtube.com/shorts/aircAruvnKk":             "aircAruvnKk",
		"https://www.youtube.com/embed/aircAruvnKk":              "aircAruvnKk",
		"aircAruvnKk":     "aircAruvnKk",
		"  aircAruvnKk  ": "aircAruvnKk",
		"watch me this https://youtu.be/aircAruvnKk please": "aircAruvnKk",
	}
	for in, want := range cases {
		got, ok := ParseVideoID(in)
		if !ok {
			t.Errorf("ParseVideoID(%q) not recognised", in)
			continue
		}
		if got != want {
			t.Errorf("ParseVideoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseVideoIDRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "   ", "not a url", "https://vimeo.com/12345", "hello"} {
		if got, ok := ParseVideoID(in); ok {
			t.Errorf("ParseVideoID(%q) = %q, want not recognised", in, got)
		}
	}
}

func TestRenderProducesNoteWithProvenance(t *testing.T) {
	dir := t.TempDir()
	track := &captions.Track{
		Title: "But what is a neural network?", Channel: "3Blue1Brown",
		Duration: 1120, Source: "auto-captions",
		Cues: []captions.Cue{
			{Start: 0, Text: "welcome"},
			{Start: 5 * time.Second, Text: "a neural network"},
		},
	}
	digest := &Digest{
		Title:    track.Title,
		Abstract: "An explanation of neural networks.",
		KeyPoints: []KeyPoint{
			{Point: "Neurons take weighted input", Quote: "a neural network", T: 5},
		},
		Sections: []Section{
			{Heading: "What is it", Summary: "It explains the basics.", Start: 0},
		},
		Concepts: []string{"neuron", "activation"},
	}

	path, err := Render(Config{VaultDir: dir, NoteDir: "Video Notes"},
		"aircAruvnKk", track, digest, "qwen2.5:7b")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	note := string(b)

	// Every provenance field must be present — this is what makes the summary checkable.
	for _, want := range []string{
		"transcript_source: auto-captions",
		"model: qwen2.5:7b",
		"prompt_version: " + PromptVersion,
		"video_id: aircAruvnKk",
		"channel: 3Blue1Brown",
		"generated:",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q", want)
		}
	}

	// The clickable timestamp is the core feature.
	if !strings.Contains(note, "https://www.youtube.com/watch?v=aircAruvnKk&t=5s") {
		t.Errorf("note missing the timestamp link:\n%s", note)
	}
	if !strings.Contains(note, "> [!abstract] Abstract") {
		t.Error("note missing the abstract callout")
	}
	if !strings.Contains(note, "## Transcript") {
		t.Error("note missing the transcript section")
	}
	if !strings.Contains(note, "a neural network") {
		t.Error("note missing the verbatim quote")
	}
}

func TestRenderQuotesRiskyTitle(t *testing.T) {
	dir := t.TempDir()
	track := &captions.Track{Title: "C++ vs Rust: a comparison #1", Source: "auto-captions",
		Cues: []captions.Cue{{Start: 0, Text: "hi"}}}
	digest := &Digest{Title: track.Title, Abstract: "a", Gaps: ""}

	path, err := Render(Config{VaultDir: dir, NoteDir: "n"}, "abcdefghijk", track, digest, "m")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, _ := os.ReadFile(path)
	// A colon-space in an unquoted YAML title would make the frontmatter invalid.
	if !strings.Contains(string(b), `title: "C++ vs Rust: a comparison #1"`) {
		t.Errorf("risky title not quoted:\n%s", firstLines(string(b), 12))
	}
}

func TestRenderOmitsGapsWhenComplete(t *testing.T) {
	dir := t.TempDir()
	track := &captions.Track{Title: "T", Source: "auto-captions",
		Cues: []captions.Cue{{Start: 0, Text: "x"}}}

	for _, gaps := range []string{"", "none", "Nothing", "coverage is complete"} {
		digest := &Digest{Title: "T", Abstract: "a", Gaps: gaps}
		path, err := Render(Config{VaultDir: dir, NoteDir: "n"}, "abcdefghijk", track, digest, "m")
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "Not covered") {
			t.Errorf("gaps %q produced a 'Not covered' callout, want it omitted", gaps)
		}
	}
}

func TestRenderIncludesGapsWhenReal(t *testing.T) {
	dir := t.TempDir()
	track := &captions.Track{Title: "T", Source: "auto-captions",
		Cues: []captions.Cue{{Start: 0, Text: "x"}}}
	digest := &Digest{Title: "T", Abstract: "a",
		Gaps: "The Q&A section from 40 minutes onward is not covered."}

	path, err := Render(Config{VaultDir: dir, NoteDir: "n"}, "abcdefghijk", track, digest, "m")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "Not covered") {
		t.Errorf("real gaps not surfaced:\n%s", firstLines(string(b), 30))
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int]string{
		0:    "",
		90:   "1:30",
		1120: "18:40",
		3725: "1:02:05",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWrap(t *testing.T) {
	lines := wrap("the quick brown fox jumps over the lazy dog", 16)
	for _, l := range lines {
		if len(l) > 16 {
			t.Errorf("line %q exceeds width 16", l)
		}
	}
	if got := strings.Join(lines, " "); got != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("wrap lost or reordered words: %q", got)
	}
	if wrap("   ", 10) != nil {
		t.Error("wrap of whitespace should be nil")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestEstimatedDuration(t *testing.T) {
	if got := EstimatedDuration(nil); got != 0 {
		t.Errorf("EstimatedDuration(nil) = %v, want 0", got)
	}
	track := &captions.Track{Duration: 600}
	if got := EstimatedDuration(track); got <= 0 {
		t.Errorf("EstimatedDuration = %v, want a positive estimate", got)
	}
}

func TestPathIsUnderVault(t *testing.T) {
	dir := t.TempDir()
	track := &captions.Track{Title: "Some Video", Source: "auto-captions",
		Cues: []captions.Cue{{Start: 0, Text: "x"}}}
	digest := &Digest{Title: "Some Video", Abstract: "a"}

	path, err := Render(Config{VaultDir: dir, NoteDir: "Video Notes"}, "abcdefghijk", track, digest, "m")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("note written outside the vault: %s", rel)
	}
	if !strings.Contains(path, "Video Notes") {
		t.Errorf("note not in the configured subdir: %s", path)
	}
}
