# Discord LLM Bot (Go)

A Discord chat bot written in Go. Users talk to the bot (DM or server mention)
and it answers using a local **LM Studio** server (OpenAI-compatible API)
running on the same machine — ideal for a Mac Mini setup.

## Features

- ✅ DM chat or `@mention` in a server
- ✅ Per-user conversation memory, **persisted to SQLite** across restarts (`/reset` to clear)
- ✅ **Context compaction** — old messages are summarized into a running summary in the background, so the bot keeps long-term memory without a growing prompt
- ✅ Typing indicator while the model generates
- ✅ Long replies are split across multiple Discord messages
- ✅ Config via `.env` file or environment variables
- ✅ Graceful shutdown on Ctrl-C

## Prerequisites

- Go 1.22+
- [LM Studio](https://lmstudio.ai) with a model loaded and the local server enabled
- A Discord bot application (see below)

## 1. Create the Discord bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications) → **New Application**.
2. **Bot** tab → **Reset Token** → copy the token.
3. Disable *Message Content Intent? No — enable it*: the **Privileged Gateway Intents** section → turn on **Message Content Intent** (required to read user messages).
4. **OAuth2 → URL Generator**: scope `bot`; bot permissions: `Send Messages`, `Read Message History`, `View Channels`.
5. Open the generated URL to invite the bot to your server.

> The bot works in DMs without any special server permissions.

## 2. Start the LM Studio server

1. Load a chat model in LM Studio.
2. Open the **Developer** (server) tab and press **Start Server** (default: `http://localhost:1234`).
3. Verify: `curl http://localhost:1234/v1/models`

## 3. Configure & run

```bash
cp .env.example .env
# edit .env: set DISCORD_TOKEN, and LMSTUDIO_MODEL to your loaded model id

go run .
```

Or build a binary:

```bash
go build -o bin/discord-bot .
./bin/discord-bot
```

## Configuration

| Variable         | Required | Default                  | Description                                  |
|------------------|----------|--------------------------|----------------------------------------------|
| `DISCORD_TOKEN`  | ✅       | —                        | Bot token (with `Bot ` prefix handled)        |
| `LMSTUDIO_URL`   | ✅       | —                        | e.g. `http://localhost:1234/v1`               |
| `LMSTUDIO_API_KEY` | ❌     | empty                    | Bearer key, if your server requires one       |
| `LMSTUDIO_MODEL` | ❌       | `local-model`            | Model id as shown by `/v1/models`             |
| `SYSTEM_PROMPT`  | ❌       | helpful-assistant default | The system prompt sent with every chat        |
| `MAX_HISTORY`    | ❌       | `20`                     | Recent messages kept per conversation as context |
| `HISTORY_DB`     | ❌       | `data/history.db`        | SQLite file where chat history is persisted   |
| `COMPACT_AT`     | ❌       | `40`                     | Compact a conversation's history once it has more stored messages than this |

Chat history is stored **per conversation** (your user + the channel it
happens in) in the SQLite file above and survives restarts. Your DM with
the bot, each server channel, and each thread keep independent memory,
so a discussion in one place isn't diluted by chatter elsewhere. Only the
most recent `MAX_HISTORY` of that conversation are sent to the model, plus
a running **summary** of everything older.

### Context compaction

When a user stores more than `COMPACT_AT` messages, a background worker
waits for them to go quiet (~45s) and then asks the model to fold the
oldest messages into that user's summary. The summarized rows are deleted
in the same transaction the summary is saved, so nothing is lost or
duplicated. Up to 12 newest messages are always left untouched. This
happens between turns and never blocks a reply.

Real environment variables always win over `.env` values.

## Using the bot

- **DM** the bot and just type.
- In a server: `@YourBot hey there`
- `/reset` — clears the bot's memory for *this* conversation only (your DM and other channels/threads keep theirs).

The bot answers using Discord's native reply feature, so every response
(including the 🤔 Thinking… placeholder) shows as *Replying to <you>*
directly under your message. Long replies are split across messages and
each chunk stays linked to your original message.

The bot's status line shows which model it is running (e.g.
`🧠 google/gemma-4-e2b`). If LM Studio isn't reachable when the bot
starts, it shows `🔌 Waiting for LM Studio…` instead.

## Running at startup on the Mac Mini (optional)

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

## Project layout

```
main.go               entrypoint: config, discord session, signal handling
internal/config/      env/.env loading and validation
internal/llm/         OpenAI-compatible chat client for LM Studio
internal/bot/         Discord handler, reply chunking
internal/store/       SQLite persistence for chat history + summaries
internal/bot/compactor.go  background summarization worker

## Tests

```bash
go test ./...
```
```
