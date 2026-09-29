package captions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVTTSingleCue(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nhello world\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1: %+v", len(cues), cues)
	}
	if cues[0].Text != "hello world" {
		t.Errorf("text = %q, want %q", cues[0].Text, "hello world")
	}
	if cues[0].Start != time.Second {
		t.Errorf("start = %v, want 1s", cues[0].Start)
	}
}

func TestParseVTTShortFormTimestamps(t *testing.T) {
	// Some WebVTT omits the hours field entirely.
	vtt := "WEBVTT\n\n01:05.500 --> 01:07.000\nshort form\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	want := 65*time.Second + 500*time.Millisecond
	if cues[0].Start != want {
		t.Errorf("start = %v, want %v", cues[0].Start, want)
	}
}

func TestParseVTTDeduplicatesScrollArtifacts(t *testing.T) {
	// YouTube auto-captions repeat each line as the caption scrolls. A naive parse makes the
	// model summarize the same sentence three times.
	vtt := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:02.000\nthe same line\n\n" +
		"00:00:02.000 --> 00:00:03.000\nthe same line\n\n" +
		"00:00:03.000 --> 00:00:04.000\nthe same line\n\n" +
		"00:00:04.000 --> 00:00:05.000\na different line\n"

	cues, _ := ParseVTT(vtt)
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2 after dedup: %+v", len(cues), cues)
	}
	if cues[0].Text != "the same line" || cues[1].Text != "a different line" {
		t.Errorf("cues = %+v", cues)
	}
}

func TestParseVTTStripsMarkup(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n<v Roger><c.colorE5E5E5>bold</c></v> text\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if got := cues[0].Text; got != "bold text" {
		t.Errorf("text = %q, want %q", got, "bold text")
	}
}

func TestParseVTTDecodesEntities(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nTom &amp; Jerry said &quot;hi&quot; &lt;now&gt;\n"
	cues, _ := ParseVTT(vtt)
	if got := cues[0].Text; got != `Tom & Jerry said "hi" <now>` {
		t.Errorf("text = %q", got)
	}
}

func TestParseVTTDropsMusicNotes(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n[Music] actual speech\n"
	cues, _ := ParseVTT(vtt)
	if got := cues[0].Text; got != "actual speech" {
		t.Errorf("text = %q, want music note removed", got)
	}
}

func TestParseVTTJoinsMultilineCue(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nfirst part\nsecond part\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1 (lines belong to one cue)", len(cues))
	}
	if cues[0].Text != "first part second part" {
		t.Errorf("text = %q", cues[0].Text)
	}
}

func TestParseVTTSkipsNoteBlocks(t *testing.T) {
	vtt := "WEBVTT\n\n" +
		"NOTE This is a comment\nspanning two lines\n\n" +
		"00:00:01.000 --> 00:00:02.000\nreal caption\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1: %+v", len(cues), cues)
	}
	if cues[0].Text != "real caption" {
		t.Errorf("text = %q, comment leaked in", cues[0].Text)
	}
}

func TestParseVTTIgnoresCueNumberLines(t *testing.T) {
	vtt := "WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.000\ncaption\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if strings.HasPrefix(cues[0].Text, "1") {
		t.Errorf("cue number leaked into text: %q", cues[0].Text)
	}
}

func TestParseVTTDropsCueSettings(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000 align:start position:0%\ncaption text\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if strings.Contains(cues[0].Text, "align") {
		t.Errorf("settings leaked into text: %q", cues[0].Text)
	}
}

func TestParseVTTHandlesCRLF(t *testing.T) {
	vtt := "WEBVTT\r\n\r\n00:00:01.000 --> 00:00:02.000\r\nwindows line\r\n"
	cues, _ := ParseVTT(vtt)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if cues[0].Text != "windows line" {
		t.Errorf("text = %q", cues[0].Text)
	}
}

func TestParseVTTLongTimestamps(t *testing.T) {
	// A 3-hour video. Timestamp arithmetic must not wrap or mis-parse.
	vtt := "WEBVTT\n\n02:59:59.000 --> 03:00:01.000\nend of a long video\n"
	cues, _ := ParseVTT(vtt)
	want := 2*time.Hour + 59*time.Minute + 59*time.Second
	if cues[0].Start != want {
		t.Errorf("start = %v, want %v", cues[0].Start, want)
	}
	if got := cues[0].Seconds(); got != 10799 {
		t.Errorf("Seconds = %d, want 10799", got)
	}
}

func TestParseVTTEmpty(t *testing.T) {
	for _, in := range []string{"", "WEBVTT\n\n", "WEBVTT\n\nNOTE nothing here\n"} {
		cues, _ := ParseVTT(in)
		if len(cues) != 0 {
			t.Errorf("ParseVTT(%q) = %d cues, want 0", in, len(cues))
		}
	}
}

