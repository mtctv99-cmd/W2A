package copilot

import (
	"log"
	"sync"
	"time"
)

// CopilotSession represents a persistent conversation thread on Microsoft Sydney with sticky account affinity.
type CopilotSession struct {
	SessionID      string    `json:"session_id"`
	AccountID      string    `json:"account_id"`
	ConversationID string    `json:"conversation_id"`
	ChatSessionID  string    `json:"chat_session_id"`
	InvocationID   int       `json:"invocation_id"`
	TurnCount      int       `json:"turn_count"`
	CreatedAt      time.Time `json:"created_at"`
	LastActive     time.Time `json:"last_active"`
}

const (
	MaxSessionTurns   = 25
	DefaultSessionTTL = 30 * time.Minute
)

type CopilotSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*CopilotSession
}

var GlobalCopilotSessions = NewCopilotSessionStore()

func NewCopilotSessionStore() *CopilotSessionStore {
	store := &CopilotSessionStore{
		sessions: make(map[string]*CopilotSession),
	}
	go store.startSweeper()
	return store
}

func (s *CopilotSessionStore) startSweeper() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.SweepIdleSessions(DefaultSessionTTL)
	}
}

// GetOrCreateSession retrieves an existing session or initializes a new one.
func (s *CopilotSessionStore) GetOrCreateSession(sessionID string) (*CopilotSession, bool) {
	if sessionID == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, exists := s.sessions[sessionID]
	if exists {
		// Check turn limit
		if sess.TurnCount >= MaxSessionTurns {
			log.Printf("[CopilotSession] Session %s reached max turns (%d/%d), auto-resetting conversation thread.\n",
				sessionID, sess.TurnCount, MaxSessionTurns)
			sess.ConversationID = ""
			sess.ChatSessionID = ""
			sess.InvocationID = 0
			sess.TurnCount = 0
		}
		sess.LastActive = time.Now()
		return sess, true
	}

	newSess := &CopilotSession{
		SessionID:    sessionID,
		InvocationID: 0,
		CreatedAt:    time.Now(),
		LastActive:   time.Now(),
		TurnCount:    0,
	}
	s.sessions[sessionID] = newSess
	log.Printf("[CopilotSession] Created new persistent session: %s\n", sessionID)
	return newSess, false
}

// BindAccount binds an account to the session for sticky affinity.
func (s *CopilotSessionStore) BindAccount(sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.AccountID = accountID
		sess.LastActive = time.Now()
	}
}

// UpdateSessionMetadata saves Sydney conversationId, chatSessionId, and updates invocationId and turnCount.
func (s *CopilotSessionStore) UpdateSessionMetadata(sessionID, convID, chatSessID string, nextInvocID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		if convID != "" {
			sess.ConversationID = convID
		}
		if chatSessID != "" {
			sess.ChatSessionID = chatSessID
		}
		sess.InvocationID = nextInvocID
		sess.TurnCount++
		sess.LastActive = time.Now()
		log.Printf("[CopilotSession] Updated session %s (turn #%d, invocID: %d, conv: %s)\n",
			sessionID, sess.TurnCount, sess.InvocationID, sess.ConversationID)
	}
}

// ResetSession resets conversation metadata for a fresh thread without losing sticky account binding.
func (s *CopilotSessionStore) ResetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.ConversationID = ""
		sess.ChatSessionID = ""
		sess.InvocationID = 0
		sess.TurnCount = 0
		sess.LastActive = time.Now()
		log.Printf("[CopilotSession] Manually reset session thread: %s\n", sessionID)
	}
}

// DeleteSession removes a session completely.
func (s *CopilotSessionStore) DeleteSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	log.Printf("[CopilotSession] Removed session: %s\n", sessionID)
}

// SweepIdleSessions purges sessions that have been idle longer than ttl.
func (s *CopilotSessionStore) SweepIdleSessions(ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	count := 0
	for id, sess := range s.sessions {
		if now.Sub(sess.LastActive) > ttl {
			delete(s.sessions, id)
			count++
		}
	}
	if count > 0 {
		log.Printf("[CopilotSession] Sweeper purged %d idle sessions (TTL %v)\n", count, ttl)
	}
	return count
}

// GetActiveSessions returns current active sessions snapshot.
func (s *CopilotSessionStore) GetActiveSessions() []*CopilotSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*CopilotSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		copySess := *sess
		list = append(list, &copySess)
	}
	return list
}
