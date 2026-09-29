// Package youtube implements the one skill: turn a YouTube link into an Obsidian note.
//
// The pipeline is fetch captions → chunk → map → reduce → render. The chunking strategy
// borrows from lecture-digest's map-reduce (ADR 0004 there), adapted for short-form video
// where there are no slide changes or speaker turns to cut on.
package youtube

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/llm"
)

// PromptVersion is stamped into every note's frontmatter. When the prompt improves, this is
// what tells you which notes were produced by which prompt — the same reasoning as
// lecture-digest's ADR 0005.
const PromptVersion = "v1"

// TargetChunkTokens is the aim for each map chunk. Comfortably inside a 4k-8k local context
// once the prompt and output schema are accounted for.
const TargetChunkTokens = 2500

// CharsPerToken is a rough English ratio. Chunking on characters rather than a tokenizer keeps
// the dependency list empty, and being approximate here is harmless: the boundary still lands
// between cues either way.
const CharsPerToken = 4

// chunkOverlapChars repeats the tail of the previous chunk so a sentence spanning a boundary
// isn't lost. Cheap insurance against a chunk starting mid-thought.
const chunkOverlapChars = 400

// Chunk is one unit of the map stage.
type Chunk struct {
	Index int
	Cues  []captions.Cue
	// First and Last are the cue timestamps bounding this chunk, kept so the merge step can
	// reason about coverage.
	First time.Duration
	Last  time.Duration
}

// ChunkSummary is the fixed schema every chunk is mapped to.
//
// Every field is a claim plus evidence plus a timestamp. That triple is the whole design: a
// summary you can check. An unevidenced summary of a video you half-watched is how you end up
// confidently skipping the one worth watching.
type ChunkSummary struct {
	Topic     string     `json:"topic"`
	Summary   string     `json:"summary"`
	KeyPoints []KeyPoint `json:"key_points"`
	Concepts  []string   `json:"concepts"`
	NextLooks string     `json:"next_looks_like,omitempty"`
}

type KeyPoint struct {
	Point string  `json:"point"`
	Quote string  `json:"evidence_quote"`
	T     float64 `json:"t"`
}

// Digest is the merged result, rendered into the note body.
type Digest struct {
	Title     string
	Abstract  string
	Sections  []Section
	KeyPoints []KeyPoint
	Concepts  []string
	Gaps      string
}

type Section struct {
	Heading string     `json:"heading"`
	Summary string     `json:"summary"`
	Start   float64    `json:"start"`
	Points  []KeyPoint `json:"key_points"`
}

// chunkSchema is sent to Ollama as a format constraint. Field names double as the model's
// instructions, so the json tags carry weight.
var chunkSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"topic":   map[string]any{"type": "string"},
		"summary": map[string]any{"type": "string"},
		"key_points": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"point":          map[string]any{"type": "string"},
					"evidence_quote": map[string]any{"type": "string"},
					"t":              map[string]any{"type": "number"},
				},
				"required": []string{"point", "evidence_quote", "t"},
			},
		},
		"concepts": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
		"next_looks_like": map[string]any{"type": "string"},
	},
	"required": []string{"topic", "summary", "key_points"},
}

const chunkSystemPrompt = `You summarise one section of a video transcript.

Rules:
- Every key point must be supported by a verbatim quote from the transcript. Do not paraphrase the evidence.
- "t" is the start time of the quote in SECONDS, taken from the cue markers in the transcript.
- Do not invent claims. If the section says nothing notable, return an empty key_points array.
- Faithful over complete: a summary that omits is better than one that embellishes.
- Write "summary" as 3-5 sentences covering what this section actually says, in its own order.`

const mergeSystemPrompt = `You merge several section summaries of one video into a single digest.

Rules:
- Preserve every distinct key point. If two sections make the same point, keep it once and merge both timestamps.
- Never invent a key point that is not in the input. Do not add facts.
- Keep timestamps from the input; do not estimate new ones.
- Order sections by their start time, following the video's actual structure.
- "gaps" names anything the input sections did not cover. If coverage is complete, say so plainly.`

var digestSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"abstract": map[string]any{"type": "string"},
		"sections": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"heading":    map[string]any{"type": "string"},
					"summary":    map[string]any{"type": "string"},
					"start":      map[string]any{"type": "number"},
					"key_points": map[string]any{"type": "array", "items": digestPointSchema()},
				},
				"required": []string{"heading", "summary", "start"},
			},
		},
		"key_points": map[string]any{
			"type":  "array",
			"items": digestPointSchema(),
		},
		"concepts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"gaps":     map[string]any{"type": "string"},
	},
	"required": []string{"abstract", "sections", "key_points"},
}

func digestPointSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"point":          map[string]any{"type": "string"},
			"evidence_quote": map[string]any{"type": "string"},
			"t":              map[string]any{"type": "number"},
		},
		"required": []string{"point", "evidence_quote", "t"},
	}
}

// Summarizer turns cues into a digest.
type Summarizer struct {
	LLM llm.Summarizer
	// Progress, if set, is called after each chunk so the caller can report intermediate state.
	Progress func(done, total int)
}

