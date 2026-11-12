package cohere

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// A real response from the v2 Chat API, trimmed of ids.
const toolCallResponse = `{
  "message": {
    "role": "assistant",
    "tool_plan": "I will close the issue.",
    "tool_calls": [{
      "id": "close_issue_1", "type": "function",
      "function": {"name": "close_issue", "arguments": "{\"repo\":\"acme/api\",\"number\":7,\"comment\":\"duplicate of 3\"}"}
    }]
  },
  "finish_reason": "TOOL_CALL",
  "usage": {"billed_units": {"input_tokens": 67, "output_tokens": 53}, "tokens": {"input_tokens": 1508, "output_tokens": 87}}
}`

func server(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(Config{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var request = provider.Request{
	Model:  "command-a-03-2025",
	System: "You manage issues.",
	Prompt: "Close acme/api issue 7",
	Seed:   1<<40 + 5,
	Tools: []spec.Tool{{
		Name: "close_issue", Description: "Close an issue.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"number": map[string]any{"type": "integer"}}},
	}},
	Oracle: &provider.ToolCall{Name: "secret"},
}

func TestRequestShape(t *testing.T) {
	var got map[string]any
	p := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat" || r.Method != http.MethodPost {
			t.Errorf("got %s %s, want POST /v2/chat", r.Method, r.URL.Path)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer test-key" {
			t.Errorf("Authorization = %q", a)
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "secret") {
			t.Error("the oracle must never be sent to a real model")
		}
		json.Unmarshal(body, &got)
		io.WriteString(w, toolCallResponse)
	})
	if _, err := p.Complete(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != request.Prompt {
		t.Errorf("messages = %v", msgs)
	}
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if got["tools"].([]any)[0].(map[string]any)["type"] != "function" || fn["name"] != "close_issue" || fn["parameters"] == nil {
		t.Errorf("tools = %v", got["tools"])
	}
	if seed := got["seed"].(float64); seed < 0 || seed >= 1<<31 {
		t.Errorf("seed %v out of int32 range", seed)
	}
	if got["model"] != "command-a-03-2025" {
		t.Errorf("model = %v", got["model"])
	}
}

func TestParsesToolCall(t *testing.T) {
	p := server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, toolCallResponse) })
	resp, err := p.Complete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "close_issue" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	args := resp.ToolCalls[0].Args
	if args["repo"] != "acme/api" || args["number"] != 7.0 {
		t.Errorf("args = %v", args)
	}
	if resp.Usage.InputTokens != 67 || resp.Usage.OutputTokens != 53 {
		t.Errorf("usage should be billed units, got %+v", resp.Usage)
	}
}

func TestParsesTextAnswer(t *testing.T) {
	p := server(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"role":"assistant","content":[{"type":"text","text":"Glad to help!"}]},"finish_reason":"COMPLETE"}`)
	})
	resp, err := p.Complete(context.Background(), request)
	if err != nil || len(resp.ToolCalls) != 0 || resp.Text != "Glad to help!" {
		t.Fatalf("got %+v, %v", resp, err)
	}
}

func TestMalformedArgumentsAreAModelError(t *testing.T) {
	p := server(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"tool_calls":[{"function":{"name":"close_issue","arguments":"{not json"}}]}}`)
	})
	resp, err := p.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("bad arguments are the model's fault, not a transport error: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Args != nil {
		t.Fatalf("want the call kept with no args, got %+v", resp.ToolCalls)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		status    int
		header    string
		retryable bool
		after     time.Duration
	}{
		{http.StatusTooManyRequests, "7", true, 7 * time.Second},
		{http.StatusTooManyRequests, "", true, 0},
		{http.StatusServiceUnavailable, "", true, 0},
		{http.StatusBadRequest, "", false, 0},
		{http.StatusUnauthorized, "", false, 0},
	}
	for _, c := range cases {
		p := server(t, func(w http.ResponseWriter, r *http.Request) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.WriteHeader(c.status)
			io.WriteString(w, `{"message":"something went wrong"}`)
		})
		_, err := p.Complete(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "something went wrong") {
			t.Fatalf("%d: want error with API message, got %v", c.status, err)
		}
		if provider.IsRetryable(err) != c.retryable || provider.RetryAfter(err) != c.after {
			t.Errorf("%d: retryable=%v after=%v, want %v %v", c.status,
				provider.IsRetryable(err), provider.RetryAfter(err), c.retryable, c.after)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	release := make(chan struct{})
	p := server(t, func(w http.ResponseWriter, r *http.Request) {
		select { // hang until the client gives up or the test ends
		case <-r.Context().Done():
		case <-release:
		}
	})
	// Cleanups run last-in first-out: free the handler before the server
	// closes, since Close waits for running handlers.
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Complete(ctx, request); err != context.DeadlineExceeded {
		t.Fatalf("want context.DeadlineExceeded so the runner sees a timeout, got %v", err)
	}
}

func TestNeedsKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing key should be an error")
	}
}
