package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"app/internal/llm"
)

// reminderPollInterval is how often the worker checks for due reminders.
const reminderPollInterval = 20 * time.Second

// reminderToolNote is appended to the system prompt so models that are shy
// about tool calling still use create_reminder when asked.
const reminderToolNote = "When the user asks to be reminded of something later (e.g. 'remind me in 10 minutes to stretch'), call the create_reminder tool, then briefly confirm."

// createReminderTool is offered to the model on every chat turn so it can
// schedule reminders ("remind me in 10 minutes to stretch").
var createReminderTool = llm.Tool{
	Type: "function",
	Function: llm.Function{
		Name:        "create_reminder",
		Description: "Schedule a reminder that pings the user later. Use it when the user asks to be reminded of something (e.g. 'remind me in 10 minutes to stretch', 'ping me at 3pm about the meeting'). Provide either delay_minutes for relative times or due_at for specific clock times, plus a short message describing what to remind them about.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"delay_minutes": map[string]any{
					"type":        "number",
					"description": "Minutes from now, for relative times like 'in 10 minutes' or 'in 2 hours'. Omit when using due_at.",
				},
				"due_at": map[string]any{
					"type":        "string",
					"description": "Absolute time in RFC3339 format with the bot's local timezone offset (e.g. 2026-09-08T15:00:00-05:00), when the user names a clock time like 'at 3pm'. Omit when using delay_minutes.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "Short reminder text, e.g. 'stretch your legs' or 'meeting with Sam'.",
				},
			},
			"required": []string{"message"},
		},
	},
}

// StartReminderWorker launches the background worker that delivers due
// reminders; it stops when ctx is cancelled.
func (b *Bot) StartReminderWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(reminderPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.deliverDueReminders()
			}
		}
	}()
}

// deliverDueReminders sends every reminder whose time has come. A failed
// send keeps the row so it retries on the next tick; a missing channel
// (deleted thread) drops it.
func (b *Bot) deliverDueReminders() {
	due, err := b.hist.DueReminders(time.Now())
	if err != nil {
		slog.Error("failed to load due reminders", "err", err)
		return
	}
	for _, r := range due {
		channel, err := b.dg.Channel(r.ChannelID)
		if err != nil {
			slog.Warn("reminder channel missing; dropping reminder", "id", r.ID, "channel", r.ChannelID, "err", err)
			if derr := b.hist.DeleteReminder(r.ID); derr != nil {
				slog.Error("failed to drop stale reminder", "id", r.ID, "err", derr)
			}
			continue
		}
		text := "⏰ " + r.Message
		if channel.Type != discordgo.ChannelTypeDM {
			text = "<@" + r.UserID + "> " + text
		}
		if _, err := b.dg.ChannelMessageSend(channel.ID, text); err != nil {
			slog.Error("failed to send reminder; will retry", "id", r.ID, "channel", r.ChannelID, "err", err)
			continue
		}
		if err := b.hist.DeleteReminder(r.ID); err != nil {
			slog.Error("failed to mark reminder delivered", "id", r.ID, "err", err)
		} else {
			slog.Info("delivered reminder", "id", r.ID, "user", r.UserID, "channel", r.ChannelID)
		}
	}
}

// handleToolCalls executes the model's tool calls (currently only
// create_reminder), then asks the model for a short natural confirmation.
func (b *Bot) handleToolCalls(userID, channelID string, messages []llm.Message, reply llm.Message) (string, error) {
	followUp := make([]llm.Message, 0, len(messages)+len(reply.ToolCalls)+1)
	followUp = append(followUp, messages...)
	followUp = append(followUp, reply)

	var fallback []string // canned confirmations if the model's fails
	for _, tc := range reply.ToolCalls {
		if tc.Function.Name != "create_reminder" {
			followUp = append(followUp, toolResult(tc.ID, fmt.Sprintf("Unknown tool %q; ignore it.", tc.Function.Name)))
			continue
		}
		var args struct {
			DelayMinutes *float64 `json:"delay_minutes"`
			DueAt        string   `json:"due_at"`
			Message      string   `json:"message"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			followUp = append(followUp, toolResult(tc.ID, fmt.Sprintf("Error: arguments were not valid JSON (%v). Call again.", err)))
			continue
		}
		msg := strings.TrimSpace(args.Message)
		if msg == "" {
			followUp = append(followUp, toolResult(tc.ID, "Error: 'message' is empty. Call again with a short reminder text."))
			continue
		}

		now := time.Now()
		var due time.Time
		switch {
		case args.DelayMinutes != nil && *args.DelayMinutes > 0:
			due = now.Add(time.Duration(*args.DelayMinutes * float64(time.Minute)))
		case strings.TrimSpace(args.DueAt) != "":
			t, perr := parseDueTime(strings.TrimSpace(args.DueAt))
			if perr != nil {
				followUp = append(followUp, toolResult(tc.ID, fmt.Sprintf("Error: could not understand due_at %q (%v). Use RFC3339 like 2026-09-08T15:00:00-05:00.", args.DueAt, perr)))
				continue
			}
			due = t
		default:
			followUp = append(followUp, toolResult(tc.ID, "Error: provide delay_minutes (relative) or due_at (absolute). Call again."))
			continue
		}

		id, err := b.hist.AddReminder(userID, channelID, msg, due)
		if err != nil {
			followUp = append(followUp, toolResult(tc.ID, fmt.Sprintf("Error: failed to store reminder (%v).", err)))
			continue
		}
		slog.Info("reminder scheduled", "id", id, "user", userID, "channel", channelID, "due", due.Format(time.RFC3339))
		followUp = append(followUp, toolResult(tc.ID, fmt.Sprintf("OK: reminder #%d scheduled (due %s).", id, due.Format(time.RFC3339))))
		fallback = append(fallback, fmt.Sprintf("⏰ Got it — I'll remind you about “%s” %s.", msg, whenText(due)))
	}

	followUp = append(followUp, llm.Message{
		Role:    llm.RoleUser,
		Content: "Briefly confirm the reminder(s) you just scheduled (one or two sentences). Don't mention tools or JSON.",
	})
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	if confirm, err := b.llm.Chat(ctx, followUp); err == nil {
		confirm = strings.TrimSpace(confirm)
		if confirm != "" {
			return confirm, nil
		}
	} else {
		slog.Warn("reminder confirmation call failed; using canned text", "err", err)
	}
	if len(fallback) == 0 {
		return "", fmt.Errorf("model called tools but none succeeded")
	}
	return strings.Join(fallback, "\n"), nil
}

// toolResult builds a tool-role message answering one tool call.
func toolResult(callID, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: content}
}

// parseDueTime accepts RFC3339 plus a few forgiving formats; bare times are
// assumed to be today (or tomorrow if already past).
func parseDueTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		now := time.Now()
		today := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if !today.After(now) {
			today = today.AddDate(0, 0, 1) // already past → tomorrow
		}
		return today, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized format")
}

// whenText renders a due time for humans: relative + absolute.
func whenText(due time.Time) string {
	now := time.Now()
	var rel string
	if d := due.Sub(now); d > 0 {
		rel = "in " + humanDuration(d) + ", "
	}
	if due.Year() == now.Year() && due.YearDay() == now.YearDay() {
		return rel + "at " + due.Format("3:04 PM")
	}
	return rel + "on " + due.Format("Monday, January 2 at 3:04 PM")
}

// humanDuration renders a duration as e.g. "9m", "2h 5m", or "3d".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
