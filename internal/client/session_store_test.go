package client

import (
	"testing"
	"time"
)

func TestGeminiSessionStore_Lifecycle(t *testing.T) {
	store := NewGeminiSessionStore()

	// 1. Create session
	sess, existing := store.GetOrCreateSession("test-sess-1")
	if existing {
		t.Fatalf("Expected new session, got existing")
	}
	if sess.SessionID != "test-sess-1" || sess.TurnCount != 0 {
		t.Fatalf("Invalid initial session: %+v", sess)
	}

	// 2. Bind account
	store.BindAccount("test-sess-1", "acc-01")
	sess2, existing2 := store.GetOrCreateSession("test-sess-1")
	if !existing2 || sess2.AccountID != "acc-01" {
		t.Fatalf("Expected existing session bound to acc-01, got: %+v", sess2)
	}

	// 3. Update metadata
	store.UpdateSessionMetadata("test-sess-1", "c_123", "r_456", "rc_789")
	sess3, _ := store.GetOrCreateSession("test-sess-1")
	if sess3.TurnCount != 1 || sess3.ConversationID != "c_123" || sess3.ResponseID != "r_456" || sess3.ChoiceID != "rc_789" {
		t.Fatalf("Metadata update failed: %+v", sess3)
	}

	// 4. Test 25-turn auto-reset
	for i := 1; i < MaxSessionTurns; i++ {
		store.UpdateSessionMetadata("test-sess-1", "c_123", "r_456", "rc_789")
	}
	// Calling GetOrCreateSession after reaching MaxSessionTurns will trigger auto-reset of thread metadata
	sessAfter, _ := store.GetOrCreateSession("test-sess-1")
	if sessAfter.TurnCount != 0 || sessAfter.ConversationID != "" {
		t.Fatalf("Auto-reset failed: turnCount=%d, convID=%s", sessAfter.TurnCount, sessAfter.ConversationID)
	}
	if sessAfter.AccountID != "acc-01" {
		t.Fatalf("Auto-reset lost account binding: %s", sessAfter.AccountID)
	}

	// 5. Test sweeper idle purge
	sessAfter.LastActive = time.Now().Add(-1 * time.Hour)
	purged := store.SweepIdleSessions(30 * time.Minute)
	if purged != 1 {
		t.Fatalf("Expected 1 purged session, got %d", purged)
	}
}
