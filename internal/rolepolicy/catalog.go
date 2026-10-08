package rolepolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// FetchCatalog reads GET {base URL}/models (the base URL already ends in /v1)
// with the connection's token and keeps every data[].id byte-for-byte.
// The result only says what the token can see; it proves nothing about
// tools, context, search or quality.
func FetchCatalog(ctx context.Context, client *http.Client, connectionID string, conn Connection, now time.Time) CatalogSnapshot {
	snapshot := CatalogSnapshot{ConnectionID: connectionID, FetchedAt: now}
	token := strings.TrimSpace(os.Getenv(conn.TokenEnv))
	if token == "" {
		snapshot.Err = "token env " + conn.TokenEnv + " is not set"
		return snapshot
	}
	baseURL := conn.ResolvedBaseURL()
	if baseURL == "" {
		snapshot.Err = "base url env " + conn.BaseURLEnv + " is not set"
		return snapshot
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		snapshot.Err = err.Error()
		return snapshot
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		snapshot.Err = err.Error()
		return snapshot
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("[rolepolicy] close /models body for %s: %v", connectionID, closeErr)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		snapshot.Err = err.Error()
		return snapshot
	}
	if resp.StatusCode != http.StatusOK {
		snapshot.Err = fmt.Sprintf("GET %s returned %d", endpoint, resp.StatusCode)
		return snapshot
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		snapshot.Err = "decode /models: " + err.Error()
		return snapshot
	}
	for _, item := range decoded.Data {
		if item.ID != "" {
			snapshot.ModelIDs = append(snapshot.ModelIDs, item.ID)
		}
	}
	return snapshot
}
