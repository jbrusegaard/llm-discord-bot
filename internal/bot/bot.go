package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"app/internal/config"
	"app/internal/llm"
	"app/internal/store"
)

const discordMaxLen = 2000

// Bot wires the Discord gateway to the LLM client.
type Bot struct {
	dg         *discordgo.Session
	llm        *llm.Client
	system     string
	hist       *store.Store
	compactor  *compactor
	maxHistory int
	timeout    time.Duration
}

// New constructs a Bot from config. hist must be a live store.
func New(cfg config.Config, session *discordgo.Session, hist *store.Store) *Bot {
	client := llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model)
	return &Bot{
		dg:         session,
		llm:        client,
		system:     cfg.SystemPrompt + "\n\n" + reminderToolNote,
		hist:       hist,
		compactor:  newCompactor(hist, client, cfg.CompactAt),
		maxHistory: cfg.MaxHistory,
		timeout:    3 * time.Minute,
	}
}

// StartCompactor launches the background compaction worker; it stops
// when ctx is cancelled.
func (b *Bot) StartCompactor(ctx context.Context) {
	go b.compactor.run(ctx)
}

// HandleMessageCreate is the discordgo OnMessageCreate callback.
func (b *Bot) HandleMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore the bot's own messages.
	if m.Author.ID == b.dg.State.User.ID {
		return
	}
	// Ignore DMs from webhooks/bots.
	if m.Author.Bot {
		return
	}

	channel, err := b.dg.Channel(m.ChannelID)
	if err != nil {
		slog.Error("failed to fetch channel", "id", m.ChannelID, "err", err)
		return
	}
	isDM := channel.Type == discordgo.ChannelTypeDM

	mentioned := false
	for _, ref := range m.Mentions {
		if ref.ID == b.dg.State.User.ID {
			mentioned = true
			break
		}
	}
	// In guilds, only respond when mentioned; in DMs, always respond.
	if !isDM && !mentioned {
		return
	}

	text := strings.TrimSpace(strings.TrimPrefix(m.Content, "<@"+b.dg.State.User.ID+"> "))
	text = strings.TrimSpace(strings.TrimPrefix(text, "<@!"+b.dg.State.User.ID+">"))
	if text == "" {
		return
	}

	if strings.EqualFold(text, "/reset") {
		if err := b.hist.Clear(m.Author.ID, m.ChannelID); err != nil {
			slog.Error("failed to clear history", "user", m.Author.ID, "channel", m.ChannelID, "err", err)
		}
		if _, err := b.dg.ChannelMessageSendReply(channel.ID, "✅ Memory cleared for this conversation.", m.SoftReference()); err != nil {
			slog.Error("failed to send reset ack", "err", err)
		}
		return
	}

	if strings.EqualFold(text, "/reminders") {
		pending, err := b.hist.PendingForUser(m.Author.ID)
		var out string
		switch {
		case err != nil:
			slog.Error("failed to list reminders", "user", m.Author.ID, "err", err)
			out = "⚠️ Couldn't load your reminders."
		case len(pending) == 0:
			out = "⏰ No pending reminders. Ask me to remind you about something!"
		default:
			var sb strings.Builder
			fmt.Fprintf(&sb, "⏰ You have %d pending reminder(s):\n", len(pending))
			for _, r := range pending {
				fmt.Fprintf(&sb, "• %s — %s\n", whenText(r.DueAt), r.Message)
			}
			out = strings.TrimRight(sb.String(), "\n")
		}
		if _, err := b.dg.ChannelMessageSendReply(channel.ID, out, m.SoftReference()); err != nil {
			slog.Error("failed to send reminder list", "err", err)
		}
		return
	}

	// Give immediate feedback while the model thinks: typing indicator plus
	// a "Thinking…" message that will be edited into the reply. Every bot
	// message is sent as a Discord reply to the user's message so responses
	// stay visually attached to what triggered them. SoftReference keeps the
	// send working even if the user deletes their message mid-generation.
	if err := b.dg.ChannelTyping(channel.ID); err != nil {
		slog.Debug("typing indicator failed", "err", err)
	}
	placeholder, phErr := b.dg.ChannelMessageSendReply(channel.ID, "🤔 Thinking…", m.SoftReference())
	if phErr != nil {
		slog.Warn("failed to send thinking placeholder", "err", phErr)
		placeholder = nil
	}

	reply, err := b.chat(m.Author.ID, m.ChannelID, text)
	if err != nil {
		slog.Error("chat failed", "user", m.Author.Username, "err", err)
		notice := "⚠️ Sorry, I couldn't get a response from the model. Please try again."
		if placeholder != nil {
			if _, editErr := b.dg.ChannelMessageEdit(placeholder.ChannelID, placeholder.ID, notice); editErr != nil {
				slog.Error("failed to update placeholder with error notice", "err", editErr)
			}
		} else if _, sendErr := b.dg.ChannelMessageSendReply(channel.ID, notice, m.SoftReference()); sendErr != nil {
			slog.Error("failed to send error notice", "err", sendErr)
		}
		return
	}

	chunks := splitMessage(reply, discordMaxLen)
	if placeholder != nil {
		// Replace the "Thinking…" message with the first chunk so the
		// reply reads as one message; fall back to a plain send on error.
		if _, editErr := b.dg.ChannelMessageEdit(placeholder.ChannelID, placeholder.ID, chunks[0]); editErr != nil {
			slog.Error("failed to edit placeholder with reply", "err", editErr)
		} else {
			chunks = chunks[1:]
		}
	}
	for _, chunk := range chunks {
		if _, err := b.dg.ChannelMessageSendReply(channel.ID, chunk, m.SoftReference()); err != nil {
			slog.Error("failed to send reply chunk", "err", err)
			return
		}
	}
}

