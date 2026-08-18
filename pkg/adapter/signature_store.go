package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const claudeCodeSessionHeader = "X-Claude-Code-Session-Id"

type signatureEntry struct {
	value     string
	updatedAt time.Time
}

type sessionSignatures struct {
	updatedAt time.Time
	entries   map[string]signatureEntry
}

type signatureStore struct {
	mu          sync.Mutex
	ttl         time.Duration
	maxSessions int
	maxEntries  int
	sessions    map[string]*sessionSignatures
}

func newSignatureStore(ttl time.Duration, maxSessions, maxEntries int) *signatureStore {
	return &signatureStore{
		ttl: ttl, maxSessions: maxSessions, maxEntries: maxEntries,
		sessions: make(map[string]*sessionSignatures),
	}
}

func signatureSessionKey(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

func (s *signatureStore) snapshot(sessionID string) map[string]string {
	key := signatureSessionKey(sessionID)
	if key == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup(now)
	session := s.sessions[key]
	if session == nil {
		return nil
	}
	result := make(map[string]string, len(session.entries))
	for entryKey, entry := range session.entries {
		result[entryKey] = entry.value
	}
	return result
}

func (s *signatureStore) remember(sessionID string, signatures map[string]string) {
	key := signatureSessionKey(sessionID)
	if key == "" || len(signatures) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup(now)
	session := s.sessions[key]
	if session == nil {
		if len(s.sessions) >= s.maxSessions {
			s.evictOldestSession()
		}
		session = &sessionSignatures{entries: make(map[string]signatureEntry)}
		s.sessions[key] = session
	}
	for entryKey, value := range signatures {
		if entryKey == "" || value == "" {
			continue
		}
		if len(session.entries) >= s.maxEntries {
			if _, exists := session.entries[entryKey]; !exists {
				evictOldestSignature(session.entries)
			}
		}
		session.entries[entryKey] = signatureEntry{value: value, updatedAt: now}
	}
	session.updatedAt = now
}

func (s *signatureStore) cleanup(now time.Time) {
	for key, session := range s.sessions {
		if now.Sub(session.updatedAt) > s.ttl {
			delete(s.sessions, key)
		}
	}
}

func (s *signatureStore) evictOldestSession() {
	var oldestKey string
	var oldest time.Time
	for key, session := range s.sessions {
		if oldestKey == "" || session.updatedAt.Before(oldest) {
			oldestKey, oldest = key, session.updatedAt
		}
	}
	delete(s.sessions, oldestKey)
}

func evictOldestSignature(entries map[string]signatureEntry) {
	var oldestKey string
	var oldest time.Time
	for key, entry := range entries {
		if oldestKey == "" || entry.updatedAt.Before(oldest) {
			oldestKey, oldest = key, entry.updatedAt
		}
	}
	delete(entries, oldestKey)
}
