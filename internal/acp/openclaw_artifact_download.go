package acp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	openClawArtifactDownloadPath     = "/artifacts/openclaw/download"
	openClawArtifactDownloadTTL      = 24 * time.Hour
	openClawArtifactDownloadMaxBytes = 64 * 1024 * 1024
	defaultBridgePublicURL           = "https://xworkmate-bridge.svc.plus"
)

func (s *Server) HandleOpenClawArtifactDownload(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		shared.WriteJSONError(w, nil, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}
	if !shared.OriginAllowed(strings.TrimSpace(r.Header.Get("Origin")), s.allowedOrigins) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32003, "origin not allowed")
		return
	}
	if !s.authorized(r) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}

	query := r.URL.Query()
	sessionKey := strings.TrimSpace(query.Get("sessionKey"))
	runID := strings.TrimSpace(query.Get("runId"))
	relativePath := safeOpenClawArtifactDownloadRelativePath(query.Get("relativePath"))
	expires := strings.TrimSpace(query.Get("expires"))
	signature := strings.TrimSpace(query.Get("sig"))
	if sessionKey == "" || runID == "" || relativePath == "" || expires == "" || signature == "" {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32602, "missing artifact download parameters")
		return
	}
	expiresUnix, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || expiresUnix <= 0 {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32602, "invalid artifact download expiry")
		return
	}
	if time.Now().Unix() > expiresUnix {
		shared.WriteJSONError(w, nil, http.StatusGone, -32041, "artifact download link expired")
		return
	}
	if !validOpenClawArtifactDownloadSignature(sessionKey, runID, relativePath, expires, signature) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32042, "invalid artifact download signature")
		return
	}

	if rpcErr := ensureProductionGatewayConnected(s, "openclaw", nil); rpcErr != nil {
		shared.WriteJSONError(w, nil, http.StatusBadGateway, rpcErr.Code, rpcErr.Message)
		return
	}
	readResult := s.gateway.RequestByMode(
		"openclaw",
		"xworkmate.artifacts.read",
		map[string]any{
			"sessionKey":     sessionKey,
			"runId":          runID,
			"relativePath":   relativePath,
			"maxInlineBytes": openClawArtifactDownloadMaxBytes,
		},
		time.Minute,
		nil,
	)
	if !readResult.OK {
		message := strings.TrimSpace(shared.StringArg(readResult.Error, "message", "openclaw artifact read failed"))
		if openClawArtifactReadMissing(readResult.Error, message) {
			shared.WriteJSONError(w, nil, http.StatusNotFound, -32044, "artifact_missing")
			return
		}
		shared.WriteJSONError(w, nil, http.StatusBadGateway, -32043, message)
		return
	}

	payload := shared.AsMap(readResult.Payload)
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", ""))
	artifact, ok := firstOpenClawArtifactForPath(payload, remoteWorkingDirectory, relativePath)
	if !ok {
		shared.WriteJSONError(w, nil, http.StatusNotFound, -32044, "artifact_missing")
		return
	}
	if got := safeOpenClawArtifactDownloadRelativePath(shared.StringArg(artifact, "relativePath", "")); got != relativePath {
		shared.WriteJSONError(w, nil, http.StatusBadGateway, -32045, "artifact read returned mismatched path")
		return
	}
	if strings.TrimSpace(shared.StringArg(artifact, "encoding", "")) != "base64" {
		shared.WriteJSONError(w, nil, http.StatusRequestEntityTooLarge, -32046, "artifact content is not inline; maximum download size exceeded")
		return
	}
	rawContent := strings.TrimSpace(shared.StringArg(artifact, "content", ""))
	if rawContent == "" {
		shared.WriteJSONError(w, nil, http.StatusRequestEntityTooLarge, -32046, "artifact content unavailable")
		return
	}
	content, err := base64.StdEncoding.DecodeString(rawContent)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadGateway, -32047, "artifact content is not valid base64")
		return
	}
	if len(content) > openClawArtifactDownloadMaxBytes {
		shared.WriteJSONError(w, nil, http.StatusRequestEntityTooLarge, -32046, "artifact exceeds maximum download size")
		return
	}
	if !artifactSHA256Matches(content, shared.StringArg(artifact, "sha256", "")) {
		shared.WriteJSONError(w, nil, http.StatusBadGateway, -32048, "artifact checksum mismatch")
		return
	}

	contentType := strings.TrimSpace(shared.StringArg(artifact, "contentType", ""))
	if contentType == "" {
		contentType = artifactContentType(relativePath)
	}
	filename := filepath.Base(relativePath)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, "")))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) decorateOpenClawArtifactDownloadURLs(result map[string]any, sessionKey string, runID string) {
	if result == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	for _, key := range []string{"artifacts", "files", "attachments"} {
		switch items := result[key].(type) {
		case []any:
			for index, item := range items {
				mapped := shared.AsMap(item)
				if len(mapped) == 0 {
					continue
				}
				s.decorateOpenClawArtifactDownloadURL(mapped, sessionKey, runID)
				items[index] = mapped
			}
			result[key] = items
		case []map[string]any:
			for _, mapped := range items {
				s.decorateOpenClawArtifactDownloadURL(mapped, sessionKey, runID)
			}
		}
	}
}

