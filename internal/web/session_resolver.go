package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type sessionBinding struct {
	SessionID      string    `json:"sessionId"`
	ConversationID string    `json:"conversationId"`
	AccountID      string    `json:"accountId"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
	IPFingerprint  string    `json:"ipFingerprint,omitempty"`
	UserField      string    `json:"userField,omitempty"`
	ContextFinger  string    `json:"contextFinger,omitempty"`
	contextHistory []oaiMsg  `json:"-"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	byExplicit  map[string]string // explicitID -> sessionID
	byUserField map[string]string // userField -> sessionID
	byIPFinger  map[string]string // ipFingerprint -> sessionID
	byContext   map[string]string // contextFingerprint -> sessionID
	ttl         time.Duration
	contextTTL  time.Duration
}

func openSessionResolver() *sessionResolver {
	ttl := 30 * time.Minute
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 5 * time.Minute
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         ttl,
		contextTTL:  contextTTL,
	}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	if b, err := os.ReadFile(sr.path); err == nil {
		var list []sessionBinding
		if err := json.Unmarshal(b, &list); err == nil {
			now := time.Now().UTC()
			for _, s := range list {
				if now.Sub(s.LastUsedAt) > sr.ttl {
					continue
				}
				sr.reindexLocked(s)
			}
		}
	}
}

func (sr *sessionResolver) saveLocked() {
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	sr.sessions[s.SessionID] = s
	if s.UserField != "" {
		sr.byUserField[s.UserField] = s.SessionID
	}
	if s.IPFingerprint != "" {
		sr.byIPFinger[s.IPFingerprint] = s.SessionID
	}
	if s.ContextFinger != "" {
		sr.byContext[s.ContextFinger] = s.SessionID
	}
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			delete(sr.sessions, id)
			if sr.byUserField[s.UserField] == id {
				delete(sr.byUserField, s.UserField)
			}
			if sr.byIPFinger[s.IPFingerprint] == id {
				delete(sr.byIPFinger, s.IPFingerprint)
			}
			if sr.byContext[s.ContextFinger] == id {
				delete(sr.byContext, s.ContextFinger)
			}
		}
	}
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
}

func clientIPFingerprint(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextSimilarity(a, b []oaiMsg) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	lastA := a[len(a)-1]
	lastB := b[len(b)-1]
	if lastA.Role != lastB.Role {
		return 0
	}
	textA := contentToString(lastA.Content)
	textB := contentToString(lastB.Content)
	if textA == textB {
		return 1.0
	}
	similarity := jaccardSimilarity(textA, textB)
	return similarity
}

func jaccardSimilarity(a, b string) float64 {
	setA := tokenize(a)
	setB := tokenize(b)
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	intersection := 0
	for k := range setA {
		if setB[k] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func tokenize(s string) map[string]bool {
	tokens := map[string]bool{}
	words := strings.Fields(strings.ToLower(s))
	for _, w := range words {
		tokens[w] = true
	}
	return tokens
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	explicitID := r.Header.Get("X-M365-Session-Id")
	ipFinger := clientIPFingerprint(r)
	ctxFinger := contextFingerprint(body.Messages)

	if explicitID != "" {
		if sessID, ok := sr.byExplicit[explicitID]; ok {
			if sess, ok := sr.sessions[sessID]; ok {
				sess.LastUsedAt = time.Now().UTC()
				sr.sessions[sessID] = sess
				sr.saveLocked()
				return ResolveResult{
					SessionID:      sess.SessionID,
					ConversationID: sess.ConversationID,
					AccountID:      sess.AccountID,
					MatchedBy:      "explicit",
					IsNew:          false,
				}
			}
		}
		if sess, ok := sr.sessions[explicitID]; ok {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[explicitID] = sess
			sr.saveLocked()
			return ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "explicit",
				IsNew:          false,
			}
		}
	}

	if body.User != "" {
		if sessID, ok := sr.byUserField[body.User]; ok {
			if sess, ok := sr.sessions[sessID]; ok {
				sess.LastUsedAt = time.Now().UTC()
				if explicitID != "" {
					sess.SessionID = explicitID
					sr.byExplicit[explicitID] = sessID
				}
				sr.sessions[sessID] = sess
				sr.saveLocked()
				return ResolveResult{
					SessionID:      sess.SessionID,
					ConversationID: sess.ConversationID,
					AccountID:      sess.AccountID,
					MatchedBy:      "user_field",
					IsNew:          false,
				}
			}
		}
	}

	if sessID, ok := sr.byIPFinger[ipFinger]; ok {
		if sess, ok := sr.sessions[sessID]; ok {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[sessID] = sess
			sr.saveLocked()
			return ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "ip_fingerprint",
				IsNew:          false,
			}
		}
	}

	if ctxFinger != "" {
		if sessID, ok := sr.byContext[ctxFinger]; ok {
			if sess, ok := sr.sessions[sessID]; ok {
				sess.LastUsedAt = time.Now().UTC()
				sr.sessions[sessID] = sess
				sr.saveLocked()
				return ResolveResult{
					SessionID:      sess.SessionID,
					ConversationID: sess.ConversationID,
					AccountID:      sess.AccountID,
					MatchedBy:      "context_exact",
					IsNew:          false,
				}
			}
		}
	}

	threshold := 0.6
	if v := os.Getenv("M365_CONTEXT_SIMILARITY"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			threshold = f
		}
	}
	bestMatchID := ""
	bestSimilarity := 0.0
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		sim := contextSimilarity(sess.contextHistory, body.Messages)
		if sim > bestSimilarity {
			bestSimilarity = sim
			bestMatchID = id
		}
	}
	if bestMatchID != "" && bestSimilarity >= threshold {
		sess := sr.sessions[bestMatchID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestMatchID] = sess
		sr.saveLocked()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_similar_%.2f", bestSimilarity),
			IsNew:          false,
		}
	}

	return ResolveResult{IsNew: true}
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, r *http.Request) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	now := time.Now().UTC()
	explicitID := r.Header.Get("X-M365-Session-Id")
	if explicitID != "" && sessionID == "" {
		sessionID = explicitID
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		CreatedAt:      now,
		LastUsedAt:     now,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextFinger:  contextFingerprint(body.Messages),
		contextHistory: cloneMessages(body.Messages),
	}

	sr.reindexLocked(sess)
	sr.saveLocked()
}

func (sr *sessionResolver) GetSession(sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	return s, ok
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	if !ok {
		return false
	}
	delete(sr.sessions, sessionID)
	delete(sr.byExplicit, sessionID)
	if s.UserField != "" {
		delete(sr.byUserField, s.UserField)
	}
	if s.IPFingerprint != "" {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if s.ContextFinger != "" {
		delete(sr.byContext, s.ContextFinger)
	}
	sr.saveLocked()
	return true
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)
	return out
}
