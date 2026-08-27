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
	var authService interface {
		ValidateAuthorizationHeader(string) bool
	}
	// UAT/production uses Accounts credential introspection. Keep the static
	// token implementation as a local/dev compatibility path when no Accounts
	// endpoint is configured; this also preserves the existing bridge test and
	// standalone development workflow without weakening configured UAT auth.
	if introspectionURL := strings.TrimSpace(os.Getenv("BRIDGE_ACCOUNTS_INTROSPECTION_URL")); introspectionURL != "" || strings.TrimSpace(os.Getenv("BRIDGE_ACCOUNTS_SERVICE_TOKEN")) != "" {
		authService = service.NewCredentialTokenAuthService(
			introspectionURL,
			shared.EnvOrDefault("BRIDGE_ACCOUNTS_SERVICE_TOKEN", ""),
		)
	} else {
		var expected string
		var extras []string
		if len(authTokens) > 0 {
			expected = authTokens[0]
			extras = authTokens[1:]
		}
		authService = service.NewStaticTokenAuthService(expected, extras...)
	}
	s := &Server{
		sessions:              make(map[string]*session),
		config:                config,
		accountsSessionAPIURL: strings.TrimRight(strings.TrimSpace(os.Getenv("BRIDGE_ACCOUNTS_SESSION_API_URL")), "/"),
		accountsSessionClient: newAccountsSessionProxyClient(),
		allowedOrigins:        shared.ParseAllowedOrigins(shared.EnvOrDefault("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*")),
		authService:           authService,
		openClawGate:          newOpenClawGatewayAdmissionGate(config),
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

// bridgeInboundAuthTokens supplies the legacy static-token compatibility
// path and is also used to reject an unsafe internal/public token collision.
// When Accounts introspection is configured, app-facing requests still use
// the credential service above; these values are never accepted there.
func bridgeInboundAuthTokens() []string {
	seen := make(map[string]struct{})
	tokens := make([]string, 0, 3)
	for _, raw := range []string{
		os.Getenv("AI_WORKSPACE_AUTH_TOKEN"),
		os.Getenv("BRIDGE_AUTH_TOKEN"),
		os.Getenv("BRIDGE_REVIEW_AUTH_TOKEN"),
	} {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
}