// Summarize runs map over the chunks, then reduce in rounds.
//
// A naive single reduce of N chunk summaries overflows context the same way the original
// transcript would — which is why this merges in rounds rather than all at once.
func (s *Summarizer) Summarize(ctx context.Context, track *captions.Track) (*Digest, error) {
	chunks := ChunkCues(track.Cues)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no transcript content to summarize")
	}

	// Map.
	summaries := make([]ChunkSummary, 0, len(chunks))
	for i, c := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("summarize cancelled after %d/%d chunks: %w", i, len(chunks), err)
		}
		var cs ChunkSummary
		user := formatChunkForPrompt(c, i, len(chunks))
		if err := s.LLM.CompleteJSON(ctx, chunkSystemPrompt, user, chunkSchema, &cs); err != nil {
			return nil, fmt.Errorf("summarize chunk %d/%d: %w", i+1, len(chunks), err)
		}
		summaries = append(summaries, cs)
		if s.Progress != nil {
			s.Progress(i+1, len(chunks))
		}
	}

	// Reduce in rounds: 6 → 2 → 1, rather than everything at once.
	merged := summaries
	round := 1
	for len(merged) > 1 {
		var next []ChunkSummary
		group := 3
		for i := 0; i < len(merged); i += group {
			end := i + group
			if end > len(merged) {
				end = len(merged)
			}
			batch := merged[i:end]
			if len(batch) == 1 {
				next = append(next, batch[0])
				continue
			}
			var cs ChunkSummary
			user := formatBatchForMerge(batch, round)
			if err := s.LLM.CompleteJSON(ctx, mergeSystemPrompt, user, chunkSchema, &cs); err != nil {
				return nil, fmt.Errorf("merge round %d: %w", round, err)
			}
			next = append(next, cs)
		}
		if len(next) >= len(merged) {
			// Merging isn't converging; taking what we have beats looping.
			merged = next
			break
		}
		merged = next
		round++
	}

	// Final digest, written fresh from the merged summaries in the video's own order.
	var d digestJSON
	user := formatForDigest(merged)
	if err := s.LLM.CompleteJSON(ctx, mergeSystemPrompt, user, digestSchema, &d); err != nil {
		return nil, fmt.Errorf("build final digest: %w", err)
	}

	return &Digest{
		Title:     track.Title,
		Abstract:  strings.TrimSpace(d.Abstract),
		Sections:  d.Sections,
		KeyPoints: d.KeyPoints,
		Concepts:  d.Concepts,
		Gaps:      strings.TrimSpace(d.Gaps),
	}, nil
}

type digestJSON struct {
	Abstract  string     `json:"abstract"`
	Sections  []Section  `json:"sections"`
	KeyPoints []KeyPoint `json:"key_points"`
	Concepts  []string   `json:"concepts"`
	Gaps      string     `json:"gaps"`
}

// ChunkCues splits cues into chunks near TargetChunkTokens, never mid-cue.
func ChunkCues(cues []captions.Cue) []Chunk {
	if len(cues) == 0 {
		return nil
	}

	var chunks []Chunk
	var cur []captions.Cue
	chars := 0

	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Cues:  cur,
			First: cur[0].Start,
			Last:  cur[len(cur)-1].Start,
		})
		// Carry the tail of this chunk into the next so a sentence spanning the boundary
		// survives.
		overlap := cur
		if len(overlap) > 1 {
			kept := 0
			count := 0
			for i := len(overlap) - 1; i >= 0; i-- {
				count += len(overlap[i].Text)
				if count > chunkOverlapChars {
					break
				}
				kept = i
			}
			overlap = overlap[kept:]
		}
		cur = append([]captions.Cue(nil), overlap...)
		chars = 0
		for _, c := range overlap {
			chars += len(c.Text)
		}
	}

	for _, c := range cues {
		cur = append(cur, c)
		chars += len(c.Text) + 12 // +12 approximates the "[mm:ss] " marker cost

		// Only cut once past the target, and always on a cue boundary.
		if chars >= TargetChunkTokens*CharsPerToken {
			flush()
		}
	}
	// Trailing chunk, unless everything fit in one.
	if len(cur) > 0 && (len(chunks) == 0 || len(cur) != len(cues)) {
		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Cues:  cur,
			First: cur[0].Start,
			Last:  cur[len(cur)-1].Start,
		})
	}
	return chunks
}

// formatCue renders one cue with its timestamp, which is what lets the model cite "t".
func formatCue(c captions.Cue) string {
	return fmt.Sprintf("[%s] %s", clock(c.Start), c.Text)
}

func clock(d time.Duration) string {
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

func formatChunkForPrompt(c Chunk, i, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Section %d of %d, spanning %s to %s.\n\nTranscript:\n",
		i+1, total, clock(c.First), clock(c.Last))
	for _, cue := range c.Cues {
		b.WriteString(formatCue(cue))
		b.WriteString("\n")
	}
	return b.String()
}

func formatBatchForMerge(batch []ChunkSummary, round int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merge these %d section summaries (round %d) into one.\n\n", len(batch), round)
	for i, cs := range batch {
		fmt.Fprintf(&b, "--- Section %d ---\nTopic: %s\nSummary: %s\n",
			i+1, cs.Topic, cs.Summary)
		for _, kp := range cs.KeyPoints {
			fmt.Fprintf(&b, "  - [%ds] %s (quote: %q)\n",
				int(kp.T), kp.Point, kp.Quote)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func formatForDigest(summaries []ChunkSummary) string {
	var b strings.Builder
	b.WriteString("Write the final digest for this video from the merged section summaries below.\n\n")
	for i, cs := range summaries {
		fmt.Fprintf(&b, "--- Section %d ---\nTopic: %s\nSummary: %s\n",
			i+1, cs.Topic, cs.Summary)
		for _, kp := range cs.KeyPoints {
			fmt.Fprintf(&b, "  - [%ds] %s (quote: %q)\n", int(kp.T), kp.Point, kp.Quote)
		}
		if len(cs.Concepts) > 0 {
			fmt.Fprintf(&b, "Concepts: %s\n", strings.Join(cs.Concepts, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}
