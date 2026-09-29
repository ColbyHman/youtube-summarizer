// Package llm is a minimal OpenAI-compatible chat client.
//
// No client library: the bot talks to Ollama's /v1/chat/completions over plain HTTP, and
// hand-rolled structs keep the binary and dependency list small (ADR 0002). That constraint
// is deliberate — the inference host is a Mac Mini on the LAN, and the bot needs nothing from
// it beyond a JSON response.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// New builds a client. baseURL should include the /v1 suffix, e.g.
// http://mac-mini.local:11434/v1
func New(baseURL, model string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// Format asks Ollama for JSON-constrained output where it supports it. Ignored by other
	// OpenAI-compatible servers, which is fine — the schema is validated either way.
	Format json.RawMessage `json:"format,omitempty"`
	// Options carries Ollama-specific sampling params. Zero value means server defaults.
	Options *genOptions `json:"options,omitempty"`
}

type genOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a single-turn prompt and returns the assistant's text.
//
// A connection failure is reported distinctly from a server error, because they mean different
// things operationally: a refused connection is the Mac Mini being unreachable, not a bad
// prompt, and the user-facing message should say so (ADR 0002).
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("llm: no base URL configured")
	}

	body := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream: false,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("llm: request cancelled: %w", ctx.Err())
		}
		return "", fmt.Errorf("llm: cannot reach inference host at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	// Bound the read: a runaway server shouldn't be able to exhaust memory here.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: inference host returned %s: %s",
			resp.Status, snippet(payload, 300))
	}

	var out chatResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices in response: %s", snippet(payload, 300))
	}

	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("llm: empty completion")
	}
	return text, nil
}

// CompleteJSON asks for JSON output constrained to the given schema, then parses it.
//
// The schema is advisory to the server but authoritative on return: a malformed body is an
// error, never a partially-populated struct. A summary with missing key points silently
// dropped is worse than one that failed loudly.
func (c *Client) CompleteJSON(ctx context.Context, system, user string, schema any, out any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("llm: marshal schema: %w", err)
	}

	body := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream: false,
		Format: raw,
	}

	payload, err := c.doJSON(ctx, body)
	if err != nil {
		return err
	}

	// Models wrap JSON in prose or fences often enough that a bare Unmarshal is not enough.
	text, err := extractJSON(string(payload))
	if err != nil {
		return fmt.Errorf("llm: %w (raw: %s)", err, snippet(payload, 300))
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("llm: decode into %T: %w", out, err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, body chatRequest) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("llm: request cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("llm: cannot reach inference host at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: inference host returned %s: %s",
			resp.Status, snippet(payload, 300))
	}

	var out chatResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm: no choices in response: %s", snippet(payload, 300))
	}
	return []byte(out.Choices[0].Message.Content), nil
}

// extractJSON pulls a JSON value out of a model response that may be wrapped in prose or
// fenced in a markdown block.
func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty response")
	}

	if fenced := between(s, "```json", "```"); fenced != "" {
		s = strings.TrimSpace(fenced)
	} else if fenced := between(s, "```", "```"); fenced != "" {
		s = strings.TrimSpace(fenced)
	}

	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		return s, nil
	}

	// Fall back to the outermost brace pair.
	start := strings.IndexAny(s, "{[")
	end := strings.LastIndexAny(s, "}]")
	if start < 0 || end <= start {
		return "", fmt.Errorf("no JSON found in response")
	}
	return s[start : end+1], nil
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Summarizer is the interface the skill depends on, so tests can substitute a stub without an
// inference host. CompleteJSON is the only method the real summarizer needs.
type Summarizer interface {
	CompleteJSON(ctx context.Context, system, user string, schema any, out any) error
	// ModelName reports the model for stamping into note frontmatter. It is named differently
	// from the Model field because Go forbids a field and method sharing a name.
	ModelName() string
}

// ModelName reports the configured model name.
func (c *Client) ModelName() string { return c.Model }
