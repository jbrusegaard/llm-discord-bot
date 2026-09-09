package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the bot.
type Config struct {
	// Discord
	Token string

	// LM Studio (OpenAI-compatible server)
	BaseURL      string // e.g. http://localhost:1234/v1
	APIKey       string // may be empty for local use
	Model        string // e.g. "local-model"
	MaxHistory   int    // number of recent messages kept per user (0 = default 20)
	SystemPrompt string

	// Persistence
	DBPath    string // SQLite file for chat history (default "data/history.db")
	CompactAt int    // compact when a user has more stored messages than this (0 = default 40)
}

// Load reads configuration from the process environment, optionally
// pre-populating it from the given dotenv-style file (path may be "").
// Lines in the file are KEY=VALUE; '#' starts a comment. Existing
// environment variables take precedence over file values.
func Load(dotenvPath string) (Config, error) {
	applyDotenv(dotenvPath)

	c := Config{
		Token:        os.Getenv("DISCORD_TOKEN"),
		BaseURL:      strings.TrimRight(os.Getenv("LMSTUDIO_URL"), "/"),
		APIKey:       os.Getenv("LMSTUDIO_API_KEY"),
		Model:        os.Getenv("LMSTUDIO_MODEL"),
		SystemPrompt: os.Getenv("SYSTEM_PROMPT"),
		DBPath:       os.Getenv("HISTORY_DB"),
		CompactAt:    atoiEnv("COMPACT_AT", 0),
	}

	if c.Token == "" {
		return c, fmt.Errorf("DISCORD_TOKEN is required")
	}
	if c.BaseURL == "" {
		return c, fmt.Errorf("LMSTUDIO_URL is required (e.g. http://localhost:1234/v1)")
	}
	if c.Model == "" {
		c.Model = "local-model"
	}
	if c.SystemPrompt == "" {
		c.SystemPrompt = "You are a helpful assistant on a Discord server. Keep replies concise and friendly."
	}
	if v := atoiEnv("MAX_HISTORY", 0); v > 0 {
		c.MaxHistory = v
	} else {
		c.MaxHistory = 20
	}
	if c.DBPath == "" {
		c.DBPath = "data/history.db"
	}
	return c, nil
}

// atoiEnv returns the integer value of the env var, or def if unset/invalid.
func atoiEnv(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

// applyDotenv parses a simple KEY=VALUE file and sets missing env vars.
func applyDotenv(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // optional file; missing file is fine
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
