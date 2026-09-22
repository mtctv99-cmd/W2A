package browser

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"geminigo/internal/client"
)

type BrowserSession struct {
	SessionID   string    `json:"session_id"`
	AccountID   string    `json:"account_id"`
	ProfileDir  string    `json:"profile_dir"`
	ChatURL     string    `json:"chat_url"`
	LastActive  time.Time `json:"last_active"`
	mu          sync.Mutex
	playwrightMu sync.Mutex // serializes Playwright launches on this session's Chrome profile
}

type SessionPool struct {
	mu       sync.RWMutex
	sessions map[string]*BrowserSession
}

var GlobalSessionPool = &SessionPool{
	sessions: make(map[string]*BrowserSession),
}

func init() {
	go startSessionSweeper()
}

func startSessionSweeper() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		GlobalSessionPool.SweepIdleSessions(3 * time.Minute)
	}
}

func (p *SessionPool) SweepIdleSessions(maxIdle time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for id, sess := range p.sessions {
		if now.Sub(sess.LastActive) > maxIdle {
			log.Printf("[SessionPool] Session %s idle for >%v, auto closing...\n", id, maxIdle)
			delete(p.sessions, id)
		}
	}
}

func (p *SessionPool) GetOrCreateSession(sessionID string) (*BrowserSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sessionID == "" {
		sessionID = fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}

	if sess, ok := p.sessions[sessionID]; ok {
		sess.LastActive = time.Now()
		return sess, nil
	}

	// Pick next healthy account
	_, _, accountID, profileDir := client.GetNextHealthyCookie()
	if accountID == "" {
		accountID = "Account-1"
	}
	if profileDir == "" {
		profileDir, _ = filepath.Abs(fmt.Sprintf("data/profiles/%s", accountID))
	} else {
		profileDir, _ = filepath.Abs(profileDir)
	}

	sess := &BrowserSession{
		SessionID:  sessionID,
		AccountID:  accountID,
		ProfileDir: profileDir,
		LastActive: time.Now(),
	}
	p.sessions[sessionID] = sess
	log.Printf("[SessionPool] Created new session %s bound to %s\n", sessionID, accountID)
	return sess, nil
}

func (p *SessionPool) GetActiveSessionCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.sessions)
}

func (p *SessionPool) GetSessionsInfo() []map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var list []map[string]interface{}
	for id, sess := range p.sessions {
		list = append(list, map[string]interface{}{
			"session_id":  id,
			"account_id":  sess.AccountID,
			"chat_url":    sess.ChatURL,
			"last_active": sess.LastActive.Format(time.RFC3339),
		})
	}
	return list
}

func RunPlaywrightScript(prompt, accountID, mediaType, profileDir, chatURL string) (string, error) {
	if profileDir == "" {
		profileDir, _ = filepath.Abs(fmt.Sprintf("data/profiles/%s", accountID))
	} else {
		profileDir, _ = filepath.Abs(profileDir)
	}
	_ = os.MkdirAll(profileDir, 0755)

	timeoutDuration := 450 * time.Second // 7.5 minutes max timeout for videos
	if mediaType == "image" {
		timeoutDuration = 90 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", "playwright_media.js", prompt, accountID, mediaType, profileDir, chatURL)
	SetProcessGroup(cmd)

	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			KillProcessGroup(cmd.Process.Pid)
		}
		return "", fmt.Errorf("%s generation timed out after %v", mediaType, timeoutDuration)
	}

	outputStr := string(out)
	if err != nil {
		if cmd.Process != nil {
			KillProcessGroup(cmd.Process.Pid)
		}
		return "", fmt.Errorf("playwright failed: %v, output: %s", err, outputStr)
	}

	return outputStr, nil
}

func StartCDPMediaGenerationWithSession(prompt, sessionID, mediaType string) (mediaURL string, chatURL string, err error) {
	sess, err := GlobalSessionPool.GetOrCreateSession(sessionID)
	if err != nil {
		return "", "", err
	}

	// Copy session fields under lock, release before long-running Playwright call
	// then acquire per-session Playwright lock to serialize Chrome on same profile
	sess.mu.Lock()
	accountID := sess.AccountID
	profileDir := sess.ProfileDir
	chatURLIn := sess.ChatURL
	if mediaType == "video" {
		// For video generation, always force clean new chat thread on same account profile to prevent DOM video clutter & duplicate matching
		chatURLIn = ""
	}
	sess.LastActive = time.Now()
	sess.mu.Unlock()

	sess.playwrightMu.Lock()
	outputStr, err := RunPlaywrightScript(prompt, accountID, mediaType, profileDir, chatURLIn)
	sess.playwrightMu.Unlock()
	if err != nil {
		// Re-lock only to reset broken chat URL
		sess.mu.Lock()
		sess.ChatURL = ""
		sess.mu.Unlock()
		return "", "", err
	}

	extractedMedia := ""
	extractedChat := ""
	extractedCookies := ""

	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "__MEDIA_URL__=") {
			extractedMedia = strings.TrimPrefix(line, "__MEDIA_URL__=")
		}
		if strings.HasPrefix(line, "__CHAT_URL__=") {
			extractedChat = strings.TrimPrefix(line, "__CHAT_URL__=")
		}
		if strings.HasPrefix(line, "__COOKIES__=") {
			extractedCookies = strings.TrimPrefix(line, "__COOKIES__=")
		}
	}

	if extractedCookies != "" {
		sess.mu.Lock()
		if err := client.AddAccountCookieWithID(sess.AccountID, extractedCookies, sess.ProfileDir); err == nil {
			log.Printf("[SessionPool] Auto-synced fresh cookies from Playwright profile to %s\n", sess.AccountID)
		}
		sess.mu.Unlock()
	}

	if extractedChat != "" && strings.HasPrefix(extractedChat, "https://gemini.google.com/app") && mediaType != "video" {
		sess.mu.Lock()
		sess.ChatURL = extractedChat
		sess.mu.Unlock()
		log.Printf("[SessionPool] Saved active chat URL for session %s: %s\n", sess.SessionID, sess.ChatURL)
	}

	if extractedMedia == "" {
		sess.mu.Lock()
		chatURL = sess.ChatURL
		sess.mu.Unlock()
		return "", chatURL, fmt.Errorf("%s URL not found in output: %s", mediaType, outputStr)
	}

	sess.mu.Lock()
	chatURL = sess.ChatURL
	sess.mu.Unlock()
	return extractedMedia, chatURL, nil
}
