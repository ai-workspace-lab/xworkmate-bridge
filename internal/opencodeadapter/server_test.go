package opencodeadapter

import (
	"testing"

	"xworkmate-bridge/internal/shared"
)

type stubOpenCodeClient struct {
	initializeCalled int
	createTitle      string
	sendSessionID    string
	sendPrompt       string
	sendParams       map[string]any
}

type invalidSessionOpenCodeClient struct {
	stubOpenCodeClient
}

func (s *stubOpenCodeClient) Initialize() (initializeResult, error) {
	s.initializeCalled++
	return initializeResult{ProtocolVersion: 1}, nil
}

func (s *stubOpenCodeClient) Call(method string, params map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

func (s *stubOpenCodeClient) CreateSession(title string) (string, error) {
	s.createTitle = title
	return "upstream-1", nil
}

func (s *stubOpenCodeClient) SendMessage(sessionID, prompt string, params map[string]any) (map[string]any, error) {
	s.sendSessionID = sessionID
	s.sendPrompt = prompt
	s.sendParams = params
	return map[string]any{"message": "pong"}, nil
}

func (s *stubOpenCodeClient) Close() error { return nil }

func (s *invalidSessionOpenCodeClient) CreateSession(title string) (string, error) {
	s.createTitle = title
	return "<nil>", nil
}

func TestHandleSessionStartUsesCreateSessionAndSendMessage(t *testing.T) {
	client := &stubOpenCodeClient{}
	server := NewServer(client)
	result := server.handleRequest(sharedRequest("session.start", map[string]any{
		"sessionId":        "thread-1",
		"taskPrompt":       "hello",
		"workingDirectory": t.TempDir(),
		"title":            "demo",
	}))
	if got := result["sessionId"]; got != "thread-1" {
		t.Fatalf("expected bridge session id thread-1, got %v", got)
	}
	if client.createTitle != "demo" {
		t.Fatalf("expected create title demo, got %q", client.createTitle)
	}
	if client.sendSessionID != "upstream-1" {
		t.Fatalf("expected upstream session id upstream-1, got %q", client.sendSessionID)
	}
	if client.sendPrompt != "hello" {
		t.Fatalf("expected prompt hello, got %q", client.sendPrompt)
	}
}

func TestHandleSessionMessageReusesSession(t *testing.T) {
	client := &stubOpenCodeClient{}
	server := NewServer(client)
	server.handleRequest(sharedRequest("session.start", map[string]any{
		"sessionId":  "thread-1",
		"taskPrompt": "hello",
	}))
	result := server.handleRequest(sharedRequest("session.message", map[string]any{
		"sessionId":  "thread-1",
		"taskPrompt": "follow-up",
	}))
	if got := result["message"]; got != "pong" {
		t.Fatalf("expected message pong, got %v", got)
	}
	if client.sendPrompt != "follow-up" {
		t.Fatalf("expected follow-up prompt, got %q", client.sendPrompt)
	}
}

func TestHandleSessionStartRejectsInvalidProviderSessionID(t *testing.T) {
	client := &invalidSessionOpenCodeClient{}
	server := NewServer(client)
	result := server.handleRequest(sharedRequest("session.start", map[string]any{
		"sessionId":  "thread-1",
		"taskPrompt": "hello",
	}))
	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected invalid upstream session to fail, got %#v", result)
	}
	if got := result["error"]; got != "opencode create session returned no session id" {
		t.Fatalf("expected missing session id error, got %#v", result)
	}
	if client.sendSessionID != "" {
		t.Fatalf("expected no follow-up message call, got session id %q", client.sendSessionID)
	}
}

func sharedRequest(method string, params map[string]any) shared.RPCRequest {
	return shared.RPCRequest{
		Method: method,
		Params: params,
	}
}
