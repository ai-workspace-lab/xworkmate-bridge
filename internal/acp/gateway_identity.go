package acp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
)

var bridgeGatewayIdentity = struct {
	sync.Mutex
	value       gatewayruntime.DeviceIdentity
	deviceToken string
}{}

type storedBridgeGatewayIdentity struct {
	Version             int    `json:"version"`
	DeviceID            string `json:"deviceId"`
	PublicKeyBase64URL  string `json:"publicKeyBase64Url"`
	PrivateKeyBase64URL string `json:"privateKeyBase64Url"`
	CreatedAtMs         int64  `json:"createdAtMs"`
	DeviceToken         string `json:"deviceToken,omitempty"`
}

func newBridgeGatewayIdentity() gatewayruntime.DeviceIdentity {
	identity, _ := bridgeGatewayOpenClawCredentials()
	return identity
}

func bridgeGatewayOpenClawCredentials() (gatewayruntime.DeviceIdentity, string) {
	bridgeGatewayIdentity.Lock()
	defer bridgeGatewayIdentity.Unlock()
	if strings.TrimSpace(bridgeGatewayIdentity.value.DeviceID) != "" {
		return bridgeGatewayIdentity.value, bridgeGatewayIdentity.deviceToken
	}
	identity, deviceToken, ok := loadBridgeGatewayIdentity()
	if !ok {
		identity = generateBridgeGatewayIdentity()
		deviceToken = ""
	}
	bridgeGatewayIdentity.value = identity
	bridgeGatewayIdentity.deviceToken = deviceToken
	return bridgeGatewayIdentity.value, bridgeGatewayIdentity.deviceToken
}

func generateBridgeGatewayIdentity() gatewayruntime.DeviceIdentity {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return gatewayruntime.DeviceIdentity{}
	}
	return gatewayruntime.DeviceIdentity{
		DeviceID:            deriveOpenClawDeviceID(publicKey),
		PublicKeyBase64URL:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKeyBase64URL: base64.RawURLEncoding.EncodeToString(privateKey),
	}
}

func loadBridgeGatewayIdentity() (gatewayruntime.DeviceIdentity, string, bool) {
	path := bridgeGatewayIdentityPath()
	data, err := os.ReadFile(path)
	if err != nil {
		identity := generateBridgeGatewayIdentity()
		return identity, "", persistBridgeGatewayIdentity(path, identity, "")
	}
	var stored storedBridgeGatewayIdentity
	if err := json.Unmarshal(data, &stored); err != nil {
		identity := generateBridgeGatewayIdentity()
		return identity, "", persistBridgeGatewayIdentity(path, identity, "")
	}
	identity := gatewayruntime.DeviceIdentity{
		DeviceID:            strings.TrimSpace(stored.DeviceID),
		PublicKeyBase64URL:  strings.TrimSpace(stored.PublicKeyBase64URL),
		PrivateKeyBase64URL: strings.TrimSpace(stored.PrivateKeyBase64URL),
	}
	if normalized, ok := normalizeBridgeGatewayIdentity(identity); ok {
		if normalized.DeviceID != identity.DeviceID {
			_ = persistBridgeGatewayIdentity(path, normalized, strings.TrimSpace(stored.DeviceToken))
		}
		return normalized, strings.TrimSpace(stored.DeviceToken), true
	}
	identity = generateBridgeGatewayIdentity()
	return identity, "", persistBridgeGatewayIdentity(path, identity, "")
}

func normalizeBridgeGatewayIdentity(identity gatewayruntime.DeviceIdentity) (gatewayruntime.DeviceIdentity, bool) {
	publicKey, err := decodeRawBase64URL(identity.PublicKeyBase64URL)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return gatewayruntime.DeviceIdentity{}, false
	}
	privateKey, err := decodeRawBase64URL(identity.PrivateKeyBase64URL)
	if err != nil {
		return gatewayruntime.DeviceIdentity{}, false
	}
	var edPrivateKey ed25519.PrivateKey
	switch len(privateKey) {
	case ed25519.PrivateKeySize:
		edPrivateKey = ed25519.PrivateKey(privateKey)
	case ed25519.SeedSize:
		edPrivateKey = ed25519.NewKeyFromSeed(privateKey)
	default:
		return gatewayruntime.DeviceIdentity{}, false
	}
	derivedPublic, ok := edPrivateKey.Public().(ed25519.PublicKey)
	if !ok || subtle.ConstantTimeCompare(derivedPublic, publicKey) != 1 {
		return gatewayruntime.DeviceIdentity{}, false
	}
	identity.DeviceID = deriveOpenClawDeviceID(publicKey)
	identity.PublicKeyBase64URL = base64.RawURLEncoding.EncodeToString(publicKey)
	identity.PrivateKeyBase64URL = base64.RawURLEncoding.EncodeToString(edPrivateKey)
	return identity, true
}

func saveBridgeGatewayDeviceToken(deviceToken string) {
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return
	}
	bridgeGatewayIdentity.Lock()
	defer bridgeGatewayIdentity.Unlock()
	if strings.TrimSpace(bridgeGatewayIdentity.value.DeviceID) == "" {
		return
	}
	bridgeGatewayIdentity.deviceToken = deviceToken
	_ = persistBridgeGatewayIdentity(
		bridgeGatewayIdentityPath(),
		bridgeGatewayIdentity.value,
		deviceToken,
	)
}

func clearBridgeGatewayDeviceToken() {
	bridgeGatewayIdentity.Lock()
	defer bridgeGatewayIdentity.Unlock()
	if strings.TrimSpace(bridgeGatewayIdentity.value.DeviceID) == "" {
		return
	}
	bridgeGatewayIdentity.deviceToken = ""
	_ = persistBridgeGatewayIdentity(
		bridgeGatewayIdentityPath(),
		bridgeGatewayIdentity.value,
		"",
	)
}

func persistBridgeGatewayIdentity(
	path string,
	identity gatewayruntime.DeviceIdentity,
	deviceToken string,
) bool {
	if strings.TrimSpace(identity.DeviceID) == "" ||
		strings.TrimSpace(identity.PublicKeyBase64URL) == "" ||
		strings.TrimSpace(identity.PrivateKeyBase64URL) == "" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	payload := storedBridgeGatewayIdentity{
		Version:             1,
		DeviceID:            identity.DeviceID,
		PublicKeyBase64URL:  identity.PublicKeyBase64URL,
		PrivateKeyBase64URL: identity.PrivateKeyBase64URL,
		CreatedAtMs:         time.Now().UnixMilli(),
		DeviceToken:         strings.TrimSpace(deviceToken),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return false
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return false
	}
	_ = os.Chmod(path, 0o600)
	return true
}

func bridgeGatewayIdentityPath() string {
	if path := strings.TrimSpace(os.Getenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		home = "."
	}
	return filepath.Join(home, ".xworkmate-bridge", "openclaw-device.json")
}

func deriveOpenClawDeviceID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])
}

func isOpenClawMode(mode string) bool {
	return strings.TrimSpace(strings.ToLower(mode)) == "openclaw"
}

func decodeRawBase64URL(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
}
