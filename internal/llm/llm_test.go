package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ollamaLike mimics the response shape of an OpenAI-compatible server.
func ollamaLike(t *testing.T, content string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCompleteReturnsText(t *testing.T) {
	srv := ollamaLike(t, "a plain answer", http.StatusOK)
	c := New(srv.URL+"/v1", "test-model", 5*time.Second)

	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "a plain answer" {
		t.Errorf("got %q, want %q", got, "a plain answer")
	}
}

func TestCompleteSendsModelAndMessages(t *testing.T) {
	var body chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "ok"}},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "qwen2.5:7b", 5*time.Second)
	if _, err := c.Complete(context.Background(), "be terse", "summarize this"); err != nil {
		t.Fatal(err)
	}

	if body.Model != "qwen2.5:7b" {
		t.Errorf("model = %q, want qwen2.5:7b", body.Model)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(body.Messages))
	}
	if body.Messages[0].Role != "system" || body.Messages[0].Content != "be terse" {
		t.Errorf("system message = %+v", body.Messages[0])
	}
	if body.Stream {
		t.Error("stream = true, want false — the client does not read SSE")
	}
}

func TestCompleteReportsUnreachableHostDistinctly(t *testing.T) {
	// A refused connection means the Mac Mini is unreachable, not that the prompt is bad.
	// The two need different user-facing messages (ADR 0002).
	c := New("http://127.0.0.1:1/v1", "m", 2*time.Second)
	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("want error against a dead host")
	}
	if !strings.Contains(err.Error(), "cannot reach inference host") {
		t.Errorf("err = %v, want an unreachable-host error", err)
	}
}

func TestCompleteReportsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "ghost", 5*time.Second)
	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("want error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to mention the status", err)
	}
}

func TestCompleteRejectsEmptyResponse(t *testing.T) {
	srv := ollamaLike(t, "   ", http.StatusOK)
	c := New(srv.URL+"/v1", "m", 5*time.Second)
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("want error on a blank completion")
	}
}

func TestCompleteHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "m", 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Complete(ctx, "s", "u"); err == nil {
		t.Fatal("want error on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want the context deadline to cut it short", elapsed)
	}
}

func TestCompleteJSONParsesCleanJSON(t *testing.T) {
	srv := ollamaLike(t, `{"topic":"nets","key_points":["a"]}`, http.StatusOK)
	c := New(srv.URL+"/v1", "m", 5*time.Second)

	var out struct {
		Topic     string   `json:"topic"`
		KeyPoints []string `json:"key_points"`
	}
	if err := c.CompleteJSON(context.Background(), "s", "u", nil, &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Topic != "nets" || len(out.KeyPoints) != 1 {
		t.Errorf("out = %+v", out)
	}
}

func TestCompleteJSONUnwrapsFencedBlock(t *testing.T) {
	// Models wrap JSON in a markdown fence often enough that this must work.
	body := "Here is the result:\n```json\n{\"topic\":\"nets\"}\n```\nHope that helps!"
	srv := ollamaLike(t, body, http.StatusOK)
	c := New(srv.URL+"/v1", "m", 5*time.Second)

	var out struct {
		Topic string `json:"topic"`
	}
	if err := c.CompleteJSON(context.Background(), "s", "u", nil, &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Topic != "nets" {
		t.Errorf("topic = %q, want nets", out.Topic)
	}
}

func TestCompleteJSONUnwrapsProse(t *testing.T) {
	body := `Sure! {"topic":"nets","n":2} — let me know if you need more.`
	srv := ollamaLike(t, body, http.StatusOK)
	c := New(srv.URL+"/v1", "m", 5*time.Second)

	var out struct {
		Topic string `json:"topic"`
		N     int    `json:"n"`
	}
	if err := c.CompleteJSON(context.Background(), "s", "u", nil, &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Topic != "nets" || out.N != 2 {
		t.Errorf("out = %+v", out)
	}
}

func TestCompleteJSONSendsSchema(t *testing.T) {
	var body chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": `{"topic":"x"}`}},
			},
		})
	}))
	defer srv.Close()

	schema := map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]string{"type": "string"}}}
	c := New(srv.URL+"/v1", "m", 5*time.Second)
	var out struct {
		Topic string `json:"topic"`
	}
	if err := c.CompleteJSON(context.Background(), "s", "u", schema, &out); err != nil {
		t.Fatal(err)
	}
	if len(body.Format) == 0 {
		t.Error("schema not sent in the format field")
	}
	if !strings.Contains(string(body.Format), "topic") {
		t.Errorf("format = %s, want the schema contents", body.Format)
	}
}

func TestCompleteJSONFailsLoudlyOnMalformed(t *testing.T) {
	// A summary with key points silently dropped is worse than one that failed.
	srv := ollamaLike(t, "this is not json at all", http.StatusOK)
	c := New(srv.URL+"/v1", "m", 5*time.Second)

	var out struct {
		Topic string `json:"topic"`
	}
	err := c.CompleteJSON(context.Background(), "s", "u", nil, &out)
	if err == nil {
		t.Fatal("want an error on non-JSON output")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"prose {\"a\":1} more prose", `{"a":1}`},
		{"[1,2,3]", "[1,2,3]"},
		{"  {\"a\":1}  ", `{"a":1}`},
	}
	for _, c := range cases {
		got, err := extractJSON(c.in)
		if err != nil {
			t.Errorf("extractJSON(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("extractJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractJSONRejectsNonJSON(t *testing.T) {
	for _, in := range []string{"", "   ", "no braces here"} {
		if _, err := extractJSON(in); err == nil {
			t.Errorf("extractJSON(%q) succeeded, want an error", in)
		}
	}
}

func TestModelName(t *testing.T) {
	c := New("http://x/v1", "gemma3:12b", time.Second)
	if c.ModelName() != "gemma3:12b" {
		t.Errorf("ModelName = %q", c.ModelName())
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("http://host:11434/v1/", "m", time.Second)
	if strings.HasSuffix(c.BaseURL, "/") {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
}
