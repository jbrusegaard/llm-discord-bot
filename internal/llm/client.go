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
)

// Message is a single chat message.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
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
	body := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0.7,
		Stream:      false,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed (is LM Studio running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var cr chatResponse
		if json.Unmarshal(data, &cr) == nil && cr.Error != nil {
			return "", fmt.Errorf("llm error (%d): %s", resp.StatusCode, cr.Error.Message)
		}
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("model returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
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
