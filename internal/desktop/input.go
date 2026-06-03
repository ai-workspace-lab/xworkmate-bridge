package desktop

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// InputEvent represents a client mouse or keyboard action
type InputEvent struct {
	Type   string  `json:"type"`             // "mouse_move", "mouse_down", "mouse_up", "key_down", "key_up", "scroll"
	X      float64 `json:"x,omitempty"`      // normalized x coordinate (0.0 to 1.0)
	Y      float64 `json:"y,omitempty"`      // normalized y coordinate (0.0 to 1.0)
	Button int     `json:"button,omitempty"` // mouse button: 1=left, 2=middle, 3=right, 4=scroll_up, 5=scroll_down
	Key    string  `json:"key,omitempty"`    // key symbol or keycode
}

// XdotoolInjector injects inputs by writing to a persistent xdotool process stdin
type XdotoolInjector struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	mu        sync.Mutex
	display   string
	width     int
	height    int
	isStarted bool
}

func NewXdotoolInjector(display string) *XdotoolInjector {
	if display == "" {
		display = ":0.0"
	}
	return &XdotoolInjector{
		display: display,
		width:   1280, // Default fallbacks
		height:  720,
	}
}

// Start launches xdotool and queries screen resolution
func (xi *XdotoolInjector) Start() error {
	xi.mu.Lock()
	defer xi.mu.Unlock()

	if xi.isStarted {
		return nil
	}

	// 1. Resolve screen resolution
	w, h, err := xi.queryDisplayGeometry()
	if err == nil {
		xi.width = w
		xi.height = h
		log.Printf("Detected remote display geometry: %dx%d on %s", w, h, xi.display)
	} else {
		log.Printf("Warning: Failed to query display geometry: %v. Using default: %dx%d", err, xi.width, xi.height)
	}

	// 2. Launch persistent xdotool process
	cmd := exec.Command("xdotool", "-")
	cmd.Env = append(cmd.Env, "DISPLAY="+xi.display)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to open xdotool stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("failed to start xdotool process: %w", err)
	}

	xi.cmd = cmd
	xi.stdin = stdin
	xi.isStarted = true

	return nil
}

// Inject sends a command to the persistent xdotool process
func (xi *XdotoolInjector) Inject(event InputEvent) error {
	xi.mu.Lock()
	defer xi.mu.Unlock()

	if !xi.isStarted || xi.stdin == nil {
		return fmt.Errorf("injector is not running")
	}

	var cmdStr string

	switch event.Type {
	case "mouse_move":
		absX := int(event.X * float64(xi.width))
		absY := int(event.Y * float64(xi.height))
		cmdStr = fmt.Sprintf("mousemove %d %d\n", absX, absY)

	case "mouse_down":
		btn := xi.mapButton(event.Button)
		cmdStr = fmt.Sprintf("mousedown %d\n", btn)

	case "mouse_up":
		btn := xi.mapButton(event.Button)
		cmdStr = fmt.Sprintf("mouseup %d\n", btn)

	case "key_down":
		key := xi.sanitizeKey(event.Key)
		cmdStr = fmt.Sprintf("keydown %s\n", key)

	case "key_up":
		key := xi.sanitizeKey(event.Key)
		cmdStr = fmt.Sprintf("keyup %s\n", key)

	case "scroll":
		// xdotool maps scroll up to button 4 and scroll down to button 5
		if event.Button == 4 || event.Button == 5 {
			cmdStr = fmt.Sprintf("click %d\n", event.Button)
		}

	default:
		return fmt.Errorf("unsupported input type: %s", event.Type)
	}

	if cmdStr != "" {
		_, err := xi.stdin.Write([]byte(cmdStr))
		if err != nil {
			// Try to restart if pipe is broken
			log.Printf("xdotool write error: %v. Attempting to restart injector.", err)
			xi.isStarted = false
			xi.stdin.Close()
			if restartErr := xi.Start(); restartErr == nil {
				_, _ = xi.stdin.Write([]byte(cmdStr))
			}
			return err
		}
	}

	return nil
}

// Close terminates the xdotool process
func (xi *XdotoolInjector) Close() error {
	xi.mu.Lock()
	defer xi.mu.Unlock()

	if !xi.isStarted {
		return nil
	}

	xi.isStarted = false
	if xi.stdin != nil {
		_ = xi.stdin.Close()
	}
	if xi.cmd != nil {
		_ = xi.cmd.Process.Kill()
	}

	xi.stdin = nil
	xi.cmd = nil
	return nil
}

func (xi *XdotoolInjector) queryDisplayGeometry() (int, int, error) {
	cmd := exec.Command("xdotool", "getdisplaygeometry")
	cmd.Env = append(cmd.Env, "DISPLAY="+xi.display)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}

	parts := strings.Fields(string(out))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("invalid geometry output: %s", string(out))
	}

	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("failed to parse geometry: %w, %w", err1, err2)
	}

	return w, h, nil
}

func (xi *XdotoolInjector) mapButton(btn int) int {
	// Standard mapping: 1=left, 2=middle, 3=right
	if btn <= 0 || btn > 3 {
		return 1
	}
	return btn
}

func (xi *XdotoolInjector) sanitizeKey(key string) string {
	// Clean or map keys if they need special handling in xdotool
	// For example, Flutter "Backspace" needs to be "BackSpace" or "Return" to "Return".
	key = strings.TrimSpace(key)
	switch strings.ToLower(key) {
	case "enter":
		return "Return"
	case "backspace":
		return "BackSpace"
	case "tab":
		return "Tab"
	case "escape":
		return "Escape"
	case "control":
		return "ctrl"
	case "shift":
		return "shift"
	case "alt":
		return "alt"
	case "meta":
		return "super"
	}
	return key
}