func (s *Server) decorateOpenClawArtifactDownloadURL(artifact map[string]any, sessionKey string, runID string) {
	if artifact == nil {
		return
	}
	relativePath := strings.TrimSpace(shared.StringArg(artifact, "relativePath", ""))
	if relativePath == "" {
		relativePath = strings.TrimSpace(shared.StringArg(artifact, "path", ""))
	}
	if relativePath == "" {
		relativePath = strings.TrimSpace(shared.StringArg(artifact, "name", ""))
	}
	relativePath = safeOpenClawArtifactDownloadRelativePath(relativePath)
	if relativePath == "" {
		return
	}
	downloadURL := s.openClawArtifactDownloadURL(sessionKey, runID, relativePath, time.Now())
	if downloadURL == "" {
		return
	}
	artifact["relativePath"] = relativePath
	artifact["downloadUrl"] = downloadURL
	delete(artifact, "downloadURL")
	delete(artifact, "download_url")
}

func (s *Server) openClawArtifactDownloadURL(sessionKey string, runID string, relativePath string, now time.Time) string {
	sessionKey = strings.TrimSpace(sessionKey)
	runID = strings.TrimSpace(runID)
	relativePath = safeOpenClawArtifactDownloadRelativePath(relativePath)
	if sessionKey == "" || runID == "" || relativePath == "" || openClawArtifactSigningSecret() == "" {
		return ""
	}
	rawBase := strings.TrimSpace(shared.EnvOrDefault("XWORKMATE_BRIDGE_PUBLIC_URL", ""))
	if rawBase == "" {
		rawBase = strings.TrimSpace(shared.EnvOrDefault("BRIDGE_PUBLIC_URL", ""))
	}
	if rawBase == "" {
		rawBase = defaultBridgePublicURL
	}
	if !strings.Contains(rawBase, "://") {
		rawBase = "https://" + rawBase
	}
	parsed, err := url.Parse(rawBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + openClawArtifactDownloadPath
	parsed.RawQuery = ""
	expires := strconv.FormatInt(now.Add(openClawArtifactDownloadTTL).Unix(), 10)
	query := parsed.Query()
	query.Set("sessionKey", sessionKey)
	query.Set("runId", runID)
	query.Set("relativePath", relativePath)
	query.Set("expires", expires)
	query.Set("sig", signOpenClawArtifactDownload(sessionKey, runID, relativePath, expires))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func firstOpenClawArtifactForPath(
	payload map[string]any,
	remoteWorkingDirectory string,
	relativePath string,
) (map[string]any, bool) {
	artifacts := extractArtifactPayloads(payload, remoteWorkingDirectory)
	for _, artifact := range artifacts {
		if safeOpenClawArtifactDownloadRelativePath(shared.StringArg(artifact, "relativePath", "")) == relativePath {
			return artifact, true
		}
	}
	return nil, false
}

func safeOpenClawArtifactDownloadRelativePath(rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" || strings.Contains(trimmed, "\x00") {
		return ""
	}
	slashPath := strings.ReplaceAll(trimmed, "\\", "/")
	if path.IsAbs(slashPath) {
		return ""
	}
	for _, segment := range strings.Split(slashPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ""
		}
	}
	cleaned := path.Clean(slashPath)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}

func validOpenClawArtifactDownloadSignature(
	sessionKey string,
	runID string,
	relativePath string,
	expires string,
	signature string,
) bool {
	expected := signOpenClawArtifactDownload(sessionKey, runID, relativePath, expires)
	if expected == "" || signature == "" {
		return false
	}
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return false
	}
	actualBytes, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(expectedBytes, actualBytes)
}

func signOpenClawArtifactDownload(sessionKey string, runID string, relativePath string, expires string) string {
	secret := openClawArtifactSigningSecret()
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.Join([]string{
		strings.TrimSpace(sessionKey),
		strings.TrimSpace(runID),
		safeOpenClawArtifactDownloadRelativePath(relativePath),
		strings.TrimSpace(expires),
	}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func openClawArtifactSigningSecret() string {
	for _, key := range []string{
		"XWORKMATE_ARTIFACT_DOWNLOAD_SIGNING_SECRET",
		"BRIDGE_ARTIFACT_SIGNING_SECRET",
		"BRIDGE_AUTH_TOKEN",
	} {
		if value := strings.TrimSpace(shared.EnvOrDefault(key, "")); value != "" {
			return value
		}
	}
	return ""
}

func artifactSHA256Matches(content []byte, expected string) bool {
	expected = strings.TrimSpace(strings.ToLower(expected))
	if expected == "" || len(expected) != 64 {
		return true
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return true
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]) == expected
}

func openClawArtifactReadMissing(errorPayload map[string]any, message string) bool {
	code := strings.ToLower(strings.TrimSpace(shared.StringArg(errorPayload, "code", "")))
	detailCode := strings.ToLower(strings.TrimSpace(shared.StringArg(shared.AsMap(errorPayload["details"]), "code", "")))
	message = strings.ToLower(strings.TrimSpace(message))
	for _, value := range []string{code, detailCode, message} {
		if strings.Contains(value, "artifact_not_found") ||
			strings.Contains(value, "not_found") ||
			strings.Contains(value, "not found") ||
			strings.Contains(value, "no such file") {
			return true
		}
	}
	return false
}
