package gatewayruntime

import (
	"testing"
	"time"
)

func TestManagerConnectUsesReportedRemoteAddressInSnapshot(t *testing.T) {
	t.Parallel()

	server := newFakeGatewayServer(t)
	defer server.Close()

	manager := NewManager()
	manager.ReconnectDelay = 20 * time.Millisecond

	result := manager.Connect(ConnectRequest{
		RuntimeID: "runtime-1",
		Mode:      "openclaw",
		ClientID:  "openclaw-macos",
		Locale:    "en_US",
		UserAgent: "XWorkmate/1.0.0",
		Endpoint: Endpoint{
			Host: "127.0.0.1",
			Port: server.Port(),
			TLS:  false,
		},
		ReportedRemoteAddress: "xworkmate-bridge.svc.plus:443",
		ConnectAuthMode:       "shared-token",
		ConnectAuthFields:     []string{"token"},
		ConnectAuthSources:    []string{"shared:form"},
		HasSharedAuth:         true,
		HasDeviceToken:        false,
		PackageInfo: PackageInfo{
			AppName: "XWorkmate",
			Version: "1.0.0",
		},
		DeviceInfo: DeviceInfo{
			Platform:        "macos",
			PlatformVersion: "14.0",
			DeviceFamily:    "Mac",
			ModelIdentifier: "Mac14,5",
		},
		Identity: DeviceIdentity{
			DeviceID:            "device-1",
			PublicKeyBase64URL:  "tl4fnKW7VLD0Cl4lQTu2CEgHPs4PWAX7eVgWfWQWk2Q",
			PrivateKeyBase64URL: "dr7GfMKoO-lJBtgA0dE5m6f_X4kEFsxChDc7mW8mkXu2Xh-cpbsUsPQKXiVBO7YISAc-zg9YBft5WBZ9ZBaTZA",
		},
		Auth: AuthConfig{
			Token: "shared-token",
		},
	}, func(map[string]any) {})
	if !result.OK {
		t.Fatalf("expected connect success, got %#v", result.Error)
	}

	if got := result.Snapshot["remoteAddress"]; got != "xworkmate-bridge.svc.plus:443" {
		t.Fatalf("expected reported remote address, got %#v", got)
	}
}
