package client

import (
	"crypto/rand"
	"fmt"
	"log"
	"sync"
	"time"
)

// GeminiSession represents a persistent conversation thread with sticky account affinity.
type GeminiSession struct {
	SessionID      string    `json:"session_id"`
	AccountID      string    `json:"account_id"`
	ConvUUID       string    `json:"conv_uuid"`
	ConversationID string    `json:"conversation_id"`
	ResponseID     string    `json:"response_id"`
	ChoiceID       string    `json:"choice_id"`
	TurnCount      int       `json:"turn_count"`
	CreatedAt      time.Time `json:"created_at"`
	LastActive     time.Time `json:"last_active"`
}

const (
	MaxSessionTurns  = 25
	DefaultSessionTTL = 30 * time.Minute
)

type GeminiSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*GeminiSession
}

var GlobalGeminiSessions = NewGeminiSessionStore()

func NewGeminiSessionStore() *GeminiSessionStore {
	store := &GeminiSessionStore{
		sessions: make(map[string]*GeminiSession),
	}
	// Background idle sweeper
	go store.startSweeper()
	return store
}

func (s *GeminiSessionStore) startSweeper() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.SweepIdleSessions(DefaultSessionTTL)
	}
}

// GetOrCreateSession retrieves an existing session or initializes a new one.
func (s *GeminiSessionStore) GetOrCreateSession(sessionID string) (*GeminiSession, bool) {
	if sessionID == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, exists := s.sessions[sessionID]
	if exists {
		// Check turn limit
		if sess.TurnCount >= MaxSessionTurns {
			log.Printf("[GeminiSession] Session %s reached max turns (%d/%d), auto-resetting conversation thread.\n",
				sessionID, sess.TurnCount, MaxSessionTurns)
			sess.ConversationID = ""
			sess.ResponseID = ""
			sess.ChoiceID = ""
			sess.TurnCount = 0
			sess.ConvUUID = generateRandomUUID()
		}
		sess.LastActive = time.Now()
		// Return copy of pointer
		return sess, true
	}

	newSess := &GeminiSession{
		SessionID:  sessionID,
		ConvUUID:   generateRandomUUID(),
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		TurnCount:  0,
	}
	s.sessions[sessionID] = newSess
	log.Printf("[GeminiSession] Created new persistent session: %s\n", sessionID)
	return newSess, false
}

// BindAccount binds an account to the session for sticky affinity.
func (s *GeminiSessionStore) BindAccount(sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.AccountID = accountID
		sess.LastActive = time.Now()
	}
}

// UpdateSessionMetadata saves the extracted conversation_id, response_id, choice_id and increments turn count.
func (s *GeminiSessionStore) UpdateSessionMetadata(sessionID, convID, respID, choiceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		if convID != "" {
			sess.ConversationID = convID
		}
		if respID != "" {
			sess.ResponseID = respID
		}
		if choiceID != "" {
			sess.ChoiceID = choiceID
		}
		sess.TurnCount++
		sess.LastActive = time.Now()
		log.Printf("[GeminiSession] Updated session %s (turn #%d, conv: %s, resp: %s)\n",
			sessionID, sess.TurnCount, sess.ConversationID, sess.ResponseID)
	}
}

// ResetSession resets conversation metadata for a fresh thread without losing sticky account binding.
func (s *GeminiSessionStore) ResetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.ConversationID = ""
		sess.ResponseID = ""
		sess.ChoiceID = ""
		sess.TurnCount = 0
		sess.ConvUUID = generateRandomUUID()
		sess.LastActive = time.Now()
		log.Printf("[GeminiSession] Manually reset session thread: %s\n", sessionID)
	}
}

// DeleteSession removes a session completely.
func (s *GeminiSessionStore) DeleteSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	log.Printf("[GeminiSession] Removed session: %s\n", sessionID)
}

// SweepIdleSessions purges sessions that have been idle longer than ttl.
func (s *GeminiSessionStore) SweepIdleSessions(ttl time.Duration) int {
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
		log.Printf("[GeminiSession] Sweeper purged %d idle sessions (TTL %v)\n", count, ttl)
	}
	return count
}

// GetActiveSessions returns current active sessions snapshot.
func (s *GeminiSessionStore) GetActiveSessions() []*GeminiSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*GeminiSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		copySess := *sess
		list = append(list, &copySess)
	}
	return list
}

func generateRandomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
