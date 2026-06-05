package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

type ThreadSessionMapper struct {
	mu       sync.Mutex
	sessions map[string]string
}

func NewThreadSessionMapper() *ThreadSessionMapper {
	return &ThreadSessionMapper{sessions: make(map[string]string)}
}

func (m *ThreadSessionMapper) OpenClawSessionID(threadID string, sessionID string) string {
	key := strings.TrimSpace(threadID)
	if key == "" {
		key = strings.TrimSpace(sessionID)
	}
	if key == "" {
		key = "main"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := strings.TrimSpace(m.sessions[key]); existing != "" {
		return existing
	}
	sum := sha256.Sum256([]byte(key))
	session := "xwm-" + hex.EncodeToString(sum[:])[:24]
	m.sessions[key] = session
	return session
}
