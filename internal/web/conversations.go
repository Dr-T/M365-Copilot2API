package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, map[string]any{"conversations": s.sessions.list()})
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s.conversationManager.Delete(body.ID)
	if !s.sessions.delete(body.ID) {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) conversationCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		KeepN int    `json:"keep_n"`
	}
	if json.NewDecoder(r.Body).Decode(&body) == nil {
		if body.Mode != "" {
			s.conversationManager = openConversationManager()
		}
	}
	cleaned := s.conversationManager.Cleanup()
	jsonOut(w, map[string]any{
		"status":    "cleaned",
		"mode":      string(s.conversationManager.Mode()),
		"deleted":   cleaned,
		"remaining": len(s.conversationManager.List()),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions := s.sessionResolver.ListSessions()
		jsonOut(w, map[string]any{
			"object": "list",
			"data":   sessions,
		})
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sess, ok := s.sessionResolver.GetSession(body.SessionID)
		if !ok {
			jsonOut(w, map[string]any{
				"object":     "session",
				"id":         body.SessionID,
				"created":    time.Now().Unix(),
				"expires_in": 1800,
				"status":     "created",
			})
			return
		}
		jsonOut(w, map[string]any{
			"object":          "session",
			"id":              sess.SessionID,
			"conversation_id": sess.ConversationID,
			"created":         sess.CreatedAt.Unix(),
			"status":          "active",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats := cacheStats.GetStats()
	jsonOut(w, map[string]any{
		"object": "cache_stats",
		"stats":  stats,
	})
}

func (s *Server) handleCacheStatsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cacheStats.Reset()
	jsonOut(w, map[string]any{"status": "reset"})
}

func (s *Server) handleM365Conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	chats, err := m365CloudClient.ListConversations()
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"object": "list", "data": chats, "count": len(chats)})
}

func (s *Server) handleM365Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ConversationID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := m365CloudClient.DeleteConversation(body.ConversationID); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "deleted", "conversation_id": body.ConversationID})
}

func (s *Server) handleM365Cleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		MaxAgeHours int `json:"max_age_hours"`
		KeepN       int `json:"keep_n"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	maxAge := time.Duration(body.MaxAgeHours) * time.Hour
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	keepN := body.KeepN
	if keepN <= 0 {
		keepN = 5
	}

	deleted, err := m365CloudClient.CleanupOldConversations(maxAge, keepN)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "cleaned", "deleted": deleted})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if s.sessionResolver.DeleteSession(sessionID) {
		jsonOut(w, map[string]any{"status": "deleted", "session_id": sessionID})
	} else {
		http.Error(w, "session not found", http.StatusNotFound)
	}
}

type conversationWhitelistRequest struct {
	ConversationID string `json:"conversation_id"`
	Add            bool   `json:"add"`
}

func (s *Server) conversationWhitelist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body conversationWhitelistRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ConversationID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Add {
		s.conversationManager.Whitelist(body.ConversationID)
	} else {
		s.conversationManager.Unwhitelist(body.ConversationID)
	}
	jsonOut(w, map[string]any{"status": "updated", "conversation_id": body.ConversationID, "whitelisted": body.Add})
}
