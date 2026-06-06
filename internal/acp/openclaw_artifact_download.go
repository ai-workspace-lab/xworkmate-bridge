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

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/shared"
)

const (
	openClawArtifactDownloadPath     = "/artifacts/openclaw/download"
	openClawArtifactDownloadTTL      = 24 * time.Hour
	openClawArtifactDownloadMaxBytes = 64 * 1024 * 1024
	openClawArtifactReadAttempts     = 3
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
	sessionKey := strings.TrimSpace(query.Get("openclawSessionKey"))
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(query.Get("sessionKey"))
	}
	runID := strings.TrimSpace(query.Get("runId"))
	rawArtifactScope := strings.TrimSpace(query.Get("artifactScope"))
	artifactScope := safeOpenClawArtifactDownloadArtifactScope(rawArtifactScope)
	relativePath := safeOpenClawArtifactDownloadRelativePath(query.Get("relativePath"))
	expires := strings.TrimSpace(query.Get("expires"))
	signature := strings.TrimSpace(query.Get("sig"))
	if sessionKey == "" || runID == "" || relativePath == "" || expires == "" || signature == "" {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32602, "missing artifact download parameters")
		return
	}
	if rawArtifactScope != "" && artifactScope == "" {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32602, "invalid artifact scope")
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
	if !validOpenClawArtifactDownloadSignature(sessionKey, runID, artifactScope, relativePath, expires, signature) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32042, "invalid artifact download signature")
		return
	}

	if rpcErr := ensureProductionGatewayConnected(s, "openclaw", nil); rpcErr != nil {
		shared.WriteJSONError(w, nil, http.StatusBadGateway, rpcErr.Code, rpcErr.Message)
		return
	}
	readParams := map[string]any{
		"openclawSessionKey": sessionKey,
		"sessionKey":         sessionKey,
		"runId":              runID,
		"relativePath":       relativePath,
		"maxInlineBytes":     openClawArtifactDownloadMaxBytes,
	}
	if artifactScope != "" {
		readParams["artifactScope"] = artifactScope
	}
	readResult := s.readOpenClawArtifactWithRetry(
		"openclaw",
		readParams,
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
	artifact, ok := firstOpenClawArtifactForPath(payload, remoteWorkingDirectory, artifactScope, relativePath)
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
	rangeStart, rangeEnd, partialContent, rangeOK := openClawArtifactContentRange(
		r.Header.Get("Range"),
		len(content),
	)
	if !rangeOK {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
		shared.WriteJSONError(w, nil, http.StatusRequestedRangeNotSatisfiable, -32049, "invalid artifact range")
		return
	}
	body := content
	statusCode := http.StatusOK
	if partialContent {
		body = content[rangeStart : rangeEnd+1]
		statusCode = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, len(content)))
	}
	filename := filepath.Base(relativePath)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, "")))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func (s *Server) readOpenClawArtifactWithRetry(
	gatewayProvider string,
	readParams map[string]any,
	notify func(map[string]any),
) gatewayruntime.RequestResult {
	var readResult gatewayruntime.RequestResult
	for attempt := 1; attempt <= openClawArtifactReadAttempts; attempt++ {
		readResult = s.gateway.RequestByMode(
			gatewayProvider,
			"xworkmate.artifacts.read",
			readParams,
			time.Minute,
			notify,
		)
		if readResult.OK {
			return readResult
		}
		message := strings.TrimSpace(shared.StringArg(readResult.Error, "message", ""))
		if openClawArtifactReadMissing(readResult.Error, message) || attempt == openClawArtifactReadAttempts {
			return readResult
		}
		time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
	}
	return readResult
}

func (s *Server) decorateOpenClawArtifactDownloadURLs(result map[string]any, sessionKey string, runID string) {
	if result == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	resultArtifactScope := safeOpenClawArtifactDownloadArtifactScope(shared.StringArg(result, "artifactScope", ""))
	for _, key := range []string{"artifacts", "files", "attachments"} {
		switch items := result[key].(type) {
		case []any:
			for index, item := range items {
				mapped := shared.AsMap(item)
				if len(mapped) == 0 {
					continue
				}
				s.decorateOpenClawArtifactDownloadURL(mapped, sessionKey, runID, resultArtifactScope)
				items[index] = mapped
			}
			result[key] = items
		case []map[string]any:
			for _, mapped := range items {
				s.decorateOpenClawArtifactDownloadURL(mapped, sessionKey, runID, resultArtifactScope)
			}
		}
	}
}

func stripOpenClawArtifactInlineContent(result map[string]any) {
	if result == nil {
		return
	}
	for _, key := range []string{"artifacts", "files", "attachments"} {
		switch items := result[key].(type) {
		case []any:
			for index, item := range items {
				mapped := shared.AsMap(item)
				stripOpenClawArtifactMapInlineContent(mapped)
				if mapped != nil {
					items[index] = mapped
				}
			}
			result[key] = items
		case []map[string]any:
			for _, item := range items {
				stripOpenClawArtifactMapInlineContent(item)
			}
		}
	}
}

