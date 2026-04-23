package acp

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
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
	httpServer := &http.Server{
		Addr:         strings.TrimSpace(*listen),
		Handler:      server.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	if err := httpServer.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ACP server failed: %w", err)
	}
	return nil
}

func NewServer() *Server {
	config, providerCatalog, providerOrder := newProductionProviderCatalog()
	return &Server{
		config:          config,
		sessions:        make(map[string]*session),
		queues:          make(map[string]chan task),
		gateway:         gatewayruntime.NewManager(),
		providerCatalog: providerCatalog,
		providerOrder:   providerOrder,
		authService:     service.NewStaticTokenAuthService(strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))),
		allowedOrigins:  shared.ParseAllowedOrigins(shared.EnvOrDefault("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*")),
	}
}
