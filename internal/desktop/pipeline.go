package desktop

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const DefaultRTPPort = 5004

func normalizeRTPPort(port int) int {
	if port <= 0 {
		return DefaultRTPPort
	}
	return port
}

// PipelineManager manages the screen capture process lifecycle
type PipelineManager struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	isRunning bool
	cancel    context.CancelFunc
}

type PipelineConfig struct {
	Display  string // e.g. ":0"
	Port     int    // e.g. 5004
	Width    int    // e.g. 1920
	Height   int    // e.g. 1080
	FPS      int    // e.g. 30
	Bitrate  int    // in kbps, e.g. 2000
	UseGPU   bool   // try using hardware accelerated encoding (nvh264enc / h264_nvenc)
	ToolType string // "gstreamer" or "ffmpeg" or "auto"
}

func NewPipelineManager() *PipelineManager {
	return &PipelineManager{}
}

// Start spawns the GStreamer or FFmpeg capture process in the background
func (pm *PipelineManager) Start(cfg PipelineConfig) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.isRunning {
		return fmt.Errorf("pipeline is already running")
	}

	if cfg.Display == "" {
		cfg.Display = os.Getenv("DISPLAY")
		if cfg.Display == "" {
			cfg.Display = ":0.0"
		}
	}
	cfg.Port = normalizeRTPPort(cfg.Port)
	if cfg.Width <= 0 {
		cfg.Width = 1280
	}
	if cfg.Height <= 0 {
		cfg.Height = 720
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = 2000
	}

	tool, args, err := pm.resolvePipeline(cfg)
	if err != nil {
		return fmt.Errorf("failed to resolve pipeline command: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pm.cancel = cancel

	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = desktopCommandEnv(cfg.Display)

	// Capture stdout/stderr for logging
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()

	log.Printf("Starting capture pipeline: %s %s", tool, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start pipeline process: %w", err)
	}

	pm.cmd = cmd
	pm.isRunning = true

	// Monitor process termination asynchronously
	go func() {
		err := cmd.Wait()
		pm.mu.Lock()
		pm.isRunning = false
		pm.cmd = nil
		pm.mu.Unlock()
		if err != nil {
			log.Printf("Capture pipeline exited with error: %v", err)
		} else {
			log.Printf("Capture pipeline stopped cleanly")
		}
	}()

	return nil
}

// Stop terminates the capture process
func (pm *PipelineManager) Stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.isRunning || pm.cancel == nil {
		return
	}

	log.Println("Stopping capture pipeline...")
	pm.cancel() // cancels the context, sending SIGKILL or SIGTERM

	// Wait up to 2 seconds for clean exit
	for i := 0; i < 20; i++ {
		if !pm.isRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	pm.isRunning = false
	pm.cmd = nil
	pm.cancel = nil
}

func (pm *PipelineManager) IsRunning() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.isRunning
}

// resolvePipeline checks for available software and builds command arguments
func (pm *PipelineManager) resolvePipeline(cfg PipelineConfig) (string, []string, error) {
	tool := cfg.ToolType
	if tool == "auto" || tool == "" {
		if pm.hasExecutable("gst-launch-1.0") {
			tool = "gstreamer"
		} else if pm.hasExecutable("ffmpeg") {
			tool = "ffmpeg"
		} else {
			return "", nil, fmt.Errorf("neither GStreamer (gst-launch-1.0) nor FFmpeg was found in PATH")
		}
	}

	switch tool {
	case "gstreamer":
		return pm.buildGStreamer(cfg)
	case "ffmpeg":
		return pm.buildFFmpeg(cfg)
	default:
		return "", nil, fmt.Errorf("unsupported capture tool type: %s", tool)
	}
}

func (pm *PipelineManager) hasExecutable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (pm *PipelineManager) buildGStreamer(cfg PipelineConfig) (string, []string, error) {
	var pipelineParts []string

	// 1. Capture Source (X11)
	pipelineParts = append(pipelineParts, fmt.Sprintf("ximagesrc display-name=%s", cfg.Display))
	pipelineParts = append(pipelineParts, "video/x-raw,framerate=30/1")
	pipelineParts = append(pipelineParts, "videoconvert")
	pipelineParts = append(pipelineParts, "video/x-raw,format=I420")

	// 2. Encoder
	encoderStr := "x264enc speed-preset=ultrafast tune=zerolatency bitrate=" + fmt.Sprintf("%d", cfg.Bitrate) + " byte-stream=true key-int-max=30"
	if cfg.UseGPU {
		// Detect if nvcodec is present by calling gst-inspect-1.0 or simply attempting it.
		// We'll default to nvh264enc.
		encoderStr = "nvh264enc bitrate=" + fmt.Sprintf("%d", cfg.Bitrate) + " preset=low-latency gop-size=30"
	}
	pipelineParts = append(pipelineParts, encoderStr)

	// 3. Payload and Sink
	pipelineParts = append(pipelineParts, "video/x-h264,profile=baseline")
	pipelineParts = append(pipelineParts, "rtph264pay config-interval=1 pt=96")
	pipelineParts = append(pipelineParts, fmt.Sprintf("udpsink host=127.0.0.1 port=%d sync=false async=false", cfg.Port))

	// Join GStreamer pipeline with '!'
	pipelineStr := strings.Join(pipelineParts, " ! ")
	args := []string{"-v"}
	args = append(args, strings.Split(pipelineStr, " ")...)

	var cleanArgs []string
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed != "" {
			cleanArgs = append(cleanArgs, trimmed)
		}
	}

	return "gst-launch-1.0", cleanArgs, nil
}

func (pm *PipelineManager) buildFFmpeg(cfg PipelineConfig) (string, []string, error) {
	args := []string{
		"-f", "x11grab",
		"-draw_mouse", "1",
		"-framerate", fmt.Sprintf("%d", cfg.FPS),
		"-video_size", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
		"-i", cfg.Display,
	}

	// Encoder config
	if cfg.UseGPU {
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "llhp", // low latency high quality
			"-tune", "zerolatency",
			"-g", "30",
			"-pix_fmt", "yuv420p",
		)
	} else {
		args = append(args,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-pix_fmt", "yuv420p",
			"-x264-params", "repeat-headers=1",
			"-g", "30",
		)
	}

	// Constant Bitrate
	args = append(args,
		"-b:v", fmt.Sprintf("%dk", cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", cfg.Bitrate*2),
	)

	// RTP Stream over UDP
	args = append(args,
		"-f", "rtp",
		fmt.Sprintf("rtp://127.0.0.1:%d", cfg.Port),
	)

	return "ffmpeg", args, nil
}
