package acp

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

func Serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String(
		"listen",
		shared.EnvOrDefault("ACP_LISTEN_ADDR", "127.0.0.1:8787"),
		"ACP listen address",
	)
	_ = flags.Parse(args)

	server := NewServer()
	httpServer := newHTTPServer(strings.TrimSpace(*listen), server.Handler())

	if err := httpServer.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ACP server failed: %w", err)
	}
	return nil
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         strings.TrimSpace(addr),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: openClawAgentWaitMaxTimeout + openClawAgentWaitHTTPMargin,
		IdleTimeout:  2 * time.Minute,
	}
}

func NewServer() *Server {
	config := loadBridgeConfig()
	s := &Server{
		sessions:       make(map[string]*session),
		config:         config,
		allowedOrigins: shared.ParseAllowedOrigins(shared.EnvOrDefault("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*")),
		authService: service.NewStaticTokenAuthService(
			shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""),
			shared.EnvOrDefault("BRIDGE_REVIEW_AUTH_TOKEN", ""),
		),
		openClawGate: newOpenClawGatewayAdmissionGate(config),
		taskRouter: newDistributedTaskRouter(distributedTaskRouterConfig{
			Config: config,
			Token:  resolveDistributedTaskForwardToken(config),
		}),
	}
	s.Bootstrap()
	return s
}
