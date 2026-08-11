package acp

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
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
	authTokens := bridgeInboundAuthTokens()
	authToken := ""
	authExtraTokens := []string(nil)
	if len(authTokens) > 0 {
		authToken = authTokens[0]
		authExtraTokens = authTokens[1:]
	}
	s := &Server{
		sessions:       make(map[string]*session),
		config:         config,
		allowedOrigins: shared.ParseAllowedOrigins(shared.EnvOrDefault("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*")),
		authService: service.NewStaticTokenAuthService(
			authToken,
			authExtraTokens...,
		),
		openClawGate: newOpenClawGatewayAdmissionGate(config),
		taskRouter: newDistributedTaskRouter(distributedTaskRouterConfig{
			Config: config,
			Token:  resolveDistributedTaskForwardToken(config),
		}),
	}
	if internalServiceToken := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN")); internalServiceToken != "" {
		conflictsWithPublicToken := false
		for _, publicToken := range authTokens {
			if internalServiceToken == publicToken {
				conflictsWithPublicToken = true
				break
			}
		}
		if conflictsWithPublicToken {
			log.Printf("level=error component=task_run_dispatch event=auth_disabled reason=internal_token_conflicts_with_public_token")
		} else {
			s.taskRunAuthService = service.NewStaticTokenAuthService(internalServiceToken)
		}
	}
	s.Bootstrap()
	return s
}
