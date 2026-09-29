package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteCreatesNote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	content := []byte("---\nvideo_id: aircAruvnKk\n---\n\n# Hello\n")

	if err := Write(path, content); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "note.md"), []byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("got %d files, want 1 (just the note)", len(entries))
	}
}

func TestWriteOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	if err := Write(path, []byte("first, longer content")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(path, []byte("second")); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Video Notes", "2026-09-29-video.md")
	if err := Write(path, []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !Exists(path) {
		t.Error("note missing after Write created parent dirs")
	}
}

func TestWriteFailsOnEmptyContent(t *testing.T) {
	// A zero-byte note is the failure this whole package exists to prevent. Writing empty
	// content must be an error, not a silent empty file.
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.md")
	if err := Write(path, nil); err == nil {
		t.Error("Write(nil) succeeded, want error")
	}
}

func TestSweepStaleTemps(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, tempPrefix+"fresh.md.tmp")
	stale := filepath.Join(dir, tempPrefix+"stale.md.tmp")
	keep := filepath.Join(dir, "real.md")
	for _, p := range []string{fresh, stale, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the stale one past the sweep threshold.
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepStaleTemps(dir)
	if err != nil {
		t.Fatalf("SweepStaleTemps: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp still present")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temp was swept, should have been left alone")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("real note was swept")
	}
}

func TestSweepMissingDirIsNotAnError(t *testing.T) {
	removed, err := SweepStaleTemps(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("err = %v, want nil for missing dir", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"But what is a neural network?", "but-what-is-a-neural-network"},
		{"Deep Learning Chapter 1", "deep-learning-chapter-1"},
		{"  spaced  out  ", "spaced-out"},
		{"!!!", ""},
		{"", ""},
		{"C++ vs Rust: a comparison", "c-vs-rust-a-comparison"},
		{"日本語のタイトル", "日本語のタイトル"},
	}
	for _, c := range cases {
		if got := Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugTruncatesLongTitles(t *testing.T) {
	long := strings.Repeat("word ", 40)
	got := Slug(long)
	if len(got) > 60 {
		t.Errorf("slug length = %d, want <= 60", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("slug %q ends with a dash after truncation", got)
	}
}

func TestNotePathIsDeterministic(t *testing.T) {
	a := NotePath("/vault", "Video Notes", "abc123", "Some Video")
	b := NotePath("/vault", "Video Notes", "abc123", "Some Video")
	if a != b {
		t.Errorf("paths differ:\n %s\n %s", a, b)
	}
	if !strings.Contains(a, "abc123") && !strings.Contains(a, "some-video") {
		t.Errorf("path %q lacks both id and slug", a)
	}
}

func TestNotePathFallsBackToID(t *testing.T) {
	// A title that slugs to empty (punctuation only) must still yield a usable path.
	got := NotePath("/vault", "Video Notes", "abc123", "!!!")
	if !strings.HasSuffix(got, "abc123.md") {
		t.Errorf("path = %q, want it to fall back to the video ID", got)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "n.md")
	if Exists(path) {
		t.Error("Exists = true for missing file")
	}
	os.WriteFile(path, []byte("x"), 0o644)
	if !Exists(path) {
		t.Error("Exists = false for written file")
	}
	// A zero-byte file exists but is not a usable note.
	empty := filepath.Join(dir, "empty.md")
	os.WriteFile(empty, nil, 0o644)
	if Exists(empty) {
		t.Error("Exists = true for zero-byte file")
	}
	if Exists(dir) {
		t.Error("Exists = true for a directory")
	}
}

func TestFrontmatterQuotesRiskyValues(t *testing.T) {
	fm := Frontmatter(map[string]string{
		"source":            "youtube",
		"video_id":          "aircAruvnKk",
		"title":             "Neural networks: a deep dive #1",
		"transcript_source": "auto-captions",
	})
	// A colon-space in an unquoted YAML scalar would parse as a nested map.
	if !strings.Contains(fm, `title: "Neural networks: a deep dive #1"`) {
		t.Errorf("risky value not quoted:\n%s", fm)
	}
	if !strings.HasPrefix(fm, "---\n") || !strings.HasSuffix(fm, "---\n") {
		t.Errorf("frontmatter not delimited:\n%s", fm)
	}
}

func TestFrontmatterOmitsEmptyValues(t *testing.T) {
	fm := Frontmatter(map[string]string{"source": "youtube", "model": "", "channel": "3Blue1Brown"})
	if strings.Contains(fm, "model:") {
		t.Errorf("empty value emitted:\n%s", fm)
	}
	if !strings.Contains(fm, "channel: 3Blue1Brown") {
		t.Errorf("non-empty value dropped:\n%s", fm)
	}
}
