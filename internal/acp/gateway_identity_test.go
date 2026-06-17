package acp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xworkmate-bridge/internal/gatewayruntime"
)

func TestBridgeGatewayIdentityUsesOpenClawDeviceFingerprint(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "openclaw-device.json")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", identityPath)
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	identity := newBridgeGatewayIdentity()
	if strings.HasPrefix(identity.DeviceID, "xworkmate-bridge-") {
		t.Fatalf("device id must not use legacy bridge prefix: %q", identity.DeviceID)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(identity.PublicKeyBase64URL)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	sum := sha256.Sum256(publicKey)
	if got, want := identity.DeviceID, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("device id = %q, want public-key fingerprint %q", got, want)
	}
	info, err := os.Stat(identityPath)
	if err != nil {
		t.Fatalf("identity file was not created: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity file mode = %o, want 600", got)
	}

	resetBridgeGatewayIdentityForTest()
	reloaded := newBridgeGatewayIdentity()
	if reloaded.DeviceID != identity.DeviceID {
		t.Fatalf("identity should be stable across reloads: got %q want %q", reloaded.DeviceID, identity.DeviceID)
	}
}

func TestBridgeGatewayIdentityCorrectsMismatchedStoredDeviceID(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "openclaw-device.json")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", identityPath)
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(identityPath), 0o700); err != nil {
		t.Fatalf("create identity dir: %v", err)
	}
	writeStoredIdentity(t, identityPath, storedBridgeGatewayIdentity{
		Version:             1,
		DeviceID:            "legacy-wrong-device-id",
		PublicKeyBase64URL:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKeyBase64URL: base64.RawURLEncoding.EncodeToString(privateKey),
		CreatedAtMs:         1,
		DeviceToken:         "device-token-1",
	})

	identity, token := bridgeGatewayOpenClawCredentials()
	sum := sha256.Sum256(publicKey)
	if got, want := identity.DeviceID, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("device id = %q, want corrected fingerprint %q", got, want)
	}
	if token != "device-token-1" {
		t.Fatalf("expected stored device token to survive correction, got %q", token)
	}
	var stored storedBridgeGatewayIdentity
	readStoredIdentity(t, identityPath, &stored)
	if stored.DeviceID != identity.DeviceID {
		t.Fatalf("stored device id was not corrected: %#v", stored)
	}
}

func TestBridgeGatewayIdentityRegeneratesInvalidStoredKeyPair(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "openclaw-device.json")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", identityPath)
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate public key: %v", err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(identityPath), 0o700); err != nil {
		t.Fatalf("create identity dir: %v", err)
	}
	writeStoredIdentity(t, identityPath, storedBridgeGatewayIdentity{
		Version:             1,
		DeviceID:            "bad",
		PublicKeyBase64URL:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKeyBase64URL: base64.RawURLEncoding.EncodeToString(privateKey),
		CreatedAtMs:         1,
		DeviceToken:         "stale-token",
	})

	identity, token := bridgeGatewayOpenClawCredentials()
	if identity.DeviceID == "bad" || token != "" {
		t.Fatalf("expected invalid identity to regenerate and drop token, got identity=%#v token=%q", identity, token)
	}
	publicKeyBytes, err := base64.RawURLEncoding.DecodeString(identity.PublicKeyBase64URL)
	if err != nil {
		t.Fatalf("decode regenerated public key: %v", err)
	}
	sum := sha256.Sum256(publicKeyBytes)
	if identity.DeviceID != hex.EncodeToString(sum[:]) {
		t.Fatalf("regenerated device id does not match public key")
	}
}

func TestBridgeGatewayIdentityPersistsReturnedDeviceToken(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "openclaw-device.json")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", identityPath)
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	identity := newBridgeGatewayIdentity()
	saveBridgeGatewayDeviceToken("device-token-2")
	resetBridgeGatewayIdentityForTest()

	reloaded, token := bridgeGatewayOpenClawCredentials()
	if reloaded.DeviceID != identity.DeviceID {
		t.Fatalf("reloaded identity = %q, want %q", reloaded.DeviceID, identity.DeviceID)
	}
	if token != "device-token-2" {
		t.Fatalf("device token = %q, want device-token-2", token)
	}
}

func TestBridgeGatewayIdentityClearsStoredDeviceToken(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "openclaw-device.json")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", identityPath)
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	identity := newBridgeGatewayIdentity()
	saveBridgeGatewayDeviceToken("device-token-2")
	clearBridgeGatewayDeviceToken()
	resetBridgeGatewayIdentityForTest()

	reloaded, token := bridgeGatewayOpenClawCredentials()
	if reloaded.DeviceID != identity.DeviceID {
		t.Fatalf("reloaded identity = %q, want %q", reloaded.DeviceID, identity.DeviceID)
	}
	if token != "" {
		t.Fatalf("device token should be cleared, got %q", token)
	}
}

func resetBridgeGatewayIdentityForTest() {
	bridgeGatewayIdentity.Lock()
	defer bridgeGatewayIdentity.Unlock()
	bridgeGatewayIdentity.value = gatewayruntime.DeviceIdentity{}
	bridgeGatewayIdentity.deviceToken = ""
}

func writeStoredIdentity(t *testing.T, path string, stored storedBridgeGatewayIdentity) {
	t.Helper()
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
}

func readStoredIdentity(t *testing.T, path string, target *storedBridgeGatewayIdentity) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("unmarshal identity: %v", err)
	}
}
