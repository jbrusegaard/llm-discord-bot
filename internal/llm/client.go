package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries the result of a tool call back to the model.
	RoleTool Role = "tool"
)

// Message is a single chat message. ToolCalls/ToolCallID are only used for
// function calling (see ChatWithTools).
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCalls, set on assistant messages when the model wants to call tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-result message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is one function invocation requested by the model. Arguments is
// a JSON-encoded object, per the OpenAI spec (LM Studio follows it too).
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool describes one callable function in the OpenAI tools schema.
type Tool struct {
	Type     string   `json:"type"` // "function"
	Function Function `json:"function"`
}

// Function names a tool and gives its JSON-schema parameters.
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Client talks to an OpenAI-compatible /v1/chat/completions endpoint
// (LM Studio, llama.cpp server, etc.).
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New creates a client for the given base URL (e.g. http://localhost:1234/v1).
// apiKey may be empty for local-only use.
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Model returns the configured model identifier.
func (c *Client) Model() string { return c.model }

// ChatRequest is the request body for chat completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
	Tools       []Tool    `json:"tools,omitempty"`
}

// chatResponse is the response envelope.
type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Chat sends the conversation to the model and returns the assistant reply.
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	msg, err := c.do(ctx, ChatRequest{Model: c.model, Messages: messages, Temperature: 0.7})
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

// ChatWithTools is like Chat but offers the model extra tools (function
// calling). The returned message may carry ToolCalls instead of Content;
// execute them and send tool-result messages back for a follow-up reply.
func (c *Client) ChatWithTools(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return c.do(ctx, ChatRequest{Model: c.model, Messages: messages, Temperature: 0.7, Tools: tools})
}

// do performs one /chat/completions round trip and returns the assistant message.
func (c *Client) do(ctx context.Context, body ChatRequest) (Message, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Message{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("request failed (is LM Studio running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return Message{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var cr chatResponse
		if json.Unmarshal(data, &cr) == nil && cr.Error != nil {
			return Message{}, fmt.Errorf("llm error (%d): %s", resp.StatusCode, cr.Error.Message)
		}
		return Message{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return Message{}, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Message{}, fmt.Errorf("model returned no choices")
	}
	return cr.Choices[0].Message, nil
}

// Summarize condenses conversation messages into a short running summary,
// merging them into any existing summary. Used for context compaction.
func (c *Client) Summarize(ctx context.Context, existing string, msgs []Message) (string, error) {
	var convo strings.Builder
	for _, m := range msgs {
		convo.WriteString(string(m.Role) + ": " + m.Content + "\n")
	}
	var prompt strings.Builder
	prompt.WriteString("You maintain a running summary of one user's conversation with an assistant on Discord. ")
	prompt.WriteString("Merge the new conversation below into the previous summary and output ONLY the updated summary. ")
	prompt.WriteString("Keep it under 150 words, factual, in plain text. ")
	prompt.WriteString("Preserve decisions, preferences, names, plans, and open questions; drop small talk.\n\n")
	if existing != "" {
		prompt.WriteString("Previous summary:\n" + existing + "\n\n")
	}
	prompt.WriteString("New conversation to fold in:\n" + convo.String())

	reply, err := c.Chat(ctx, []Message{
		{Role: RoleUser, Content: prompt.String()},
	})
	if err != nil {
		return "", err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return "", fmt.Errorf("empty summary")
	}
	return reply, nil
}

// ExtractFacts pulls durable facts about the user out of a conversation so
// they can be remembered across all channels and DMs. existing lists facts
// already stored, so the model is asked not to repeat them.
func (c *Client) ExtractFacts(ctx context.Context, existing []string, msgs []Message) ([]string, error) {
	var convo strings.Builder
	for _, m := range msgs {
		convo.WriteString(string(m.Role) + ": " + m.Content + "\n")
	}
	known := "(none yet)"
	if len(existing) > 0 {
		var sb strings.Builder
		for _, f := range existing {
			sb.WriteString("- " + f + "\n")
		}
		known = strings.TrimRight(sb.String(), "\n")
	}

	prompt := "You maintain a list of durable facts about one Discord user so the bot can remember them in every channel and DM.\n\n" +
		"Facts already stored:\n" + known + "\n\n" +
		"New conversation between this user and the assistant:\n" + convo.String() + "\n" +
		"List any NEW durable facts about THIS USER worth remembering long-term (people they know, preferences, job, health, pets, plans). " +
		"Do not repeat stored facts. Ignore small talk and details only relevant to this one channel. " +
		"One fact per line in plain text, under 20 words each, phrased as a standalone statement (e.g. \"The user's friend is named Alec\"). " +
		"If there are no new facts, output exactly: NONE"

	reply, err := c.Chat(ctx, []Message{{Role: RoleUser, Content: prompt}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(reply, "\n") {
		line = stripListMarker(line)
		if line == "" || strings.EqualFold(line, "NONE") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// stripListMarker trims leading bullets ("- ", "* ") or numbering ("1. ",
// "2) ") from a model-output line.
func stripListMarker(line string) string {
	line = strings.TrimSpace(line)
	for _, p := range []string{"- ", "* ", "• ", "# "} {
		if strings.HasPrefix(line, p) {
			return strings.TrimSpace(strings.TrimPrefix(line, p))
		}
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && (line[i] == '.' || line[i] == ')') {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}

// Ping checks the /v1/models endpoint; handy for startup diagnostics.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ping %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping %s/models: status %d", c.baseURL, resp.StatusCode)
	}
	return nil
}
