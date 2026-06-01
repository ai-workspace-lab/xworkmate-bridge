package acp

import (
	"context"
	"net/url"
	"strings"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/shared"
)

const (
	productionOpenClawGatewayConnectTimeout   = 60 * time.Second
	productionOpenClawGatewayChallengeTimeout = 30 * time.Second
)

func (s *Server) handleGatewayMethod(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	switch method {
	case "xworkmate.gateway.connect":
		return handleGatewayConnect(s, params, notify), nil
	case "xworkmate.gateway.request":
		return handleGatewayRequest(s, params, notify), nil
	case "xworkmate.gateway.disconnect":
		return handleGatewayDisconnect(s, params, notify), nil
	default:
		return nil, &shared.RPCError{Code: -32601, Message: "unknown gateway method: " + method}
	}
}

func handleGatewayConnect(
	server *Server,
	params map[string]any,
	notify func(map[string]any),
) map[string]any {
	request := gatewayruntime.ConnectRequest{
		RuntimeID:          strings.TrimSpace(shared.StringArg(params, "runtimeId", "")),
		Mode:               strings.TrimSpace(shared.StringArg(params, "gatewayProviderId", "")),
		ClientID:           strings.TrimSpace(shared.StringArg(params, "clientId", "")),
		Locale:             strings.TrimSpace(shared.StringArg(params, "locale", "")),
		UserAgent:          strings.TrimSpace(shared.StringArg(params, "userAgent", "")),
		ConnectAuthMode:    strings.TrimSpace(shared.StringArg(params, "connectAuthMode", "")),
		ConnectAuthFields:  parseGatewayRuntimeStringSlice(params["connectAuthFields"]),
		ConnectAuthSources: parseGatewayRuntimeStringSlice(params["connectAuthSources"]),
		HasSharedAuth:      parseBool(params["hasSharedAuth"]),
		HasDeviceToken:     parseBool(params["hasDeviceToken"]),
		Endpoint: gatewayruntime.Endpoint{
			Host: strings.TrimSpace(shared.StringArg(shared.AsMap(params["endpoint"]), "host", "")),
			Port: parsePositiveInt(shared.AsMap(params["endpoint"])["port"]),
			TLS:  parseBool(shared.AsMap(params["endpoint"])["tls"]),
		},
		PackageInfo: gatewayruntime.PackageInfo{
			AppName:     strings.TrimSpace(shared.StringArg(shared.AsMap(params["packageInfo"]), "appName", "")),
			PackageName: strings.TrimSpace(shared.StringArg(shared.AsMap(params["packageInfo"]), "packageName", "")),
			Version:     strings.TrimSpace(shared.StringArg(shared.AsMap(params["packageInfo"]), "version", "")),
			BuildNumber: strings.TrimSpace(shared.StringArg(shared.AsMap(params["packageInfo"]), "buildNumber", "")),
		},
		DeviceInfo: gatewayruntime.DeviceInfo{
			Platform:        strings.TrimSpace(shared.StringArg(shared.AsMap(params["deviceInfo"]), "platform", "")),
			PlatformVersion: strings.TrimSpace(shared.StringArg(shared.AsMap(params["deviceInfo"]), "platformVersion", "")),
			DeviceFamily:    strings.TrimSpace(shared.StringArg(shared.AsMap(params["deviceInfo"]), "deviceFamily", "")),
			ModelIdentifier: strings.TrimSpace(shared.StringArg(shared.AsMap(params["deviceInfo"]), "modelIdentifier", "")),
		},
		Identity: gatewayruntime.DeviceIdentity{
			DeviceID:            strings.TrimSpace(shared.StringArg(shared.AsMap(params["identity"]), "deviceId", "")),
			PublicKeyBase64URL:  strings.TrimSpace(shared.StringArg(shared.AsMap(params["identity"]), "publicKeyBase64Url", "")),
			PrivateKeyBase64URL: strings.TrimSpace(shared.StringArg(shared.AsMap(params["identity"]), "privateKeyBase64Url", "")),
		},
		Auth: gatewayruntime.AuthConfig{
			Token:       strings.TrimSpace(shared.StringArg(shared.AsMap(params["auth"]), "token", "")),
			DeviceToken: strings.TrimSpace(shared.StringArg(shared.AsMap(params["auth"]), "deviceToken", "")),
			Password:    strings.TrimSpace(shared.StringArg(shared.AsMap(params["auth"]), "password", "")),
		},
	}
	if request.Mode == "" {
		request.Mode = "openclaw"
	}
	request = applyProductionGatewayRouting(server, request)
	usesBridgeIdentity := false
	if isOpenClawMode(request.Mode) && strings.TrimSpace(request.Identity.DeviceID) == "" {
		identity, deviceToken := bridgeGatewayOpenClawCredentials()
		request.Identity = identity
		if strings.TrimSpace(request.Auth.DeviceToken) == "" {
			request.Auth.DeviceToken = deviceToken
		}
		request.HasDeviceToken = strings.TrimSpace(request.Auth.DeviceToken) != ""
		usesBridgeIdentity = true
	}
	request.ReportedRemoteAddress = resolveGatewayReportedRemoteAddress(server, request)

	if server.gateway == nil {
		server.gateway = gatewayruntime.NewManager()
	}

	result := server.gateway.Connect(request, notify)
	if result.OK && usesBridgeIdentity {
		saveBridgeGatewayDeviceToken(result.ReturnedDeviceToken)
	}
	return map[string]any{
		"ok":                  result.OK,
		"snapshot":            result.Snapshot,
		"auth":                result.Auth,
		"returnedDeviceToken": result.ReturnedDeviceToken,
		"error":               result.Error,
	}
}