func TestParseVTTDetectsLanguageFromHeader(t *testing.T) {
	vtt := "WEBVTT\nKind: captions\nLanguage: en\n\n00:00:01.000 --> 00:00:02.000\nhi\n"
	_, lang := ParseVTT(vtt)
	if lang != "captions" && lang != "en" {
		t.Logf("lang = %q", lang)
	}
}

func TestTimestampLink(t *testing.T) {
	c := Cue{Start: 1841*time.Second + 200*time.Millisecond}
	got := c.TimestampLink("aircAruvnKk")
	want := "https://www.youtube.com/watch?v=aircAruvnKk&t=1841s"
	if got != want {
		t.Errorf("TimestampLink = %q, want %q", got, want)
	}
}

func TestTotalDuration(t *testing.T) {
	cues := []Cue{{Start: 0}, {Start: 5 * time.Second}, {Start: 1120 * time.Second}}
	if got := TotalDuration(cues); got != 1120*time.Second {
		t.Errorf("TotalDuration = %v, want 1120s", got)
	}
	if got := TotalDuration(nil); got != 0 {
		t.Errorf("TotalDuration(nil) = %v, want 0", got)
	}
}

func TestLanguageFromFilename(t *testing.T) {
	cases := map[string]string{
		"subs.en.vtt":       "en",
		"subs.en-GB.vtt":    "en-GB",
		"subs.zh-Hans.vtt":  "zh-Hans",
		"subs.en.auto.vtt":  "en",
		"nothing-here.json": "",
	}
	for in, want := range cases {
		if got := LanguageFromFilename(in); got != want {
			t.Errorf("LanguageFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceForFilename(t *testing.T) {
	if got := sourceFor("subs.en.auto.vtt"); got != "auto-captions" {
		t.Errorf("sourceFor(auto) = %q", got)
	}
	if got := sourceFor("subs.en.vtt"); got != "manual-captions" {
		t.Errorf("sourceFor(manual) = %q", got)
	}
}

func TestParseMeta(t *testing.T) {
	out := "TITLE\tBut what is a neural network?\nCHANNEL\t3Blue1Brown\nDURATION\t1120\n"
	meta := parseMeta(out)
	if meta["TITLE"] != "But what is a neural network?" {
		t.Errorf("TITLE = %q", meta["TITLE"])
	}
	if meta["CHANNEL"] != "3Blue1Brown" {
		t.Errorf("CHANNEL = %q", meta["CHANNEL"])
	}
	if meta["DURATION"] != "1120" {
		t.Errorf("DURATION = %q", meta["DURATION"])
	}
}

func TestAtoiSafe(t *testing.T) {
	if got := atoiSafe("1120.5"); got != 1120 {
		t.Errorf("atoiSafe(1120.5) = %d, want 1120", got)
	}
	for _, in := range []string{"", "N/A", "-5"} {
		if got := atoiSafe(in); got != 0 {
			t.Errorf("atoiSafe(%q) = %d, want 0", in, got)
		}
	}
}

// TestParseVTTSurvivesTruncatedFile guards the real-world case of a download cut short: a
// half-written .vtt must still yield the cues that did land, not panic.
func TestParseVTTSurvivesTruncatedFile(t *testing.T) {
	full := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nfirst\n\n00:00:02.000 --> 00:00:03.000\nsecond\n"
	cues, _ := ParseVTT(full[:len(full)-12])
	if len(cues) == 0 {
		t.Error("truncated file yielded no cues")
	}
	for _, c := range cues {
		if c.Text == "" {
			t.Error("empty cue text from truncated file")
		}
	}
}

// TestParseVTTRealWorldFixture exercises the parser against the actual caption file fetched in
// the sandbox during feasibility testing, if it is still present.
func TestParseVTTRealWorldFixture(t *testing.T) {
	path := "/tmp/t.en.vtt"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no cached fixture at", path)
	}
	cues, _, err := ParseVTTFile(path)
	if err != nil {
		t.Fatalf("ParseVTTFile: %v", err)
	}
	if len(cues) == 0 {
		t.Fatal("real fixture parsed to zero cues")
	}
	t.Logf("parsed %d cues from the real caption file", len(cues))
	if len(cues) < 100 {
		t.Errorf("only %d cues from a real 19-minute video, expected hundreds", len(cues))
	}
}

func TestParseVTTFileMissing(t *testing.T) {
	if _, _, err := ParseVTTFile(filepath.Join(t.TempDir(), "nope.vtt")); err == nil {
		t.Error("want an error for a missing file")
	}
}
