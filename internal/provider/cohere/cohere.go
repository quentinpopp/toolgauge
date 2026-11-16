// Package cohere calls Cohere's Chat API (v2) with tools.
//
// Request and response shapes follow https://docs.cohere.com/v2/reference/chat
// and were checked against the live API: tool calls come back in
// message.tool_calls with arguments as a JSON string.
package cohere

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quentinpopp/toolgauge/internal/provider"
)

// DefaultBaseURL is the public Cohere API.
const DefaultBaseURL = "https://api.cohere.com"

// DefaultModel is Cohere's general-purpose model with tool use.
const DefaultModel = "command-a-03-2025"

// Config configures the provider.
type Config struct {
	APIKey  string
	BaseURL string // default DefaultBaseURL; tests point it at a local server
	Client  *http.Client
}

// Provider is the Cohere provider.
type Provider struct {
	key    string
	url    string
	client *http.Client
}

// New returns a Cohere provider. The API key is required.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("cohere: API key is required (set COHERE_API_KEY)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Client == nil {
		// No client timeout: the runner sets a deadline per attempt.
		cfg.Client = &http.Client{}
	}
	return &Provider{key: cfg.APIKey, url: strings.TrimRight(cfg.BaseURL, "/") + "/v2/chat", client: cfg.Client}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "cohere" }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type function struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitempty"`
	Seed     int64     `json:"seed"`
}

type chatResponse struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Usage struct {
		BilledUnits struct {
			InputTokens  float64 `json:"input_tokens"`
			OutputTokens float64 `json:"output_tokens"`
		} `json:"billed_units"`
	} `json:"usage"`
}

// Complete implements provider.Provider.
func (p *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if req.Model == "" {
		req.Model = DefaultModel
	}
	body := chatRequest{
		Model: req.Model,
		// The API takes a signed integer; keep the seed in a safe range.
		Seed: int64(req.Seed % (1 << 31)),
	}
	if req.System != "" {
		body.Messages = append(body.Messages, message{Role: "system", Content: req.System})
	}
	body.Messages = append(body.Messages, message{Role: "user", Content: req.Prompt})
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, tool{Type: "function", Function: function{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, err
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return provider.Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+p.key)

	resp, err := p.client.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return provider.Response{}, ctx.Err()
		}
		// Connection resets and DNS hiccups are worth another try.
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) {
			return provider.Response{}, provider.Retryable(err)
		}
		return provider.Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return provider.Response{}, provider.Retryable(fmt.Errorf("cohere: read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return provider.Response{}, statusError(resp, data)
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return provider.Response{}, fmt.Errorf("cohere: decode response: %w", err)
	}
	out := provider.Response{Usage: provider.Usage{
		InputTokens:  int(cr.Usage.BilledUnits.InputTokens),
		OutputTokens: int(cr.Usage.BilledUnits.OutputTokens),
	}}
	for _, c := range cr.Message.Content {
		if c.Type == "text" {
			out.Text += c.Text
		}
	}
	for _, tc := range cr.Message.ToolCalls {
		call := provider.ToolCall{Name: tc.Function.Name}
		if tc.Function.Arguments != "" {
			// Malformed arguments are the model's mistake, not a transport
			// error: keep the call so the grader reports bad_args.
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &call.Args); err != nil {
				call.Args = nil
			}
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	return out, nil
}

// statusError turns a non-200 response into an error, marking rate limits
// and server errors as retryable.
func statusError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	err := fmt.Errorf("cohere: HTTP %d: %s", resp.StatusCode, msg)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return provider.RetryableAfter(err, retryAfter(resp.Header.Get("Retry-After")))
	case resp.StatusCode >= 500:
		return provider.Retryable(err)
	default:
		return err
	}
}

// retryAfter parses a Retry-After header given in seconds or as an HTTP date.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
