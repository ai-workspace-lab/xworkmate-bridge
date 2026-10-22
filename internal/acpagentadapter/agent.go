package acpagentadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// agentShutdownGrace bounds how long close waits for the agent to exit after
// stdin EOF before killing it.
const agentShutdownGrace = 5 * time.Second

// agentRequestHandler answers a request the agent sends to the client
// (for example session/request_permission). It returns the JSON-RPC result
// or an error that is sent back as a JSON-RPC error.
type agentRequestHandler func(ctx context.Context, method string, params map[string]any) (map[string]any, error)

// agentListener receives everything the agent sends for one ACP session.
type agentListener struct {
	onUpdate  func(params map[string]any)
	onRequest agentRequestHandler
}

type rpcResponse struct {
	Result map[string]any
	Err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

// stdioAgent is a full-duplex ACP v1 JSON-RPC client over a child process'
// stdin/stdout. Responses, notifications and agent-initiated requests are
// demultiplexed by a single reader goroutine.
type stdioAgent struct {
	command string
	args    []string
	dir     string

	startMu sync.Mutex
	started bool
	exited  chan struct{}
	exitErr error

	writeMu sync.Mutex
	stdin   io.WriteCloser
	cmd     *exec.Cmd

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan rpcResponse
	// listeners is keyed by ACP session ID.
	listeners map[string]agentListener

	initOnce   sync.Once
	initResult map[string]any
	initErr    error
}

func newStdioAgent(command string, args []string, dir string) *stdioAgent {
	return &stdioAgent{
		command:   strings.TrimSpace(command),
		args:      append([]string(nil), args...),
		dir:       dir,
		pending:   make(map[string]chan rpcResponse),
		listeners: make(map[string]agentListener),
	}
}

func (a *stdioAgent) start() error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if a.started {
		select {
		case <-a.exited:
			return fmt.Errorf("acp agent exited: %v", a.exitErr)
		default:
			return nil
		}
	}
	if a.command == "" {
		return fmt.Errorf("acp agent command is empty")
	}
	cmd := exec.Command(a.command, a.args...)
	cmd.Dir = a.dir
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	a.cmd = cmd
	a.stdin = stdin
	a.exited = make(chan struct{})
	a.started = true
	go a.readLoop(stdout)
	return nil
}

func (a *stdioAgent) readLoop(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 1<<20)
	var loopErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			a.dispatch(line)
		}
		if err != nil {
			loopErr = err
			break
		}
	}
	waitErr := a.cmd.Wait()
	if waitErr != nil {
		loopErr = waitErr
	}
	a.mu.Lock()
	a.exitErr = loopErr
	for id, ch := range a.pending {
		ch <- rpcResponse{Err: &rpcError{Code: -32000, Message: fmt.Sprintf("acp agent exited: %v", loopErr)}}
		delete(a.pending, id)
	}
	a.mu.Unlock()
	close(a.exited)
}

func (a *stdioAgent) dispatch(line []byte) {
	var message map[string]any
	if err := json.Unmarshal(line, &message); err != nil {
		log.Printf("[acp-agent] drop non-JSON stdout line: %v", err)
		return
	}
	method, _ := message["method"].(string)
	id, hasID := message["id"]
	params, _ := message["params"].(map[string]any)
	switch {
	case method != "" && hasID:
		go a.answerAgentRequest(id, method, params)
	case method != "":
		if method != "session/update" {
			log.Printf("[acp-agent] ignore notification %s", method)
			return
		}
		if listener, ok := a.listener(stringField(params, "sessionId")); ok && listener.onUpdate != nil {
			listener.onUpdate(params)
		}
	case hasID:
		key := fmt.Sprint(id)
		a.mu.Lock()
		ch, ok := a.pending[key]
		delete(a.pending, key)
		a.mu.Unlock()
		if !ok {
			log.Printf("[acp-agent] response for unknown id %s", key)
			return
		}
		response := rpcResponse{}
		if errPayload, ok := message["error"].(map[string]any); ok {
			code, _ := errPayload["code"].(float64)
			messageText, _ := errPayload["message"].(string)
			response.Err = &rpcError{Code: int(code), Message: messageText}
		} else {
			response.Result, _ = message["result"].(map[string]any)
			if response.Result == nil {
				response.Result = map[string]any{}
			}
		}
		ch <- response
	}
}

func (a *stdioAgent) answerAgentRequest(id any, method string, params map[string]any) {
	listener, ok := a.listener(stringField(params, "sessionId"))
	var (
		result map[string]any
		err    error
	)
	if !ok || listener.onRequest == nil {
		err = &rpcError{Code: -32601, Message: "client does not handle " + method}
	} else {
		result, err = listener.onRequest(context.Background(), method, params)
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		code := -32603
		if typed, ok := err.(*rpcError); ok {
			code = typed.Code
		}
		reply["error"] = map[string]any{"code": code, "message": err.Error()}
	} else {
		reply["result"] = result
	}
	if writeErr := a.write(reply); writeErr != nil {
		log.Printf("[acp-agent] reply to %s failed: %v", method, writeErr)
	}
}

func (a *stdioAgent) write(message map[string]any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err = a.stdin.Write(append(encoded, '\n'))
	return err
}

// call sends a request and waits for its response or ctx cancellation.
func (a *stdioAgent) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if err := a.start(); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("bridge-%d", a.nextID.Add(1))
	ch := make(chan rpcResponse, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.mu.Unlock()
	if err := a.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-ch:
		if response.Err != nil {
			return nil, response.Err
		}
		return response.Result, nil
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (a *stdioAgent) notify(method string, params map[string]any) error {
	if err := a.start(); err != nil {
		return err
	}
	return a.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// initialize runs the ACP handshake once per process. The client declares
// no filesystem or terminal capability: the agent uses its own tools inside
// the session cwd, and every side effect still goes through
// session/request_permission.
func (a *stdioAgent) initialize(ctx context.Context) (map[string]any, error) {
	a.initOnce.Do(func() {
		a.initResult, a.initErr = a.call(ctx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
				"terminal": false,
			},
			"clientInfo": map[string]any{"name": "xworkmate-acp-agent-adapter", "version": "0.1.0"},
		})
	})
	return a.initResult, a.initErr
}

func (a *stdioAgent) setListener(acpSessionID string, listener agentListener) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listeners[acpSessionID] = listener
}

func (a *stdioAgent) removeListener(acpSessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.listeners, acpSessionID)
}

func (a *stdioAgent) listener(acpSessionID string) (agentListener, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	listener, ok := a.listeners[acpSessionID]
	return listener, ok
}

func (a *stdioAgent) close() error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if !a.started {
		return nil
	}
	if err := a.stdin.Close(); err != nil {
		return err
	}
	select {
	case <-a.exited:
		return nil
	case <-time.After(agentShutdownGrace):
	}
	if err := a.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("kill acp agent after stdin EOF: %w", err)
	}
	<-a.exited
	return nil
}

func stringField(params map[string]any, key string) string {
	value, _ := params[key].(string)
	return strings.TrimSpace(value)
}
