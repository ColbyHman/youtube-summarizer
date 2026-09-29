// Package notes writes markdown notes to the vault.
//
// The vault is a Samba share (ADR 0004), so a note is written to a hidden temp file in the
// destination directory, fsynced, renamed into place, and then verified. Rename is atomic,
// so Obsidian's watcher sees either nothing or the complete file — never a half-written one.
package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// tempPrefix keeps partial files out of Obsidian's index even if the rename never happens.
	tempPrefix = ".ytdigest-"
	// staleTempAge is how long an orphaned temp file must sit before it is swept.
	staleTempAge = time.Hour
	// renameAttempts covers transient SMB oplock failures, which look like errors but are not.
	renameAttempts = 4
	// renameBackoff is the base delay between rename retries.
	renameBackoff = 150 * time.Millisecond
)

// Write atomically writes content to path, creating parent directories as needed.
//
// A file that exists but is empty is worse than a missing one: a missing note is obvious, an
// empty one reads as a video with nothing to say. So the rename is verified by stat, and a
// size mismatch is returned as an error rather than a warning.
func Write(path string, content []byte) error {
	// Reject empty content up front. Writing it and failing after the rename would still leave
	// the empty note on disk — the exact outcome this package exists to prevent.
	if len(content) == 0 {
		return fmt.Errorf("refusing to write empty note to %s", path)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create note dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, tempPrefix+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("create temp note: %w", err)
	}
	tmpName := tmp.Name()

	// Any failure past this point leaves a temp file behind; the boot sweep cleans it up.
	cleanup := func() { os.Remove(tmpName) }

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp note: %w", err)
	}
	// Without this, the rename can be reordered ahead of the data on a network filesystem and
	// produce a correctly-named zero-length note.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("fsync temp note: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp note: %w", err)
	}

	if err := renameWithRetry(tmpName, path); err != nil {
		cleanup()
		return err
	}

	if err := verify(path, len(content)); err != nil {
		return err
	}
	return nil
}

// renameWithRetry retries transient SMB failures. A single EACCES on a share is usually a
// stale oplock, not a real error; treating the first one as final produces spurious failures
// that look like data loss.
func renameWithRetry(from, to string) error {
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * renameBackoff)
	}
	return fmt.Errorf("rename note into place after %d attempts: %w", renameAttempts, err)
}

// verify confirms the note landed and is the size we wrote.
func verify(path string, want int) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("verify note %s: %w", path, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("verify note %s: landed empty, want %d bytes", path, want)
	}
	if want > 0 && st.Size() != int64(want) {
		return fmt.Errorf("verify note %s: got %d bytes, want %d", path, st.Size(), want)
	}
	return nil
}

// SweepStaleTemps removes orphaned temp files left by a process that died mid-write. Cheap,
// and it keeps the vault clean for a human who would notice stray dotfiles.
func SweepStaleTemps(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read note dir: %w", err)
	}

	cutoff := time.Now().Add(-staleTempAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed, nil
}

var unsafeChars = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// Slug converts a title into a filesystem-safe fragment. Unicode letters and digits survive,
// so non-English titles stay readable rather than collapsing to empty.
func Slug(title string) string {
	s := unsafeChars.ReplaceAllString(strings.TrimSpace(title), "-")
	s = strings.ToLower(strings.Trim(s, "-"))
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

// NotePath returns the deterministic path for a video's note, so idempotency is a
// file-existence check with no state to keep (ADR 0003).
func NotePath(vaultDir, subdir, videoID, title string) string {
	slug := Slug(title)
	if slug == "" {
		slug = videoID
	}
	return filepath.Join(vaultDir, subdir, time.Now().Format("2006-01-02")+"-"+slug+".md")
}

// Exists reports whether a note is already present.
func Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// frontmatterOrder fixes the sequence of known keys so notes diff cleanly. Anything not listed
// is still emitted, after these, in sorted order — a dropped field is a silent lie in a
// provenance record, and a typo'd key should be visible rather than vanish.
var frontmatterOrder = []string{
	"source", "video_id", "url", "title", "channel", "duration",
	"transcript_source", "model", "prompt_version", "generated",
}

// Frontmatter renders the YAML block that makes a summary checkable.
//
// transcript_source is the load-bearing field: auto-captions of music or crosstalk are
// garbage, and without this stamp a summary built on garbage is indistinguishable from one
// built on a manual transcript.
func Frontmatter(fm map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")

	written := make(map[string]bool, len(fm))
	write := func(k, v string) {
		if v == "" || written[k] {
			return
		}
		written[k] = true
		fmt.Fprintf(&b, "%s: %s\n", k, yamlScalar(v))
	}

	for _, k := range frontmatterOrder {
		write(k, fm[k])
	}
	extras := make([]string, 0, len(fm))
	for k := range fm {
		if !written[k] {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		write(k, fm[k])
	}

	b.WriteString("---\n")
	return b.String()
}

// yamlScalar quotes values that YAML would otherwise misparse. A colon-space would be read as
// a nested map, but a bare colon inside a word is fine — which matters because model tags like
// "qwen2.5:7b" are common and quoting them needlessly is noise.
func yamlScalar(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, "#{}[],&*!|>'\"%@`\\") ||
		strings.Contains(v, ": ") || strings.HasSuffix(v, ":") ||
		strings.Contains(v, " #") ||
		strings.TrimSpace(v) != v ||
		strings.ContainsAny(v[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
	}
	return v
}
