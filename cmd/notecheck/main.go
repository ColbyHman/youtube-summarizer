// Command notecheck renders one note from the cached caption fixture against a stub inference
// server and prints the head of the result. It exists to eyeball the note format without
// needing a real model; it is not part of the bot.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/ColbyHman/yt-digest/internal/captions"
	"github.com/ColbyHman/yt-digest/internal/llm"
	"github.com/ColbyHman/yt-digest/internal/youtube"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		var user string
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}

		reply := `{"topic":"Intro","summary":"Opens the video.","key_points":[{"point":"Neurons combine weighted inputs","evidence_quote":"weighted inputs","t":7}],"concepts":["neuron"]}`
		if strings.Contains(user, "final digest") {
			reply = `{"abstract":"An explanation of what neural networks actually compute, aimed at people who already know some linear algebra.","sections":[{"heading":"What a neuron computes","summary":"Breaks the idea into a weighted sum plus an activation.","start":0,"key_points":[{"point":"A neuron is a weighted sum plus nonlinearity","evidence_quote":"weighted inputs","t":7}]}],"key_points":[{"point":"A neuron is a weighted sum plus nonlinearity","evidence_quote":"weighted inputs","t":7}],"concepts":["neuron","activation function","weight matrix"],"gaps":"The final architecture discussion runs past the truncated transcript."}`
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": reply}},
			},
		})
	}))
	defer srv.Close()

	const fixture = "/tmp/t.en.vtt"
	cues, _, err := captions.ParseVTTFile(fixture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no fixture at %s: %v\n", fixture, err)
		os.Exit(1)
	}

	track := &captions.Track{
		Title:    "But what is a neural network? | Deep learning chapter 1",
		Channel:  "3Blue1Brown",
		Duration: 1120,
		Source:   "auto-captions",
		Cues:     cues,
	}

	client := llm.New(srv.URL+"/v1", "qwen2.5:7b", 10*time.Second)
	digest, err := (&youtube.Summarizer{LLM: client}).Summarize(context.Background(), track)
	if err != nil {
		fmt.Fprintln(os.Stderr, "summarize:", err)
		os.Exit(1)
	}

	vault, _ := os.MkdirTemp("", "vault-")
	path, err := youtube.Render(youtube.Config{VaultDir: vault, NoteDir: "Video Notes"},
		"aircAruvnKk", track, digest, "qwen2.5:7b")
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	// Print the head only; the tail is the full transcript.
	head := 2000
	if len(b) < head {
		head = len(b)
	}
	fmt.Println(string(b[:head]))
	fmt.Printf("\n... [transcript continues — %d bytes total, written to %s]\n", len(b), path)
}
