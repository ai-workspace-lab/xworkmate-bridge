package geminiadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

type stubClient struct {
	initResult initializeResult
	initErr    error
	callResult map[string]any
	callErr    error
	lastMethod string
	lastParams map[string]any
}

func (s *stubClient) Initialize() (initializeResult, error) {
	return s.initResult, s.initErr
}

func (s *stubClient) Call(method string, params map[string]any) (map[string]any, error) {
	s.lastMethod = method
	s.lastParams = params
	return s.callResult, s.callErr
}

func (s *stubClient) Close() error {
	return nil
}

func TestHandleCapabilitiesSynthesizesProviderResponse(t *testing.T) {
	server := NewServer(&stubClient{
		initResult: initializeResult{
			ProtocolVersion: 1,
			AuthMethods: []map[string]any{
				{"id": "gemini-api-key"},
			},
			AgentCapabilities: map[string]any{
				"mcpCapabilities": map[string]any{"http": true},
			},
		},
	})

	result := server.handleRequest(shared.RPCRequest{
		Method: "acp.capabilities",
		Params: map[string]any{},
	})
	if got := result["singleAgent"]; got != true {
		t.Fatalf("expected singleAgent true, got %#v", result)
	}
	providers, _ := result["providers"].([]string)
	if len(providers) != 1 || providers[0] != "gemini" {
		t.Fatalf("expected gemini provider, got %#v", result)
	}
}

func TestHandleRPCSessionStartReturnsUpstreamResult(t *testing.T) {
	stub := &stubClient{
		initResult: initializeResult{ProtocolVersion: 1},
		callResult: map[string]any{
			"result": map[string]any{
				"success": true,
				"output":  "hello",
			},
		},
	}
	server := NewServer(stub)
	server.upstreamMethod = "session.start"

	body, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "session.start",
		Params: map[string]any{
			"sessionId":  "s1",
			"taskPrompt": "hello",
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var envelope map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result := envelope["result"].(map[string]any)
	if got := result["output"]; got != "hello" {
		t.Fatalf("expected output hello, got %#v", result)
	}
	if stub.lastMethod != "session.start" {
		t.Fatalf("expected upstream method session.start, got %q", stub.lastMethod)
	}
}

func TestHandleSessionStartFallsBackToPromptRunner(t *testing.T) {
	stub := &stubClient{
		initResult: initializeResult{ProtocolVersion: 1},
	}
	server := NewServer(stub)
	server.sessionRunner = func(ctx context.Context, model, prompt, workingDirectory string) (string, error) {
		if model != "gemini-2.5-pro" {
			t.Fatalf("expected model gemini-2.5-pro, got %q", model)
		}
		if workingDirectory != "/tmp/demo" {
			t.Fatalf("expected workingDirectory /tmp/demo, got %q", workingDirectory)
		}
		expectedPrompt := "## User Turn 1\nReply with exactly pong"
		if prompt != expectedPrompt {
			t.Fatalf("unexpected prompt %q", prompt)
		}
		return "pong", nil
	}

	result := server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":        "s1",
			"taskPrompt":       "Reply with exactly pong",
			"model":            "gemini-2.5-pro",
			"workingDirectory": "/tmp/demo",
		},
	})
	if got := result["output"]; got != "pong" {
		t.Fatalf("expected output pong, got %#v", result)
	}
	if got := result["upstreamMethod"]; got != "prompt" {
		t.Fatalf("expected prompt upstream method, got %#v", result)
	}
}

func TestHandleSessionMessageReusesAdapterLocalHistory(t *testing.T) {
	stub := &stubClient{
		initResult: initializeResult{ProtocolVersion: 1},
	}
	server := NewServer(stub)
	callCount := 0
	server.sessionRunner = func(ctx context.Context, model, prompt, workingDirectory string) (string, error) {
		callCount++
		if callCount == 1 {
			expected := "## User Turn 1\nFirst turn"
			if prompt != expected {
				t.Fatalf("unexpected first prompt %q", prompt)
			}
			return "first-reply", nil
		}
		expected := "## User Turn 1\nFirst turn\n\n## User Turn 2\nSecond turn"
		if prompt != expected {
			t.Fatalf("unexpected second prompt %q", prompt)
		}
		if workingDirectory != "/tmp/demo" {
			t.Fatalf("expected inherited workingDirectory, got %q", workingDirectory)
		}
		if model != "gemini-2.5-flash" {
			t.Fatalf("expected inherited model, got %q", model)
		}
		return "second-reply", nil
	}

	server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":        "s1",
			"taskPrompt":       "First turn",
			"model":            "gemini-2.5-flash",
			"workingDirectory": "/tmp/demo",
		},
	})
	result := server.handleRequest(shared.RPCRequest{
		Method: "session.message",
		Params: map[string]any{
			"sessionId":  "s1",
			"taskPrompt": "Second turn",
		},
	})
	if got := result["output"]; got != "second-reply" {
		t.Fatalf("expected second reply, got %#v", result)
	}
}

func TestSessionCloseDropsAdapterLocalState(t *testing.T) {
	server := NewServer(&stubClient{initResult: initializeResult{ProtocolVersion: 1}})
	server.sessionRunner = func(ctx context.Context, model, prompt, workingDirectory string) (string, error) {
		return "ok", nil
	}
	server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":  "s1",
			"taskPrompt": "hello",
		},
	})
	result := server.handleRequest(shared.RPCRequest{
		Method: "session.close",
		Params: map[string]any{
			"sessionId": "s1",
		},
	})
	if got := result["closed"]; got != true {
		t.Fatalf("expected closed true, got %#v", result)
	}
}

func TestHandleWebSocketCapabilities(t *testing.T) {
	server := NewServer(&stubClient{
		initResult: initializeResult{ProtocolVersion: 1},
	})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.HandleWebSocket(w, r)
	}))
	defer httpServer.Close()

	wsURL := "ws" + httpServer.URL[len("http"):]
	header := http.Header{}
	header.Set("Authorization", "Bearer test-token")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if err := conn.WriteJSON(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      "cap-1",
		Method:  "acp.capabilities",
		Params:  map[string]any{},
	}); err != nil {
		t.Fatalf("write json: %v", err)
	}
	var envelope map[string]any
	if err := conn.ReadJSON(&envelope); err != nil {
		t.Fatalf("read json: %v", err)
	}
	result := envelope["result"].(map[string]any)
	providers := result["providers"].([]any)
	if len(providers) != 1 || providers[0] != "gemini" {
		t.Fatalf("expected gemini provider over websocket, got %#v", result)
	}
}
