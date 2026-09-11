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

// memoryNote tells the model how its two memory layers work so it can answer
// "what do you remember about me?" honestly.
const memoryNote = "You have two kinds of memory: (1) a list of durable facts about each user, shared across all channels and DMs; (2) separate recent context for each channel/thread/DM conversation. When asked what you remember about them, use the stored facts."

// Bot wires the Discord gateway to the LLM client.
type Bot struct {
	dg         *discordgo.Session
	llm        *llm.Client
	system     string
	hist       *store.Store
	compactor  *compactor
	facts      *factExtractor
	maxHistory int
	timeout    time.Duration
}

// New constructs a Bot from config. hist must be a live store.
func New(cfg config.Config, session *discordgo.Session, hist *store.Store) *Bot {
	client := llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model)
	return &Bot{
		dg:         session,
		llm:        client,
		system:     cfg.SystemPrompt + "\n\n" + reminderToolNote + "\n\n" + memoryNote,
		hist:       hist,
		compactor:  newCompactor(hist, client, cfg.CompactAt),
		facts:      newFactExtractor(hist, client),
		maxHistory: cfg.MaxHistory,
		timeout:    3 * time.Minute,
	}
}

// StartCompactor launches the background compaction worker; it stops
// when ctx is cancelled.
func (b *Bot) StartCompactor(ctx context.Context) {
	go b.compactor.run(ctx)
}

// StartFactExtractor launches the background per-user fact extraction
// worker; it stops when ctx is cancelled.
func (b *Bot) StartFactExtractor(ctx context.Context) {
	go b.facts.run(ctx)
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
	// Rewrite other users' mentions so the model sees readable names while
	// keeping exact ids it can pass to tools (e.g. mention_user_id).
	text, mentionIDs := b.rewriteMentions(text, m)

	if strings.EqualFold(text, "/reset") {
		if err := b.hist.Clear(m.Author.ID, m.ChannelID); err != nil {
			slog.Error("failed to clear history", "user", m.Author.ID, "channel", m.ChannelID, "err", err)
		}
		if _, err := b.dg.ChannelMessageSendReply(channel.ID, "✅ Memory cleared for this conversation.", m.SoftReference()); err != nil {
			slog.Error("failed to send reset ack", "err", err)
		}
		return
	}

	if strings.EqualFold(text, "/facts") {
		facts, err := b.hist.FactsForUser(m.Author.ID)
		var out string
		switch {
		case err != nil:
			slog.Error("failed to list user facts", "user", m.Author.ID, "err", err)
			out = "⚠️ Couldn't load what I know about you."
		case len(facts) == 0:
			out = "🧠 I don't have any stored memories about you yet. Tell me something worth remembering!"
		default:
			var sb strings.Builder
			fmt.Fprintf(&sb, "🧠 Here's what I remember about you:\n")
			for _, f := range facts {
				fmt.Fprintf(&sb, "• %s\n", f)
			}
			out = strings.TrimRight(sb.String(), "\n")
		}
		if _, err := b.dg.ChannelMessageSendReply(channel.ID, out, m.SoftReference()); err != nil {
			slog.Error("failed to send facts list", "err", err)
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
				suffix := ""
				if r.TargetUserID != "" && r.TargetUserID != m.Author.ID {
					suffix = fmt.Sprintf(" (for <@%s>)", r.TargetUserID)
				}
				fmt.Fprintf(&sb, "• %s%s — %s\n", whenText(r.DueAt), suffix, r.Message)
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

	reply, err := b.chat(m.Author.ID, m.ChannelID, text, mentionIDs)
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
// mentioned lists other users @mentioned in the triggering message; tools may
// target them (e.g. "tell @Alec ... later").
func (b *Bot) chat(userID, channelID, text string, mentioned map[string]bool) (string, error) {
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
	// Per-user facts apply in every channel and DM, so they are injected
	// here regardless of where this conversation happens.
	if facts, err := b.hist.FactsForUser(userID); err == nil && len(facts) > 0 {
		var sb strings.Builder
		for _, f := range facts {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		messages = append(messages, llm.Message{
			Role: llm.RoleSystem,
			Content: "Things you know about this user from earlier conversations (shared across all channels and DMs):\n" +
				strings.TrimRight(sb.String(), "\n"),
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
		reply, err = b.handleToolCalls(userID, channelID, messages, replyMsg, mentioned)
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
	b.facts.Enqueue(userID, channelID)
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

// rewriteMentions replaces other users' <@id> tags with a readable form that
// still carries the exact id ("@Alec (id 123...)"), so small models can copy
// ids into tool arguments verbatim. Returns the rewritten text and the set of
// mentioned user ids (the bot itself excluded).
func (b *Bot) rewriteMentions(text string, m *discordgo.MessageCreate) (string, map[string]bool) {
	mentioned := make(map[string]bool)
	for _, u := range m.Mentions {
		if u.ID == b.dg.State.User.ID {
			continue
		}
		mentioned[u.ID] = true
		replacement := fmt.Sprintf("@%s (id %s)", u.Username, u.ID)
		text = strings.ReplaceAll(text, "<@!"+u.ID+">", replacement)
		text = strings.ReplaceAll(text, "<@"+u.ID+">", replacement)
	}
	return text, mentioned
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
