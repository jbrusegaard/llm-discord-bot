package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"app/internal/llm"
	"app/internal/store"
)

const (
	// maxFactMessages bounds how many new messages one extraction covers.
	maxFactMessages = 24
	// maxFactsPerExtraction caps facts stored from a single LLM call, so a
	// chatty model can't flood the fact list in one pass.
	maxFactsPerExtraction = 10
	// factTimeout bounds one fact-extraction call.
	factTimeout = 3 * time.Minute
)

// factExtractor mines durable per-user facts ("my friend is Alec") out of
// conversations in the background and stores them globally for that user, so
// they apply in every channel and DM — not just where they were said.
type factExtractor struct {
	st   *store.Store
	llm  *llm.Client
	jobs chan conv
	mu   sync.Mutex
	seen map[conv]int64 // last message id extracted per conversation
}

func newFactExtractor(st *store.Store, client *llm.Client) *factExtractor {
	return &factExtractor{
		st:   st,
		llm:  client,
		jobs: make(chan conv, 64),
		seen: make(map[conv]int64),
	}
}

// Enqueue schedules a fact-extraction pass for the conversation. Non-blocking
// and idempotent; duplicate/pending conversations are skipped by the worker.
func (f *factExtractor) Enqueue(userID, channelID string) {
	select {
	case f.jobs <- conv{user: userID, channel: channelID}:
	default: // queue full; a later turn will re-enqueue
	}
}

// run pumps the job queue until ctx is cancelled.
func (f *factExtractor) run(ctx context.Context) {
	pending := make(map[conv]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case cv := <-f.jobs:
			if pending[cv] {
				continue
			}
			pending[cv] = true
			go func() {
				defer delete(pending, cv)
				if err := f.extract(ctx, cv); err != nil && ctx.Err() == nil {
					slog.Error("fact extraction failed", "user", cv.user, "channel", cv.channel, "err", err)
				}
			}()
		}
	}
}

// extract waits for the conversation to go quiet, then asks the model for new
// durable facts about its user from messages not yet processed. On failure the
// watermark is left unchanged so a later turn retries the same window (the
// store's dedupe makes re-extraction safe).
func (f *factExtractor) extract(ctx context.Context, cv conv) error {
	if err := waitForQuiet(ctx, f.st, cv); err != nil {
		return err
	}

	msgs, err := f.st.Since(cv.user, cv.channel, f.seenFor(cv), maxFactMessages)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil // nothing new since the last pass
	}

	existing, err := f.st.FactsForUser(cv.user)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithTimeout(ctx, factTimeout)
	facts, err := f.llm.ExtractFacts(sctx, existing, toMessages(msgs))
	cancel()
	if err != nil {
		return err
	}

	stored := 0
	for _, fact := range facts {
		if stored >= maxFactsPerExtraction {
			break
		}
		added, err := f.st.AddFact(cv.user, fact)
		if err != nil {
			return err
		}
		stored += boolToInt(added)
	}

	f.markSeen(cv, msgs)
	if stored > 0 {
		slog.Info("stored user facts", "user", cv.user, "channel", cv.channel, "facts", stored)
	}
	return nil
}

func (f *factExtractor) seenFor(cv conv) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[cv]
}

// markSeen advances the conversation's watermark past every message in msgs.
func (f *factExtractor) markSeen(cv conv, msgs []store.StoredMessage) {
	var maxID int64
	for _, m := range msgs {
		if m.ID > maxID {
			maxID = m.ID
		}
	}
	f.mu.Lock()
	f.seen[cv] = maxID
	f.mu.Unlock()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