// chat runs the LLM round trip for one user turn in a specific
// conversation (user + channel), updating that conversation's history.
func (b *Bot) chat(userID, channelID, text string) (string, error) {
	past, err := b.hist.Recent(userID, channelID, b.maxHistory)
	if err != nil {
		return "", fmt.Errorf("load history: %w", err)
	}
	messages := []llm.Message{{Role: llm.RoleSystem, Content: b.system}}
	// Local models have no clock of their own (knowledge is frozen at
	// training cutoff), so tell them what time it is; without this they
	// can't answer "what day is it?" correctly.
	messages = append(messages, llm.Message{
		Role: llm.RoleSystem,
		Content: "Current date and time on the machine running the bot: " +
			time.Now().Format("Monday, January 2, 2006 at 3:04 PM MST"),
	})
	if summary, err := b.hist.GetSummary(userID, channelID); err == nil && summary != "" {
		messages = append(messages, llm.Message{
			Role:    llm.RoleSystem,
			Content: "Summary of the earlier part of this user's conversation:\n" + summary,
		})
	}
	messages = append(messages, past...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: text})

	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	replyMsg, err := b.llm.ChatWithTools(ctx, messages, []llm.Tool{createReminderTool})
	cancel()
	if err != nil {
		// Some models reject the tools parameter; retry plain so basic chat
		// still works (reminders just won't be scheduled).
		slog.Warn("tool-enabled call failed; retrying without tools", "err", err)
		ctx, cancel = context.WithTimeout(context.Background(), b.timeout)
		plain, perr := b.llm.Chat(ctx, messages)
		cancel()
		if perr != nil {
			return "", err // keep the original (more informative) error
		}
		replyMsg = llm.Message{Role: llm.RoleAssistant, Content: plain}
	}

	var reply string
	if len(replyMsg.ToolCalls) > 0 {
		// The model wants to schedule something; execute the tool calls and
		// let it write a short confirmation.
		reply, err = b.handleToolCalls(userID, channelID, messages, replyMsg)
	} else {
		reply = strings.TrimSpace(replyMsg.Content)
	}
	if err != nil {
		return "", err
	}
	if reply == "" {
		return "", fmt.Errorf("empty model response")
	}
	if err := b.hist.Append(userID, channelID, string(llm.RoleUser), text); err != nil {
		return "", fmt.Errorf("save user message: %w", err)
	}
	if err := b.hist.Append(userID, channelID, string(llm.RoleAssistant), reply); err != nil {
		return "", fmt.Errorf("save assistant message: %w", err)
	}
	b.compactor.Enqueue(userID, channelID)
	return reply, nil
}

// PingLM checks the LM Studio server; used at startup.
func (b *Bot) PingLM(ctx context.Context) error { return b.llm.Ping(ctx) }

// UpdatePresence sets the bot's Discord status so members can see which
// model it is running. Call after the gateway has opened.
func (b *Bot) UpdatePresence(lmReachable bool) error {
	state := "🧠 " + b.llm.Model()
	if !lmReachable {
		state = "🔌 Waiting for LM Studio…"
	}
	return b.dg.UpdateCustomStatus(state)
}

// splitMessage splits long text on newlines, then hard-cuts if needed.
func splitMessage(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var chunks []string
	rest := s
	for len(rest) > limit {
		cut := strings.LastIndexByte(rest[:limit], '\n')
		if cut <= 0 {
			cut = limit
		}
		chunks = append(chunks, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], "\n")
	}
	if rest != "" {
		chunks = append(chunks, rest)
	}
	return chunks
}
