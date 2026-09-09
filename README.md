# LLM Discord Bot

> **Proof of concept** — a fun side project exploring what it's like to chat with a bot powered by local AI ([LM Studio](https://lmstudio.ai)). Expect rough edges.

A Discord chat bot written in Go that answers using a local LM Studio server (OpenAI-compatible API). Built for an always-on Mac Mini: private, free, and no cloud API bills.

## Features

- Chat via **DM** or `@mention` in a server
- **Per-conversation memory**, persisted to SQLite across restarts — your DM, each channel, and each thread keep independent context (`/reset` clears the current conversation)
- **Context compaction** — old messages are folded into a running summary in the background, so long conversations keep "memory" without an ever-growing prompt
- Replies use Discord's native reply feature (including the 🤔 Thinking… placeholder), so answers stay attached to your message
- Typing indicator while the model generates; long replies split across multiple messages
- Bot status shows which model is loaded (`🧠 <model>`)
- Config via `.env` or environment variables; graceful shutdown on Ctrl-C

## Prerequisites

- Go 1.27+
- [LM Studio](https://lmstudio.ai) with a chat model loaded and the local server enabled
- A Discord bot application (created below)

## 1. Create the Discord bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications) → **New Application**.
2. **Bot** tab → **Reset Token** → copy the token.
3. Under **Privileged Gateway Intents**, enable **Message Content Intent** (required for the bot to read user messages).
4. **OAuth2 → URL Generator**: scope `bot`; permissions: `Send Messages`, `Read Message History`, `View Channels`.
5. Open the generated URL to invite the bot to your server.

> The bot works in DMs without any special server permissions.

## 2. Start the LM Studio server

1. Load a chat model in LM Studio.
2. Open the **Developer** (server) tab and press **Start Server** (default: `http://localhost:1234`).
3. Verify: `curl http://localhost:1234/v1/models` — note the exact model id; you'll need it in `.env`.

## 3. Configure & run

```bash
cp .env.example .env
# edit .env: set DISCORD_TOKEN and LMSTUDIO_MODEL to your loaded model id

make run        # or: go run .
```

Or build a shippable static binary:

```bash
make build      # → bin/discord-bot (CGO-free, stripped)
./bin/discord-bot
```

Cross-compile with `make build GOOS=linux GOARCH=amd64` if you ever want to move off the Mac.

## Configuration

| Variable           | Required | Default                  | Description                                  |
|--------------------|----------|--------------------------|----------------------------------------------|
| `DISCORD_TOKEN`    | ✅       | —                        | Bot token (with `Bot ` prefix handled)        |
| `LMSTUDIO_URL`     | ✅       | —                        | e.g. `http://localhost:1234/v1`               |
| `LMSTUDIO_API_KEY` | ❌       | empty                    | Bearer key, if your server requires one       |
| `LMSTUDIO_MODEL`   | ❌       | `local-model`            | Model id as shown by `/v1/models`             |
| `SYSTEM_PROMPT`    | ❌       | helpful-assistant default | The system prompt sent with every chat        |
| `MAX_HISTORY`      | ❌       | `20`                     | Recent messages kept per conversation as context |
| `HISTORY_DB`       | ❌       | `data/history.db`        | SQLite file where chat history is persisted   |
| `COMPACT_AT`       | ❌       | `40`                     | Compact a conversation's history once it has more stored messages than this |

Chat history is stored **per conversation** (your user + the channel it happens in) and survives restarts. Your DM with the bot, each server channel, and each thread keep independent memory, so a discussion in one place isn't diluted by chatter elsewhere. Only the most recent `MAX_HISTORY` of that conversation are sent to the model, plus a running **summary** of everything older.

### Context compaction

When a conversation stores more than `COMPACT_AT` messages, a background worker waits for it to go quiet (~45s) and then asks the model to fold the oldest messages into that conversation's summary. The summarized rows are deleted in the same transaction the summary is saved, so nothing is lost or duplicated. Up to 12 newest messages are always left untouched. This happens between turns and never blocks a reply.

Real environment variables always win over `.env` values.

## Using the bot

- **DM** the bot and just type.
- In a server: `@YourBot hey there`
- `/reset` — clears the bot's memory for *this* conversation only (your DM and other channels/threads keep theirs).

The bot answers using Discord's native reply feature, so every response shows as *Replying to <you>* directly under your message. Long replies are split across messages and each chunk stays linked to your original message.

The bot's status line shows which model it is running (e.g. `🧠 google/gemma-4-e2b`). If LM Studio isn't reachable when the bot starts, it shows `🔌 Waiting for LM Studio…` instead.

## Running at startup on macOS (optional)

Create a LaunchAgent so the bot survives reboots and logouts:

```bash
# Create ~/Library/LaunchAgents/com.jbrusegaard.discord-bot.plist
cat > ~/Library/LaunchAgents/com.jbrusegaard.discord-bot.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.jbrusegaard.discord-bot</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/discord-bot/bin/discord-bot</string>
  </array>
  <key>WorkingDirectory</key><string>/path/to/discord-bot</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/tmp/discord-bot.err.log</string>
  <key>StandardOutPath</key><string>/tmp/discord-bot.out.log</string>
</dict></plist>
EOF

launchctl load ~/Library/LaunchAgents/com.jbrusegaard.discord-bot.plist
```

Replace both `/path/to/discord-bot` values with the real path to this repository.

## Project layout

```
main.go                 entrypoint: config, discord session, signal handling
internal/config/        env/.env loading and validation
internal/llm/           OpenAI-compatible chat client for LM Studio
internal/bot/           Discord handler, reply chunking, presence
internal/bot/compactor.go  background summarization worker
internal/store/         SQLite persistence for chat history + summaries
Makefile                build/test/tidy targets (static binary at bin/)
.env.example            configuration template
data/                   SQLite database (created at runtime, gitignored)
```

## Tests

```bash
go test ./...
```

## License

[MIT](LICENSE.md) © jbrusegaard
