package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu   sync.Mutex
	path string
	data map[string]conversation
}

func openSessionStore() *sessionStore {
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-native-sessions.json")
	}
	s := &sessionStore{path: path, data: map[string]conversation{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	return s
}

func (s *sessionStore) saveLocked() {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	_ = os.WriteFile(s.path, b, 0o600)
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		out = append(out, v)
	}
	return out
}

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *sessionStore) upsert(v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[v.ID] = v
	s.saveLocked()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return false
	}
	delete(s.data, id)
	s.saveLocked()
	return true
}

type userSession struct {
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	AccountID      string    `json:"accountId"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

type userSessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]userSession
	ttl     time.Duration
}

func openUserSessionStore(ttl time.Duration) *userSessionStore {
	path := os.Getenv("M365_USER_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-native-user-sessions.json")
	}
	s := &userSessionStore{path: path, data: map[string]userSession{}, ttl: ttl}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	s.evictLocked()
	return s
}

func (s *userSessionStore) saveLocked() {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	_ = os.WriteFile(s.path, b, 0o600)
}

func (s *userSessionStore) evictLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.ttl)
	for k, v := range s.data {
		if v.LastUsedAt.Before(cutoff) {
			delete(s.data, k)
		}
	}
}

func (s *userSessionStore) Get(user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	v, ok := s.data[user]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[user] = v
		s.saveLocked()
	}
	return v, ok
}

func (s *userSessionStore) Put(user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[user] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.saveLocked()
}

func (s *userSessionStore) Delete(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, user)
	s.saveLocked()
}
