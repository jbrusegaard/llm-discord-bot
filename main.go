package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"app/internal/bot"
	"app/internal/config"
	"app/internal/store"
)

// Set at build time via -ldflags (see Makefile).
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Printf("discord-bot %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// App-lifetime context, cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Config comes from the environment; .env in the working directory is
	// picked up automatically if present.
	cfg, err := config.Load(".env")
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	st, err := store.New(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open history database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if _, err := os.Stat(cfg.DBPath); err == nil {
		slog.Info("loaded chat history database", "path", cfg.DBPath)
	}

	dg, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		slog.Error("failed to create discord session", "err", err)
		st.Close()
		os.Exit(1)
	}
	defer dg.Close()

	h := bot.New(cfg, dg, st)
	h.StartCompactor(ctx)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	lmErr := h.PingLM(pingCtx)
	cancel()
	if lmErr != nil {
		slog.Warn("LM Studio not reachable at startup (will retry on demand)", "url", cfg.BaseURL, "err", lmErr)
	} else {
		slog.Info("connected to LM Studio", "url", cfg.BaseURL, "model", cfg.Model)
	}

	dg.AddHandler(h.HandleMessageCreate)

	if err := dg.Open(); err != nil {
		slog.Error("failed to open discord gateway", "err", err)
		os.Exit(1)
	}
	if err := h.UpdatePresence(lmErr == nil); err != nil {
		slog.Warn("failed to set bot presence", "err", err)
	}
	slog.Info("discord bot online", "user", dg.State.User.Username)

	<-ctx.Done()
	slog.Info("shutting down")
}
