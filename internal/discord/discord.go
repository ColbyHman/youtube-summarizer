// Package discord wires the bot to a Discord guild.
//
// The gateway layer is deliberately thin: acknowledge fast, hand work to the queue, post the
// result when it lands. It holds no summarization logic — that belongs to the skill (ADR 0005).
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// CommandName is the slash command users invoke.
const CommandName = "summarize"

// Options configures the bot.
type Options struct {
	Token   string
	AppID   string
	GuildID string
	Log     *slog.Logger
	// QueueDepth and Workers are passed through to the queue.
	QueueDepth int
	Workers    int
	// OnVideo is called with the raw user argument. It must return quickly — it enqueues.
	OnVideo func(arg, channelID, userID string) error
}

// Bot owns the gateway session.
type Bot struct {
	session *discordgo.Session
	opts    Options
	log     *slog.Logger

	// seen guards against two identical slash commands arriving in the same tick; Discord
	// retries interactions, and a retry must not enqueue the job twice.
	seen sync.Map
}

// New creates a bot and its gateway session.
func New(opts Options) (*Bot, error) {
	if opts.Token == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN is required")
	}
	if opts.OnVideo == nil {
		return nil, fmt.Errorf("OnVideo handler is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.GuildID == "" {
		log.Warn("DISCORD_GUILD_ID not set, commands will register globally (can take an hour)")
	}

	sess, err := discordgo.New("Bot " + opts.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	sess.Identify.Intents = discordgo.IntentsGuilds

	return &Bot{session: sess, opts: opts, log: log}, nil
}

// Open connects to the gateway and blocks until ctx is cancelled.
func (b *Bot) Open(ctx context.Context) error {
	b.registerHandlers()

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	defer b.session.Close()

	b.log.Info("connected to discord",
		"user", b.session.State.User.String(),
		"guild", b.opts.GuildID)

	<-ctx.Done()
	b.log.Info("disconnecting from discord")
	return nil
}

func (b *Bot) registerHandlers() {
	b.session.AddHandler(func(s *discordgo.Session, r *discordgo.InteractionCreate) {
		if r.Type != discordgo.InteractionApplicationCommand {
			return
		}
		// ApplicationCommandData panics on the wrong interaction type, so the type check above
		// has to come first — and a malformed payload should not take the process down.
		var data discordgo.ApplicationCommandInteractionData
		if ok := func() (ok bool) {
			defer func() {
				if rec := recover(); rec != nil {
					ok = false
				}
			}()
			data = r.ApplicationCommandData()
			return true
		}(); !ok {
			b.log.Warn("malformed application command interaction", "id", r.ID)
			return
		}
		if data.Name != CommandName {
			return
		}
		b.handleSummarize(s, r, data)
	})

	b.session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		if err := b.RegisterCommands(); err != nil {
			b.log.Error("register commands", "err", err)
		}
	})
}

// firstOptionValue returns the first string option's value, or "" if there is none. Guarded
// because a malformed interaction should produce a friendly message, not an index panic.
func firstOptionValue(data discordgo.ApplicationCommandInteractionData) string {
	for _, opt := range data.Options {
		if opt.Type == discordgo.ApplicationCommandOptionString {
			if v, ok := opt.Value.(string); ok {
				return v
			}
		}
	}
	return ""
}

func (b *Bot) handleSummarize(s *discordgo.Session, r *discordgo.InteractionCreate,
	data discordgo.ApplicationCommandInteractionData) {
	// Discord gives an interaction three seconds. Everything slow happens in the queue, so
	// this path is enqueue-and-reply only (ADR 0003).
	defer func() {
		if rec := recover(); rec != nil {
			b.log.Error("panic handling interaction", "panic", rec)
		}
	}()

	arg := strings.TrimSpace(firstOptionValue(data))
	if arg == "" {
		respond(s, r, "Give me a YouTube link.")
		return
	}

	// A retried interaction carries the same ID; answering it twice would enqueue twice.
	if _, loaded := b.seen.LoadOrStore(r.ID, true); loaded {
		b.log.Debug("duplicate interaction ignored", "id", r.ID)
		return
	}

	channelID := ""
	if r.ChannelID != "" {
		channelID = r.ChannelID
	} else if ch, err := s.Channel(r.Message.ChannelID); err == nil {
		channelID = ch.ID
	}

	if err := b.opts.OnVideo(arg, channelID, r.Member.User.ID); err != nil {
		respond(s, r, friendlyError(err))
		return
	}
	respond(s, r, "Queued — I'll post the note here when it's ready.")
}

// respond replies to an interaction, tolerating the interaction having already expired.
func respond(s *discordgo.Session, r *discordgo.InteractionCreate, msg string) {
	err := s.InteractionRespond(r.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg},
	})
	if err != nil {
		// A failed acknowledgement is not fatal: the job is already queued and will post its
		// own follow-up in-channel.
		slog.Default().Warn("interaction respond failed", "err", err)
	}
}

// Reply posts a message to a channel. This is how job results reach the user, since the
// interaction itself is long gone by the time a summary is ready.
func (b *Bot) Reply(channelID, msg string) {
	if channelID == "" {
		return
	}
	// Discord's hard message limit is 2000 characters; a long summary must not silently vanish.
	if len(msg) > 1900 {
		msg = msg[:1900] + "…"
	}

	_, err := b.session.ChannelMessageSend(channelID, msg)
	if err != nil {
		b.log.Error("post reply", "channel", channelID, "err", err)
	}
}

// RegisterCommands creates or updates the slash command.
func (b *Bot) RegisterCommands() error {
	cmd := &discordgo.ApplicationCommand{
		Name:        CommandName,
		Description: "Summarize a YouTube video into an Obsidian note",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "url",
				Description: "YouTube link, youtu.be link, Shorts link, or bare video ID",
				Required:    true,
			},
		},
	}

	// An empty guildID registers globally, which can take up to an hour to propagate; a
	// guild ID registers instantly, which is what you want while iterating.
	registered, err := b.session.ApplicationCommandCreate(b.opts.AppID, b.opts.GuildID, cmd)
	if err != nil {
		return fmt.Errorf("register command: %w", err)
	}
	b.log.Info("slash command registered", "command", registered.Name, "id", registered.ID)
	return nil
}

// friendlyError turns internal errors into something a human can act on.
//
// The queue-full case is deliberately distinct: it is not a failure of the request, the
// inference host is simply busy. Telling a user a link "failed" when the real answer is
// "wait a minute" would be misleading.
func friendlyError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "queue is full") {
		return "Queue is full — the inference host is busy. Try again in a minute."
	}
	return "⚠️ " + msg
}

// RunContext is a small helper so callers can bound their own timeouts.
func RunContext(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
