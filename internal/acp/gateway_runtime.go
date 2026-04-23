package acp

import (
	"net/url"
	"strings"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/shared"
)

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
	request = applyProductionGatewayRouting(request)
	request.ReportedRemoteAddress = resolveGatewayReportedRemoteAddress(server, request)
	result := server.gateway.Connect(request, notify)
	return map[string]any{
		"ok":                  result.OK,
		"snapshot":            result.Snapshot,
		"auth":                result.Auth,
		"returnedDeviceToken": result.ReturnedDeviceToken,
		"error":               result.Error,
	}
}

func applyProductionGatewayRouting(
	request gatewayruntime.ConnectRequest,
) gatewayruntime.ConnectRequest {
	if strings.TrimSpace(strings.ToLower(request.Mode)) != "openclaw" {
		return request
	}
	if request.Endpoint.Host == "localhost" || request.Endpoint.Host == "127.0.0.1" || request.Endpoint.Host == "0.0.0.0" {
		return request
	}
	// Route through the unified bridge ingress
	request.Endpoint = gatewayruntime.Endpoint{
		Host: "xworkmate-bridge.svc.plus",
		Port: 443,
		TLS:  true,
	}
	request.Auth.Token = strings.TrimSpace(bridgeUpstreamAuthorizationHeader())
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
	server.gateway.Disconnect(
		strings.TrimSpace(shared.StringArg(params, "runtimeId", "")),
		notify,
	)
	return map[string]any{"accepted": true}
}

func parseGatewayRuntimeStringSlice(value any) []string {
	list, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...)
		}
		return nil
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		text := strings.TrimSpace(shared.StringArg(map[string]any{"value": item}, "value", ""))
		if text == "" {
			continue
		}
		result = append(result, text)
	}
	return result
}

func parseBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return shared.BoolArg(typed, false)
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func parsePositiveInt(value any) int {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case string:
		return shared.IntArg(typed, 0)
	}
	return 0
}

func resolveGatewayReportedRemoteAddress(
	server *Server,
	request gatewayruntime.ConnectRequest,
) string {
	if strings.TrimSpace(strings.ToLower(request.Mode)) != "openclaw" {
		return ""
	}
	gatewayURL := resolveURL(server.config.Upstream.GatewayURL, "GATEWAY_RPC_URL")
	return publicEndpointAddressLabel(gatewayURL)
}

func publicEndpointAddressLabel(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(parsed.Hostname()) == "" {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	port := strings.TrimSpace(parsed.Port())
	if port == "" {
		switch strings.TrimSpace(strings.ToLower(parsed.Scheme)) {
		case "https", "wss":
			port = "443"
		case "http", "ws":
			port = "80"
		}
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}
