package hermesadapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type rpcClient interface {
	Initialize() (initializeResult, error)
	Call(method string, params map[string]any) (map[string]any, error)
	SetNotificationHandler(func(map[string]any))
	Close() error
}

type initializeResult struct {
	ProtocolVersion   int              `json:"protocolVersion"`
	AuthMethods       []map[string]any `json:"authMethods"`
	AgentCapabilities map[string]any   `json:"agentCapabilities"`
}

type stdioRPCClient struct {
	mu                  sync.Mutex
	command             string
	args                []string
	env                 []string
	protocolVersion     int
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	stdout              *bufio.Reader
	stdoutPipe          io.ReadCloser
	stderr              io.ReadCloser
	nextID              atomic.Int64
	initialized         bool
	initResult          initializeResult
	notificationHandler func(map[string]any)
}

func newStdioRPCClient(command string, args []string, env []string, protocolVersion int) *stdioRPCClient {
	return &stdioRPCClient{
		command:         strings.TrimSpace(command),
		args:            append([]string(nil), args...),
		env:             append([]string(nil), env...),
		protocolVersion: protocolVersion,
	}
}

func (c *stdioRPCClient) Initialize() (initializeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureStartedLocked(); err != nil {
		return initializeResult{}, err
	}
	if c.initialized {
		return c.initResult, nil
	}
	result, err := c.callLocked("initialize", map[string]any{
		"protocolVersion": c.protocolVersion,
		"clientInfo": map[string]any{
			"name":    "xworkmate-hermes-adapter",
			"version": "0.1.0",
		},
	})
	if err != nil {
		return initializeResult{}, err
	}
	payload, _ := result["result"].(map[string]any)
	data, _ := json.Marshal(payload)
	var parsed initializeResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		return initializeResult{}, fmt.Errorf("decode initialize result: %w", err)
	}
	c.initialized = true
	c.initResult = parsed
	return parsed, nil
}

func (c *stdioRPCClient) Call(method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureStartedLocked(); err != nil {
		return nil, err
	}
	return c.callLocked(method, params)
}

func (c *stdioRPCClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *stdioRPCClient) SetNotificationHandler(handler func(map[string]any)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notificationHandler = handler
}

func (c *stdioRPCClient) ensureStartedLocked() error {
	if c.cmd != nil {
		return nil
	}
	if c.command == "" {
		return fmt.Errorf("hermes command is empty")
	}
	cmd := exec.Command(c.command, c.args...)
	cmd.Env = append(os.Environ(), c.env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	c.stdoutPipe = stdout
	c.stdout = bufio.NewReader(stdout)
	c.stderr = stderr
	return nil
}

func (c *stdioRPCClient) closeLocked() error {
	var firstErr error
	if c.stdin != nil {
		if err := c.stdin.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && firstErr == nil && !strings.Contains(strings.ToLower(err.Error()), "finished") {
			firstErr = err
		}
		_, _ = c.cmd.Process.Wait()
	}
	c.cmd = nil
	c.stdin = nil
	c.stdout = nil
	c.stdoutPipe = nil
	c.stderr = nil
	c.initialized = false
	c.initResult = initializeResult{}
	return firstErr
}

func (c *stdioRPCClient) callLocked(method string, params map[string]any) (map[string]any, error) {
	requestID := fmt.Sprintf("req-%d", c.nextID.Add(1))
	awaitFinalText := strings.EqualFold(strings.TrimSpace(method), "session/prompt")
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  strings.TrimSpace(method),
		"params":  params,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(encoded, '\n')); err != nil {
		return nil, err
	}
	var response map[string]any
	for {
		if deadlineSetter, ok := c.stdoutPipe.(interface{ SetReadDeadline(time.Time) error }); ok {
			timeout := 2 * time.Minute
			if awaitFinalText {
				timeout = 5 * time.Minute
			}
			_ = deadlineSetter.SetReadDeadline(time.Now().Add(timeout))
		}
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			if stderr, stderrErr := io.ReadAll(c.stderr); stderrErr == nil {
				trimmed := strings.TrimSpace(string(stderr))
				if trimmed != "" {
					return nil, fmt.Errorf("hermes acp read failed: %s", trimmed)
				}
			}
			return nil, err
		}
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}
		if responseID, _ := payload["id"].(string); responseID != "" {
			if responseID == requestID {
				response = payload
				c.drainNotificationsLocked(2 * time.Second)
				return response, nil
			}
			continue
		}
		if handler := c.notificationHandler; handler != nil {
			handler(payload)
		}
	}
}

func (c *stdioRPCClient) drainNotificationsLocked(timeout time.Duration) {
	if c.stdoutPipe == nil || timeout <= 0 {
		return
	}
	if deadlineSetter, ok := c.stdoutPipe.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = deadlineSetter.SetReadDeadline(time.Now().Add(timeout))
		defer func() {
			_ = deadlineSetter.SetReadDeadline(time.Time{})
		}()
	}
	for {
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			if isTimeoutError(err) {
				return
			}
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}
		if strings.TrimSpace(fmt.Sprint(payload["id"])) != "" {
			continue
		}
		if handler := c.notificationHandler; handler != nil {
			handler(payload)
		}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func isHermesFinalSessionUpdate(notification map[string]any) bool {
	if notification == nil {
		return false
	}
	method := strings.TrimSpace(fmt.Sprint(notification["method"]))
	if method != "session.update" && method != "session/update" && method != "acp.session.update" {
		return false
	}
	params, _ := notification["params"].(map[string]any)
	if len(params) == 0 {
		return false
	}
	update, _ := params["update"].(map[string]any)
	if len(update) == 0 {
		update = params
	}
	sessionUpdate := strings.TrimSpace(fmt.Sprint(update["sessionUpdate"]))
	return sessionUpdate == "agent_message_text"
}
