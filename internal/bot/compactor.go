package bot

import (
	"context"
	"log/slog"
	"time"

	"app/internal/llm"
	"app/internal/store"
)

const (
	// keepRecent messages always stay out of compaction so the live
	// conversation window is never summarized out from under the user.
	keepRecent = 12
	// maxPerRound bounds how many messages one summarization call covers.
	maxPerRound = 60
	// maxRounds bounds total LLM calls per compaction run.
	maxRounds = 3
	// quietPeriod is how long a user must be silent before we compact.
	quietPeriod = 45 * time.Second
	// quietWaitCap limits how long we wait for the user to go quiet.
	quietWaitCap = 2 * time.Minute
	// quietRetries × quietWaitCap ≈ max time a compaction can be deferred.
	quietRetries = 5
	// summarizeTimeout bounds one summarization call.
	summarizeTimeout = 5 * time.Minute
)

// conv identifies one conversation: a user in a specific channel (DM,
// server channel, or thread). Each keeps its own history and summary.
type conv struct {
	user    string
	channel string
}

// compactor summarizes old per-conversation history in the background so
// long conversations keep a compact "memory" without growing the prompt.
type compactor struct {
	st        *store.Store
	llm       *llm.Client
	compactAt int // compact when a conversation has more stored messages than this
	jobs      chan conv
}

func newCompactor(st *store.Store, client *llm.Client, compactAt int) *compactor {
	if compactAt <= 0 {
		compactAt = 40
	}
	return &compactor{
		st:        st,
		llm:       client,
		compactAt: compactAt,
		jobs:      make(chan conv, 64),
	}
}

// Enqueue schedules a compaction check for the conversation. Non-blocking
// and idempotent; duplicate/pending conversations are skipped by the worker.
func (c *compactor) Enqueue(userID, channelID string) {
	select {
	case c.jobs <- conv{user: userID, channel: channelID}:
	default: // queue full; a later turn will re-enqueue
	}
}

// run pumps the job queue until ctx is cancelled.
func (c *compactor) run(ctx context.Context) {
	pending := make(map[conv]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case cv := <-c.jobs:
			if pending[cv] {
				continue
			}
			pending[cv] = true
			go func() {
				defer delete(pending, cv)
				if err := c.compact(ctx, cv); err != nil && ctx.Err() == nil {
					slog.Error("compaction failed", "user", cv.user, "channel", cv.channel, "err", err)
				}
			}()
		}
	}
}

// compact waits for the user to go quiet in this conversation, then folds
// its oldest messages into a running summary until the history is back
// under the threshold (or work is exhausted).
func (c *compactor) compact(ctx context.Context, cv conv) error {
	if err := c.waitForQuiet(ctx, cv); err != nil {
		return err
	}

	for round := 0; round < maxRounds; round++ {
		count, _, err := c.st.Stats(cv.user, cv.channel)
		if err != nil {
			return err
		}
		if count <= c.compactAt {
			return nil // back under the threshold
		}

		msgs, err := c.st.Compactable(cv.user, cv.channel, keepRecent, maxPerRound)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil // nothing left that is safe to compact
		}

		existing, err := c.st.GetSummary(cv.user, cv.channel)
		if err != nil {
			return err
		}

		sctx, cancel := context.WithTimeout(ctx, summarizeTimeout)
		summary, err := c.llm.Summarize(sctx, existing, toMessages(msgs))
		cancel()
		if err != nil {
			return err
		}

		ids := make([]int64, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		if err := c.st.CommitCompaction(cv.user, cv.channel, summary, ids); err != nil {
			return err
		}
		slog.Info("compacted history", "user", cv.user, "channel", cv.channel, "messages", len(msgs), "round", round+1)
	}
	return nil
}

// waitForQuiet returns once the conversation's most recent message is at
// least quietPeriod old, or gives up after quietRetries waits.
func (c *compactor) waitForQuiet(ctx context.Context, cv conv) error {
	for i := 0; i < quietRetries; i++ {
		_, last, err := c.st.Stats(cv.user, cv.channel)
		if err != nil {
			return err
		}
		if last.IsZero() {
			return nil // no messages at all
		}
		wait := quietPeriod - time.Since(last)
		if wait <= 0 {
			return nil
		}
		if wait > quietWaitCap {
			wait = quietWaitCap
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	// Still active after the cap; skip this round rather than stall the queue.
	slog.Debug("compaction deferred, user still active", "user", cv.user, "channel", cv.channel)
	return nil
}

func toMessages(msgs []store.StoredMessage) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = llm.Message{Role: m.Role, Content: m.Content}
	}
	return out
}
