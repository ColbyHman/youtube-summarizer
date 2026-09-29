package youtube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/llm"
)

// fakeOllama stands in for the Mac Mini. It records the prompts it receives and replies with
// schema-shaped JSON, so the whole pipeline can be exercised without an inference host.
type fakeOllama struct {
	server   *httptest.Server
	prompts  []string
	failWith int
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		var user string
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		f.prompts = append(f.prompts, user)

		var reply string
		switch {
		case strings.Contains(user, "final digest"):
			reply = `{"abstract":"A talk about neural networks and what they actually do.",
				"sections":[{"heading":"What a neuron is","summary":"It breaks down the idea.","start":0,
				"key_points":[{"point":"A neuron sums weighted inputs","evidence_quote":"weighted inputs","t":5}]}],
				"key_points":[{"point":"Neurons take weighted input","evidence_quote":"weighted inputs","t":5}],
				"concepts":["neuron","activation function"],"gaps":"The Q&A is not covered."}`
		default:
			reply = `{"topic":"Intro","summary":"The opening section.",
				"key_points":[{"point":"P","evidence_quote":"q","t":1}],"concepts":["c"]}`
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": reply}},
			},
		})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOllama) client(t *testing.T) *llm.Client {
	t.Helper()
	return llm.New(f.server.URL+"/v1", "qwen2.5:7b", 10*time.Second)
}

// loadRealCues parses the actual caption file fetched during feasibility testing, so this
// integration test runs against real YouTube output rather than a tidy fixture.
func loadRealCues(t *testing.T) []captions.Cue {
	t.Helper()
	path := "/tmp/t.en.vtt"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no cached caption fixture at", path)
	}
	cues, _, err := captions.ParseVTTFile(path)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return cues
}

// TestIntegrationRealCaptionsToNote is the end-to-end proof: real caption file → real HTTP
// inference call → real atomic note on disk.
func TestIntegrationRealCaptionsToNote(t *testing.T) {
	cues := loadRealCues(t)
	if len(cues) < 100 {
		t.Fatalf("fixture has only %d cues", len(cues))
	}

	fake := newFakeOllama(t)
	vault := t.TempDir()

	track := &captions.Track{
		Title:   "But what is a neural network? | Deep learning chapter 1",
		Channel: "3Blue1Brown",
		// Duration from the real metadata, not the last cue — the file is truncated.
		Duration: 1120,
		Source:   "auto-captions",
		Cues:     cues,
	}

	sum := &Summarizer{LLM: fake.client(t)}
	digest, err := sum.Summarize(context.Background(), track)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	path, err := Render(Config{VaultDir: vault, NoteDir: "Video Notes"},
		"aircAruvnKk", track, digest, "qwen2.5:7b")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The note exists, is non-empty, and is where we said it would be.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat note: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("note is empty — the exact failure ADR 0004 exists to prevent")
	}
	if !strings.HasPrefix(path, vault) {
		t.Errorf("note written outside the vault: %s", path)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	note := string(b)

	// Provenance must survive the whole pipeline.
	for _, want := range []string{
		"transcript_source: auto-captions",
		"model: qwen2.5:7b",
		"prompt_version: v1",
		"video_id: aircAruvnKk",
		"channel: 3Blue1Brown",
		"duration: 1120",
		"> [!abstract]",
		"## Key points",
		"## Transcript",
		"youtube.com/watch?v=aircAruvnKk&t=5s",
		"> [!warning] Not covered",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q", want)
		}
	}

	// The real transcript must be attached, so the summary can actually be checked.
	if !strings.Contains(note, cues[10].Text) {
		t.Error("transcript body missing from the note")
	}

	// No temp files left behind on the vault.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ytdigest-") {
			t.Errorf("temp file %s left on the vault", e.Name())
		}
	}

	t.Logf("note: %s (%d bytes, %d inference calls, %d cues)",
		path, st.Size(), len(fake.prompts), len(cues))
}

// TestIntegrationChunkedRealTranscript checks that a real 19-minute transcript is actually
// chunked, so the map stage doesn't blow past a local context window.
func TestIntegrationChunkedRealTranscript(t *testing.T) {
	cues := loadRealCues(t)
	chunks := ChunkCues(cues)
	if len(chunks) < 2 {
		t.Fatalf("a 19-minute transcript produced %d chunk(s); it must chunk", len(chunks))
	}

	totalCues := 0
	for i, c := range chunks {
		totalCues += len(c.Cues)
		chars := 0
		for _, cue := range c.Cues {
			chars += len(cue.Text)
		}
		estTokens := chars / CharsPerToken
		if estTokens > TargetChunkTokens*2 {
			t.Errorf("chunk %d is ~%d tokens, over twice the target", i, estTokens)
		}
	}
	// Every cue should be covered, allowing for the intentional overlap at boundaries.
	if totalCues < len(cues) {
		t.Errorf("chunks cover %d cues, transcript has %d — content lost", totalCues, len(cues))
	}
	t.Logf("%d cues → %d chunks", len(cues), len(chunks))
}

// TestIntegrationNoNoteOnInferenceFailure is the guarantee that matters most: a failure must
// leave the vault clean, because an empty or partial note is worse than none.
func TestIntegrationNoNoteOnInferenceFailure(t *testing.T) {
	cues := loadRealCues(t)
	vault := t.TempDir()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model exploded", http.StatusInternalServerError)
	}))
	defer dead.Close()

	track := &captions.Track{
		Title: "Failure Case", Channel: "C", Source: "auto-captions", Cues: cues,
	}

	sum := &Summarizer{LLM: llm.New(dead.URL+"/v1", "broken", 5*time.Second)}
	digest, err := sum.Summarize(context.Background(), track)
	if err == nil {
		t.Fatal("want an error when inference fails")
	}
	if digest != nil {
		t.Error("a digest was returned despite the failure")
	}

	// Nothing should have been written.
	var found []string
	filepath.Walk(vault, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("vault not clean after a failure: %v", found)
	}
}

// TestIntegrationRepeatedRunOverwritesCleanly covers the re-summarize path: two runs to the
// same video must leave exactly one note and no debris.
func TestIntegrationRepeatedRunOverwritesCleanly(t *testing.T) {
	cues := loadRealCues(t)
	if len(cues) > 60 {
		cues = cues[:60]
	}
	fake := newFakeOllama(t)
	vault := t.TempDir()

	track := &captions.Track{Title: "Repeat Me", Channel: "C", Source: "auto-captions", Cues: cues}
	sum := &Summarizer{LLM: fake.client(t)}

	var path string
	for i := 0; i < 2; i++ {
		d, err := sum.Summarize(context.Background(), track)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		p, err := Render(Config{VaultDir: vault, NoteDir: "Video Notes"},
			"repeatme1", track, d, "qwen2.5:7b")
		if err != nil {
			t.Fatalf("run %d render: %v", i, err)
		}
		if path != "" && p != path {
			t.Errorf("run %d wrote a different path: %s vs %s", i, p, path)
		}
		path = p
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	var notes, temps int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".ytdigest-") {
			temps++
		} else {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("got %d notes after two runs, want 1", notes)
	}
	if temps != 0 {
		t.Errorf("got %d temp files, want 0", temps)
	}
}
