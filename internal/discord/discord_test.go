package discord

import (
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestFriendlyErrorQueueFull(t *testing.T) {
	// Queue exhaustion is a "try again in a minute", not a failure the user caused.
	msg := friendlyError(errors.New("queue is full, try again shortly"))
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "try again") {
		t.Errorf("msg = %q, want a retry suggestion", msg)
	}
	if !strings.Contains(lower, "busy") {
		t.Errorf("msg = %q, want it to explain why", msg)
	}
	// It must not be dressed up as a failure.
	if strings.Contains(msg, "⚠️") {
		t.Errorf("msg = %q, queue-full is not a failure", msg)
	}
}

func TestFriendlyErrorPassthrough(t *testing.T) {
	msg := friendlyError(errors.New("extraction failed: video unavailable"))
	if !strings.Contains(msg, "extraction failed") {
		t.Errorf("msg = %q, want the underlying reason", msg)
	}
	if !strings.HasPrefix(msg, "⚠️") {
		t.Errorf("msg = %q, want a warning prefix", msg)
	}
}

func TestFirstOptionValue(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{
		Name: CommandName,
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Type: discordgo.ApplicationCommandOptionString, Value: "https://youtu.be/aircAruvnKk"},
		},
	}
	if got := firstOptionValue(data); got != "https://youtu.be/aircAruvnKk" {
		t.Errorf("firstOptionValue = %q", got)
	}
}

func TestFirstOptionValueHandlesMissingOption(t *testing.T) {
	// A malformed interaction must yield "" and a friendly message, not an index panic.
	if got := firstOptionValue(discordgo.ApplicationCommandInteractionData{}); got != "" {
		t.Errorf("firstOptionValue(no options) = %q, want empty", got)
	}
}

func TestFirstOptionValueIgnoresNonStringOptions(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Value: float64(7)},
		},
	}
	if got := firstOptionValue(data); got != "" {
		t.Errorf("firstOptionValue(integer) = %q, want empty", got)
	}
}

func TestFirstOptionValueHandlesWrongValueType(t *testing.T) {
	// A string option carrying a non-string value must not panic on the type assertion.
	data := discordgo.ApplicationCommandInteractionData{
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Type: discordgo.ApplicationCommandOptionString, Value: 42},
		},
	}
	if got := firstOptionValue(data); got != "" {
		t.Errorf("firstOptionValue(mistyped) = %q, want empty", got)
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Options{OnVideo: func(string, string, string) error { return nil }}); err == nil {
		t.Error("want an error with no token")
	}
}

func TestNewRequiresHandler(t *testing.T) {
	if _, err := New(Options{Token: "x"}); err == nil {
		t.Error("want an error with no OnVideo handler")
	}
}

func TestNewSucceeds(t *testing.T) {
	b, err := New(Options{
		Token:   "x",
		AppID:   "123",
		OnVideo: func(string, string, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("New returned a nil bot")
	}
}