func stripOpenClawArtifactMapInlineContent(artifact map[string]any) {
	if artifact == nil {
		return
	}
	delete(artifact, "encoding")
	delete(artifact, "content")
}

func (s *Server) decorateOpenClawArtifactDownloadURL(
	artifact map[string]any,
	sessionKey string,
	runID string,
	resultArtifactScope string,
) {
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
	artifactScope := safeOpenClawArtifactDownloadArtifactScope(shared.StringArg(artifact, "artifactScope", ""))
	if artifactScope == "" {
		artifactScope = resultArtifactScope
	}
	downloadURL := s.openClawArtifactDownloadURL(sessionKey, runID, artifactScope, relativePath, time.Now())
	if downloadURL == "" {
		return
	}
	artifact["relativePath"] = relativePath
	if artifactScope != "" {
		artifact["artifactScope"] = artifactScope
	}
	artifact["downloadUrl"] = downloadURL
	delete(artifact, "downloadURL")
	delete(artifact, "download_url")
}

func (s *Server) openClawArtifactDownloadURL(
	sessionKey string,
	runID string,
	artifactScope string,
	relativePath string,
	now time.Time,
) string {
	sessionKey = strings.TrimSpace(sessionKey)
	runID = strings.TrimSpace(runID)
	rawArtifactScope := strings.TrimSpace(artifactScope)
	artifactScope = safeOpenClawArtifactDownloadArtifactScope(artifactScope)
	relativePath = safeOpenClawArtifactDownloadRelativePath(relativePath)
	if rawArtifactScope != "" && artifactScope == "" {
		return ""
	}
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
	query.Set("openclawSessionKey", sessionKey)
	query.Set("sessionKey", sessionKey)
	query.Set("runId", runID)
	query.Set("artifactScope", artifactScope)
	query.Set("relativePath", relativePath)
	query.Set("expires", expires)
	query.Set("sig", signOpenClawArtifactDownload(sessionKey, runID, artifactScope, relativePath, expires))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func firstOpenClawArtifactForPath(
	payload map[string]any,
	remoteWorkingDirectory string,
	artifactScope string,
	relativePath string,
) (map[string]any, bool) {
	artifacts := extractArtifactPayloads(payload, remoteWorkingDirectory)
	for _, artifact := range artifacts {
		if artifactScope != "" &&
			safeOpenClawArtifactDownloadArtifactScope(shared.StringArg(artifact, "artifactScope", "")) != artifactScope {
			continue
		}
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

func safeOpenClawArtifactDownloadArtifactScope(rawScope string) string {
	scope := safeOpenClawArtifactDownloadRelativePath(rawScope)
	if scope == "" {
		return ""
	}
	parts := strings.Split(scope, "/")
	if len(parts) != 3 || parts[0] != "tasks" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return scope
}

func validOpenClawArtifactDownloadSignature(
	sessionKey string,
	runID string,
	artifactScope string,
	relativePath string,
	expires string,
	signature string,
) bool {
	expected := signOpenClawArtifactDownload(sessionKey, runID, artifactScope, relativePath, expires)
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

func signOpenClawArtifactDownload(
	sessionKey string,
	runID string,
	artifactScope string,
	relativePath string,
	expires string,
) string {
	secret := openClawArtifactSigningSecret()
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	artifactScope = safeOpenClawArtifactDownloadArtifactScope(artifactScope)
	_, _ = mac.Write([]byte(strings.Join([]string{
		strings.TrimSpace(sessionKey),
		strings.TrimSpace(runID),
		artifactScope,
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

func openClawArtifactContentRange(rawRange string, contentLength int) (int, int, bool, bool) {
	if strings.TrimSpace(rawRange) == "" {
		return 0, contentLength - 1, false, true
	}
	if contentLength <= 0 {
		return 0, 0, false, false
	}
	rawRange = strings.TrimSpace(rawRange)
	if !strings.HasPrefix(rawRange, "bytes=") || strings.Contains(rawRange, ",") {
		return 0, 0, false, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(rawRange, "bytes="))
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, false
	}
	startRaw := strings.TrimSpace(parts[0])
	endRaw := strings.TrimSpace(parts[1])
	if startRaw == "" {
		suffixLength, err := strconv.Atoi(endRaw)
		if err != nil || suffixLength <= 0 {
			return 0, 0, false, false
		}
		start := contentLength - suffixLength
		if start < 0 {
			start = 0
		}
		return start, contentLength - 1, true, true
	}
	start, err := strconv.Atoi(startRaw)
	if err != nil || start < 0 || start >= contentLength {
		return 0, 0, false, false
	}
	end := contentLength - 1
	if endRaw != "" {
		end, err = strconv.Atoi(endRaw)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		if end >= contentLength {
			end = contentLength - 1
		}
	}
	return start, end, true, true
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
