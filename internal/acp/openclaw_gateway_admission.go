package acp

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	defaultOpenClawGatewayMaxActive   = 5
	defaultOpenClawGatewayMaxQueued   = 20
	defaultOpenClawGatewayQueueWait   = 10 * time.Minute
	openClawGatewayQueueStatusTick    = 20 * time.Second
	openClawGatewayBusyErrorCode      = "OPENCLAW_GATEWAY_BUSY"
	openClawGatewayQueueFullReason    = "queue_full"
	openClawGatewayQueueTimeoutReason = "queue_timeout"
)

type openClawGatewayAdmissionGate struct {
	slots     chan struct{}
	maxActive int
	maxQueued int
	timeout   time.Duration

	mu     sync.Mutex
	queued int
}

func newOpenClawGatewayAdmissionGate(config *BridgeConfig) *openClawGatewayAdmissionGate {
	admissionConfig := OpenClawGatewayConfig{}
	if config != nil {
		admissionConfig = config.OpenClawGateway
	}
	maxActive := admissionInt(admissionConfig.MaxActive, "XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_ACTIVE", defaultOpenClawGatewayMaxActive)
	if maxActive < 1 {
		log.Printf("level=warn component=openclaw_gateway event=invalid_admission_config key=%q value=%d", "max_active", maxActive)
		maxActive = defaultOpenClawGatewayMaxActive
	}
	maxQueued := admissionInt(admissionConfig.MaxQueued, "XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_QUEUED", defaultOpenClawGatewayMaxQueued)
	if maxQueued < 0 {
		log.Printf("level=warn component=openclaw_gateway event=invalid_admission_config key=%q value=%d", "max_queued", maxQueued)
		maxQueued = defaultOpenClawGatewayMaxQueued
	}
	timeout := admissionDuration(admissionConfig.QueueTimeout, "queue_timeout", "XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_QUEUE_TIMEOUT", defaultOpenClawGatewayQueueWait)
	if timeout <= 0 {
		log.Printf("level=warn component=openclaw_gateway event=invalid_admission_config key=%q value=%q", "queue_timeout", timeout.String())
		timeout = defaultOpenClawGatewayQueueWait
	}
	return &openClawGatewayAdmissionGate{
		slots:     make(chan struct{}, maxActive),
		maxActive: maxActive,
		maxQueued: maxQueued,
		timeout:   timeout,
	}
}

func admissionInt(configValue *int, envKey string, fallback int) int {
	if configValue != nil {
		return *configValue
	}
	return envInt(envKey, fallback)
}

func admissionDuration(configValue string, configKey string, envKey string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(configValue)
	if raw == "" {
		return envDuration(envKey, fallback)
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("level=warn component=openclaw_gateway event=invalid_duration_config key=%q value=%q", configKey, raw)
		return fallback
	}
	return value
}

func (g *openClawGatewayAdmissionGate) acquire(
	ctx context.Context,
	notifyQueued func(position int, queued int),
	notifyRunning func(active int),
) (func(), *shared.RPCError) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.slots <- struct{}{}:
		if notifyRunning != nil {
			notifyRunning(len(g.slots))
		}
		return g.release, nil
	default:
	}

	g.mu.Lock()
	if g.queued >= g.maxQueued {
		g.mu.Unlock()
		return nil, openClawGatewayBusyRPCError(openClawGatewayQueueFullReason)
	}
	g.queued += 1
	position := g.queued
	queued := g.queued
	g.mu.Unlock()
	if notifyQueued != nil {
		notifyQueued(position, queued)
	}

	timer := time.NewTimer(g.timeout)
	ticker := time.NewTicker(openClawGatewayQueueStatusTick)
	defer timer.Stop()
	defer ticker.Stop()
	defer g.decrementQueued()

	for {
		select {
		case g.slots <- struct{}{}:
			if notifyRunning != nil {
				notifyRunning(len(g.slots))
			}
			return g.release, nil
		case <-ticker.C:
			if notifyQueued != nil {
				notifyQueued(position, g.queuedCount())
			}
		case <-timer.C:
			return nil, openClawGatewayBusyRPCError(openClawGatewayQueueTimeoutReason)
		case <-ctx.Done():
			return nil, openClawGatewayBusyRPCError(ctx.Err().Error())
		}
	}
}

func (g *openClawGatewayAdmissionGate) release() {
	if g == nil {
		return
	}
	select {
	case <-g.slots:
	default:
	}
}

func (g *openClawGatewayAdmissionGate) decrementQueued() {
	g.mu.Lock()
	if g.queued > 0 {
		g.queued -= 1
	}
	g.mu.Unlock()
}

func (g *openClawGatewayAdmissionGate) queuedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queued
}

func openClawGatewayBusyRPCError(reason string) *shared.RPCError {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = openClawGatewayQueueFullReason
	}
	return &shared.RPCError{
		Code:    -32002,
		Message: fmt.Sprintf("%s: OpenClaw gateway is busy (%s)", openClawGatewayBusyErrorCode, reason),
		Data: map[string]any{
			"code":   openClawGatewayBusyErrorCode,
			"reason": reason,
		},
	}
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(shared.EnvOrDefault(key, ""))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("level=warn component=openclaw_gateway event=invalid_int_env key=%q value=%q", key, raw)
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(shared.EnvOrDefault(key, ""))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("level=warn component=openclaw_gateway event=invalid_duration_env key=%q value=%q", key, raw)
		return fallback
	}
	return value
}