func applyProductionGatewayRouting(
	server *Server,
	request gatewayruntime.ConnectRequest,
) gatewayruntime.ConnectRequest {
	if !isOpenClawMode(request.Mode) {
		return request
	}

	gatewayURL := resolveURL(server.config.Upstream.GatewayURL, "GATEWAY_RPC_URL")
	if gatewayURL == "" {
		return request
	}

	parsed, err := url.Parse(gatewayURL)
	if err != nil || parsed.Hostname() == "" {
		return request
	}

	tls := strings.ToLower(parsed.Scheme) == "https" || strings.ToLower(parsed.Scheme) == "wss"
	port := parsePositiveInt(parsed.Port())
	if port == 0 {
		if tls {
			port = 443
		} else {
			port = 80
		}
	}

	request.Endpoint = gatewayruntime.Endpoint{
		Host: parsed.Hostname(),
		Port: port,
		TLS:  tls,
		Path: parsed.EscapedPath(),
	}
	request.Auth.Token = bridgeSharedAuthToken()
	request.Auth.Password = ""
	request.ConnectAuthMode = "shared-token"
	request.ConnectAuthFields = []string{"token"}
	request.ConnectAuthSources = []string{"bridge"}
	request.HasSharedAuth = request.Auth.Token != ""
	return request
}

func handleGatewayRequest(
	server *Server,
	params map[string]any,
	notify func(map[string]any),
) map[string]any {
	if server.gateway == nil {
		return map[string]any{"ok": false, "error": map[string]any{"message": "gateway not initialized"}}
	}
	timeout := time.Duration(parsePositiveInt(params["timeoutMs"])) * time.Millisecond
	result := server.gateway.Request(
		strings.TrimSpace(shared.StringArg(params, "runtimeId", "")),
		strings.TrimSpace(shared.StringArg(params, "method", "")),
		shared.AsMap(params["params"]),
		timeout,
		notify,
	)
	return map[string]any{
		"ok":      result.OK,
		"payload": result.Payload,
		"error":   result.Error,
	}
}

func handleGatewayDisconnect(
	server *Server,
	params map[string]any,
	notify func(map[string]any),
) map[string]any {
	if server.gateway != nil {
		server.gateway.Disconnect(
			strings.TrimSpace(shared.StringArg(params, "runtimeId", "")),
			notify,
		)
	}
	return map[string]any{"accepted": true}
}

func ensureProductionGatewayConnected(
	server *Server,
	mode string,
	notify func(map[string]any),
) *shared.RPCError {
	normalizedMode := strings.TrimSpace(strings.ToLower(mode))
	if normalizedMode == "" {
		normalizedMode = "openclaw"
	}
	if normalizedMode != "openclaw" {
		return nil
	}
	if server.gateway == nil {
		server.gateway = gatewayruntime.NewManager()
	}
	configureProductionOpenClawGatewayRuntime(server.gateway)

	request := applyProductionGatewayRouting(
		server,
		gatewayruntime.ConnectRequest{
			RuntimeID:   "xworkmate-bridge-openclaw",
			Mode:        "openclaw",
			ClientID:    "openclaw-macos",
			Locale:      "en_US",
			UserAgent:   "xworkmate-bridge",
			Endpoint:    gatewayruntime.Endpoint{Host: "127.0.0.1", Port: 18789, TLS: false},
			PackageInfo: gatewayruntime.PackageInfo{AppName: "XWorkmate Bridge", PackageName: "xworkmate-bridge", Version: "bridge", BuildNumber: "0"},
			DeviceInfo:  gatewayruntime.DeviceInfo{Platform: "macos", DeviceFamily: "Mac", ModelIdentifier: "Mac14,5"},
		},
	)
	identity, deviceToken := bridgeGatewayOpenClawCredentials()
	request.Identity = identity
	request.Auth.DeviceToken = deviceToken
	request.HasDeviceToken = deviceToken != ""
	request.ReportedRemoteAddress = resolveGatewayReportedRemoteAddress(server, request)
	result := server.gateway.Connect(request, notify)
	if result.OK {
		saveBridgeGatewayDeviceToken(result.ReturnedDeviceToken)
		return nil
	}
	message := strings.TrimSpace(shared.StringArg(result.Error, "message", "gateway connect failed"))
	code := strings.TrimSpace(shared.StringArg(result.Error, "code", ""))
	if code != "" {
		message = code + ": " + message
	}
	return &shared.RPCError{Code: -32002, Message: "GATEWAY_CONNECT_FAILED: " + message}
}

func configureProductionOpenClawGatewayRuntime(manager *gatewayruntime.Manager) {
	if manager == nil {
		return
	}
	manager.ConnectTimeout = productionOpenClawGatewayConnectTimeout
	manager.ChallengeTimeout = productionOpenClawGatewayChallengeTimeout
}

// Helper functions are now in helpers.go
