package copilot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	SignalRSeparator = "\x1e"
	DefaultChatURL   = "https://m365.cloud.microsoft/chat?fromcode=cmmyr718qsb&es=Click&redirfrom=cosmicRingCookie"
)

var (
	copilotMu         sync.Mutex
	cachedWsURL       string
	templatePayload   map[string]interface{}
	autoLoginRunning  atomic.Bool
	autoRefreshActive atomic.Bool
)

var browserPaths = []string{
	"/usr/bin/google-chrome",
	"/usr/bin/google-chrome-stable",
	"/usr/bin/chromium-browser",
	"/usr/bin/chromium",
	"/opt/google/chrome/chrome",
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

func findBrowser() string {
	for _, p := range browserPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "chrome", "chromium", "msedge"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "google-chrome"
}

func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func getActiveDisplay() string {
	d := os.Getenv("DISPLAY")
	if d != "" {
		return d
	}
	// Fallback to :1 (standard active GNOME display on server 1.117)
	return ":1"
}

func IsCopilotModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(lower, "copilot") ||
		strings.HasPrefix(lower, "gpt-5") ||
		strings.HasPrefix(lower, "dall-e") ||
		lower == "dalle" ||
		lower == "gpt-4o"
}

func MapModelToTone(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "gpt-5.6-think") || strings.Contains(lower, "gpt-5-think"):
		return "Gpt_5_6_Reasoning"
	case strings.Contains(lower, "gpt-5.6") || strings.Contains(lower, "gpt-5") || strings.Contains(lower, "gpt-4o"):
		return "Gpt_5_6_Chat"
	case strings.Contains(lower, "think") || strings.Contains(lower, "o1") || strings.Contains(lower, "o3"):
		return "Reasoning"
	case strings.Contains(lower, "quick"):
		return "Chat"
	default:
		return "Magic"
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func CheckTokenValid(wsURL string, minSec int64) bool {
	if wsURL == "" {
		return false
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return false
	}
	token := u.Query().Get("access_token")
	if token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return false
	}
	return claims.Exp-time.Now().Unix() > minSec
}

func loadTemplate() error {
	if templatePayload != nil {
		return nil
	}
	tplPath := filepath.Join(DefaultDataDir, "ws_template.json")
	data, err := os.ReadFile(tplPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", tplPath, err)
	}
	return json.Unmarshal(data, &templatePayload)
}

func BuildInvocationURL(baseWsURL string) (string, string, string, error) {
	return BuildInvocationURLWithSession(baseWsURL, nil)
}

func BuildInvocationURLWithSession(baseWsURL string, sess *CopilotSession) (string, string, string, error) {
	u, err := url.Parse(baseWsURL)
	if err != nil {
		return "", "", "", err
	}
	var convID, sessID string
	if sess != nil && sess.ConversationID != "" {
		convID = sess.ConversationID
		sessID = sess.ChatSessionID
	}
	if convID == "" {
		convID = randomUUID()
	}
	if sessID == "" {
		sessID = randomHex(16)
	}
	xSess := fmt.Sprintf("%s-%s-%s-%s-%s", sessID[:8], sessID[8:12], sessID[12:16], sessID[16:20], sessID[20:32])

	q := u.Query()
	q.Set("chatsessionid", sessID)
	q.Set("XRoutingParameterSessionKey", sessID)
	q.Set("clientrequestid", sessID)
	q.Set("X-SessionId", xSess)
	q.Set("ConversationId", convID)
	u.RawQuery = q.Encode()

	return u.String(), convID, sessID, nil
}

// GenerateChat invokes Microsoft Copilot Substrate Sydney Chathub SignalR endpoint using the next healthy account.
func GenerateChat(prompt string, model string, stream bool, imageDataURI string, onChunk func(string)) (string, error) {
	acc, err := GetNextHealthyAccount()
	if err != nil {
		return "", err
	}
	return GenerateChatWithAccount(acc, prompt, model, stream, imageDataURI, onChunk)
}

// GenerateChatWithAccount invokes Microsoft Copilot using a specific account.
func GenerateChatWithAccount(acc *CopilotAccount, prompt string, model string, stream bool, imageDataURI string, onChunk func(string)) (string, error) {
	return GenerateChatWithSession(acc, nil, prompt, model, stream, imageDataURI, onChunk)
}

// GenerateChatWithSession invokes Microsoft Copilot using a specific account and optional session continuity.
func GenerateChatWithSession(acc *CopilotAccount, sess *CopilotSession, prompt string, model string, stream bool, imageDataURI string, onChunk func(string)) (string, error) {
	if acc == nil || acc.TokenURL == "" {
		return "", fmt.Errorf("invalid copilot account or empty token URL")
	}
	if err := loadTemplate(); err != nil {
		return "", err
	}

	baseWsURL := acc.TokenURL
	invocURL, convID, chatSessID, err := BuildInvocationURLWithSession(baseWsURL, sess)
	if err != nil {
		return "", err
	}
	if sess != nil && sess.ConversationID == "" {
		sess.ConversationID = convID
		sess.ChatSessionID = chatSessID
	}

	reqID := randomUUID()
	traceID := randomHex(16)
	tone := MapModelToTone(model)

	// Clone template payload
	rawJSON, _ := json.Marshal(templatePayload)
	var invocObj map[string]interface{}
	_ = json.Unmarshal(rawJSON, &invocObj)

	invocIDStr := "0"
	isStart := true
	if sess != nil && sess.TurnCount > 0 {
		invocIDStr = fmt.Sprintf("%d", sess.InvocationID)
		isStart = false
	}

	invocObj["invocationId"] = invocIDStr
	args, _ := invocObj["arguments"].([]interface{})
	if len(args) == 0 {
		return "", fmt.Errorf("invalid template arguments")
	}
	arg0, _ := args[0].(map[string]interface{})
	arg0["clientCorrelationId"] = reqID
	arg0["requestId"] = reqID
	arg0["traceId"] = traceID
	arg0["tone"] = tone
	arg0["isStartOfSession"] = isStart

	arg0["conversationId"] = convID
	arg0["sessionId"] = chatSessID
	if clientInfo, ok := arg0["clientInfo"].(map[string]interface{}); ok {
		clientInfo["clientSessionId"] = chatSessID
	}

	msgMap, _ := arg0["message"].(map[string]interface{})
	msgMap["text"] = prompt
	msgMap["requestId"] = reqID
	if imageDataURI != "" {
		msgMap["imageUrl"] = imageDataURI
		msgMap["originalImageUrl"] = imageDataURI
	}

	header := make(http.Header)
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")
	header.Set("Origin", "https://m365.cloud.microsoft")

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}

	ws, _, err := dialer.Dial(invocURL, header)
	if err != nil {
		return "", fmt.Errorf("websocket dial failed: %w", err)
	}
	defer ws.Close()

	// 1. Send SignalR JSON handshake
	handshake := "{\"protocol\":\"json\",\"version\":1}" + SignalRSeparator
	if err := ws.WriteMessage(websocket.TextMessage, []byte(handshake)); err != nil {
		return "", fmt.Errorf("handshake write failed: %w", err)
	}

	_, _, err = ws.ReadMessage()
	if err != nil {
		return "", fmt.Errorf("handshake read failed: %w", err)
	}

	// 2. Send Chat Invocation
	invocBody, _ := json.Marshal(invocObj)
	invocMsg := string(invocBody) + SignalRSeparator
	if err := ws.WriteMessage(websocket.TextMessage, []byte(invocMsg)); err != nil {
		return "", fmt.Errorf("invocation write failed: %w", err)
	}

	var collected string
	var lastEmittedLen int
	timeout := time.After(45 * time.Second)

	for {
		select {
		case <-timeout:
			if collected != "" {
				return collected, nil
			}
			return "", fmt.Errorf("copilot request timeout")
		default:
			_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				if collected != "" {
					return collected, nil
				}
				return "", fmt.Errorf("read error: %w", err)
			}
			if msgType != websocket.TextMessage {
				continue
			}

			chunks := strings.Split(string(data), SignalRSeparator)
			for _, chunk := range chunks {
				chunk = strings.TrimSpace(chunk)
				if chunk == "" {
					continue
				}

				var frame struct {
					Type      int                    `json:"type"`
					Target    string                 `json:"target,omitempty"`
					Arguments []interface{}          `json:"arguments,omitempty"`
					Item      map[string]interface{} `json:"item,omitempty"`
				}

				if err := json.Unmarshal([]byte(chunk), &frame); err != nil {
					continue
				}

				if frame.Type == 6 {
					_ = ws.WriteMessage(websocket.TextMessage, []byte("{\"type\":6}"+SignalRSeparator))
					continue
				}

				if frame.Type == 1 && len(frame.Arguments) > 0 {
					argMap, _ := frame.Arguments[0].(map[string]interface{})
					if msgs, ok := argMap["messages"].([]interface{}); ok && len(msgs) > 0 {
						for _, m := range msgs {
							mObj, _ := m.(map[string]interface{})
							if mObj["author"] == "bot" {
								if isLoaderMessage(mObj) {
									continue
								}
								txt, _ := mObj["text"].(string)
								txt = cleanLoaderPrefix(txt)
								if len(txt) > len(collected) {
									delta := txt[len(collected):]
									collected = txt
									if stream && onChunk != nil && len(collected) > lastEmittedLen {
										onChunk(delta)
										lastEmittedLen = len(collected)
									}
								}
							}
						}
					}
				}

				if frame.Type == 2 {
					if item := frame.Item; item != nil {
						if sess != nil {
							convID, _ := item["conversationId"].(string)
							chatSessID, _ := item["chatSessionId"].(string)
							nextInvocID := sess.InvocationID + 1
							GlobalCopilotSessions.UpdateSessionMetadata(sess.SessionID, convID, chatSessID, nextInvocID)
						}
						if msgs, ok := item["messages"].([]interface{}); ok {
							var botTexts []string
							var allCardImgs []string
							for _, m := range msgs {
								mObj, _ := m.(map[string]interface{})
								if mObj["author"] != "user" {
									if isLoaderMessage(mObj) {
										continue
									}
									if txt, ok := mObj["text"].(string); ok && txt != "" {
										cleaned := cleanLoaderPrefix(txt)
										if cleaned != "" {
											botTexts = append(botTexts, cleaned)
										}
									}
									cardImgs := extractAdaptiveCardImages(mObj)
									allCardImgs = append(allCardImgs, cardImgs...)
								}
							}
								if len(botTexts) > 0 {
									collected = strings.Join(botTexts, "\n\n")
								}
								collected = cleanLoaderPrefix(collected)
							for _, imgURL := range allCardImgs {
								publicPath := saveCopilotBase64Image(imgURL)
								imgMD := fmt.Sprintf("\n\n![Generated Image](%s)\n", publicPath)
								if !strings.Contains(collected, publicPath) {
									collected += imgMD
									if stream && onChunk != nil {
										onChunk(imgMD)
									}
								}
							}
						}
					}
					return collected, nil
				}
			}
		}
	}
}

func isLoaderMessage(mObj map[string]interface{}) bool {
	if mType, ok := mObj["messageType"].(string); ok && mType != "" {
		return true
	}
	txt, _ := mObj["text"].(string)
	clean := strings.TrimSpace(txt)
	loaderPrefixes := []string{
		"Looking into it",
		"Working on it",
		"Just a sec",
		"Putting it together",
		"Thinking",
		"Searching the web",
		"Still thinking",
		"Almost there",
		"Finding info",
	}
	for _, p := range loaderPrefixes {
		if strings.HasPrefix(clean, p) && (len(clean) <= len(p)+4 || strings.HasSuffix(clean, "…") || strings.HasSuffix(clean, "...")) {
			return true
		}
	}
	return false
}

func cleanLoaderPrefix(text string) string {
	loaderPrefixes := []string{
		"Looking into it",
		"Working on it",
		"Just a sec",
		"Putting it together",
		"Thinking",
		"Searching the web",
		"Still thinking",
		"Almost there",
		"Finding info",
	}
	res := text
	for _, p := range loaderPrefixes {
		for _, ell := range []string{"…", "...", ":"} {
			target := p + ell
			if strings.HasPrefix(res, target) {
				res = strings.TrimPrefix(res, target)
				res = strings.TrimSpace(res)
			}
		}
	}
	return res
}

func extractAdaptiveCardImages(mObj map[string]interface{}) []string {
	var results []string
	cards, ok := mObj["adaptiveCards"].([]interface{})
	if !ok {
		return results
	}
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch node := v.(type) {
		case map[string]interface{}:
			for k, val := range node {
				if k == "url" || k == "image" {
					if s, ok := val.(string); ok && (strings.HasPrefix(s, "data:image/") || strings.HasPrefix(s, "http")) {
						results = append(results, s)
					}
				}
				walk(val)
			}
		case []interface{}:
			for _, item := range node {
				walk(item)
			}
		}
	}
	for _, c := range cards {
		walk(c)
	}
	return results
}

func saveCopilotBase64Image(dataURI string) string {
	if !strings.HasPrefix(dataURI, "data:image/") {
		return dataURI
	}
	_ = os.MkdirAll("data/files", 0755)
	rawB64 := dataURI
	if idx := strings.Index(dataURI, "base64,"); idx != -1 {
		rawB64 = dataURI[idx+7:]
	}
	imgData, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil || len(imgData) == 0 {
		return dataURI
	}
	filename := fmt.Sprintf("img_%s.png", randomHex(16))
	filePath := filepath.Join("data/files", filename)
	if err := os.WriteFile(filePath, imgData, 0644); err != nil {
		return dataURI
	}
	return fmt.Sprintf("/v1/files/%s", filename)
}

// GenerateImage invokes Copilot DALL-E 3 image generation using the next healthy account.
func GenerateImage(prompt string) ([]string, error) {
	acc, err := GetNextHealthyAccount()
	if err != nil {
		return nil, err
	}
	return GenerateImageWithAccount(acc, prompt)
}

// GenerateImageWithAccount invokes Copilot DALL-E 3 image generation using a specific account.
func GenerateImageWithAccount(acc *CopilotAccount, prompt string) ([]string, error) {
	if acc == nil || acc.TokenURL == "" {
		return nil, fmt.Errorf("invalid copilot account or empty token URL")
	}
	if err := loadTemplate(); err != nil {
		return nil, err
	}
	baseWsURL := acc.TokenURL
	invocURL, _, _, err := BuildInvocationURL(baseWsURL)
	if err != nil {
		return nil, err
	}

	reqID := randomUUID()
	traceID := randomHex(16)

	rawJSON, _ := json.Marshal(templatePayload)
	var invocObj map[string]interface{}
	_ = json.Unmarshal(rawJSON, &invocObj)

	invocObj["invocationId"] = "0"
	args, _ := invocObj["arguments"].([]interface{})
	if len(args) == 0 {
		return nil, fmt.Errorf("invalid template arguments")
	}
	arg0, _ := args[0].(map[string]interface{})
	arg0["clientCorrelationId"] = reqID
	arg0["requestId"] = reqID
	arg0["traceId"] = traceID
	arg0["tone"] = "Magic"
	arg0["isStartOfSession"] = true

	msgMap, _ := arg0["message"].(map[string]interface{})
	msgMap["text"] = prompt
	msgMap["requestId"] = reqID

	header := make(http.Header)
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")
	header.Set("Origin", "https://m365.cloud.microsoft")

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.Dial(invocURL, header)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}
	defer ws.Close()

	handshake := "{\"protocol\":\"json\",\"version\":1}" + SignalRSeparator
	if err := ws.WriteMessage(websocket.TextMessage, []byte(handshake)); err != nil {
		return nil, fmt.Errorf("handshake write failed: %w", err)
	}
	_, _, _ = ws.ReadMessage()

	invocBody, _ := json.Marshal(invocObj)
	if err := ws.WriteMessage(websocket.TextMessage, []byte(string(invocBody)+SignalRSeparator)); err != nil {
		return nil, fmt.Errorf("invocation write failed: %w", err)
	}

	var images []string
	timeout := time.After(60 * time.Second)

	for {
		select {
		case <-timeout:
			if len(images) > 0 {
				return images, nil
			}
			return nil, fmt.Errorf("image generation timeout")
		default:
			_ = ws.SetReadDeadline(time.Now().Add(20 * time.Second))
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				if len(images) > 0 {
					return images, nil
				}
				return nil, fmt.Errorf("read error: %w", err)
			}
			if msgType != websocket.TextMessage {
				continue
			}
			chunks := strings.Split(string(data), SignalRSeparator)
			for _, chunk := range chunks {
				chunk = strings.TrimSpace(chunk)
				if chunk == "" {
					continue
				}
				var frame struct {
					Type int                    `json:"type"`
					Item map[string]interface{} `json:"item,omitempty"`
				}
				if err := json.Unmarshal([]byte(chunk), &frame); err != nil {
					continue
				}
				if frame.Type == 6 {
					_ = ws.WriteMessage(websocket.TextMessage, []byte("{\"type\":6}"+SignalRSeparator))
					continue
				}
				if frame.Type == 2 {
					if item := frame.Item; item != nil {
						if msgs, ok := item["messages"].([]interface{}); ok {
							for _, m := range msgs {
								if mObj, ok := m.(map[string]interface{}); ok {
									cardImgs := extractAdaptiveCardImages(mObj)
									images = append(images, cardImgs...)
								}
							}
						}
					}
					return images, nil
				}
			}
		}
	}
}

func buildBrowserEnv() []string {
	env := os.Environ()
	disp := getActiveDisplay()
	hasDisp := false
	hasXauth := false
	for _, e := range env {
		if strings.HasPrefix(e, "DISPLAY=") {
			hasDisp = true
		}
		if strings.HasPrefix(e, "XAUTHORITY=") {
			hasXauth = true
		}
	}
	if !hasDisp {
		env = append(env, "DISPLAY="+disp)
	}
	if !hasXauth {
		for _, xPath := range []string{"/run/user/1000/gdm/Xauthority", os.ExpandEnv("$HOME/.Xauthority")} {
			if _, err := os.Stat(xPath); err == nil {
				env = append(env, "XAUTHORITY="+xPath)
				break
			}
		}
	}
	return env
}

// RaiseDisplayWindow sends _NET_ACTIVE_WINDOW to bring the Chrome window to the foreground on the active display.
func RaiseDisplayWindow(accountID string) {
	pythonCode := fmt.Sprintf(`
import subprocess
try:
    out = subprocess.check_output(["xwininfo", "-root", "-tree"], env={"DISPLAY": "%s", "XAUTHORITY": "/run/user/1000/gdm/Xauthority"}).decode()
    for line in out.splitlines():
        if "Google-chrome" in line and ("Sign in" in line or "Microsoft" in line or "Cloud" in line or "%s" in line or "chrome" in line.lower()):
            win_id = int(line.strip().split()[0], 16)
            from Xlib import X, display, protocol
            d = display.Display("%s")
            root = d.screen().root
            net_active = d.intern_atom("_NET_ACTIVE_WINDOW")
            event = protocol.event.ClientMessage(window=win_id, client_type=net_active, data=(32, [2, 0, 0, 0, 0]))
            root.send_event(event, event_mask=X.SubstructureRedirectMask | X.SubstructureNotifyMask)
            d.sync()
            break
except Exception:
    pass
`, getActiveDisplay(), accountID, getActiveDisplay())
	_ = exec.Command("python3", "-c", pythonCode).Run()
}

// StopCopilotAutoLogin terminates any active Chrome auto-login processes and frees locks.
func StopCopilotAutoLogin() {
	autoLoginRunning.Store(false)
	_ = exec.Command("pkill", "-f", "m365.cloud.microsoft").Run()
	_ = exec.Command("pkill", "-f", "data/copilot/profiles").Run()

	accs := GetAllAccounts()
	for _, a := range accs {
		pDir, _ := filepath.Abs(filepath.Join(DefaultDataDir, "profiles", a.ID))
		_ = os.Remove(filepath.Join(pDir, "SingletonLock"))
		_ = os.Remove(filepath.Join(pDir, "SingletonSocket"))
		_ = os.Remove(filepath.Join(pDir, "SingletonCookie"))
	}
	log.Println("[Copilot-AutoLogin] Đã dừng toàn bộ tiến trình đăng nhập Chrome.")
}

// StartCopilotLoginWithProfile launches Chrome on Linux display so the user can log in to M365 Copilot for a specific account.
func StartCopilotLoginWithProfile(accountID string, force bool) error {
	if autoLoginRunning.Load() {
		if force {
			StopCopilotAutoLogin()
			time.Sleep(600 * time.Millisecond)
		} else {
			RaiseDisplayWindow(accountID)
			return fmt.Errorf("tiến trình đăng nhập đang mở sẵn trên màn hình/AnyDesk. Hãy kiểm tra hoặc bấm 'Dừng' rồi mở lại")
		}
	}
	autoLoginRunning.Store(true)

	if accountID == "" {
		accs := GetAllAccounts()
		if len(accs) > 0 {
			accountID = accs[0].ID
		} else {
			accountID = "Copilot-1"
		}
	}

	go func() {
		defer autoLoginRunning.Store(false)

		browserPath := findBrowser()
		port, err := findFreePort()
		if err != nil {
			log.Printf("[Copilot-AutoLogin] Không tìm được port trống: %v\n", err)
			return
		}

		profileDir, _ := filepath.Abs(filepath.Join(DefaultDataDir, "profiles", accountID))
		_ = os.MkdirAll(profileDir, 0755)

		// Clean stale singleton locks
		_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))

		args := []string{
			fmt.Sprintf("--remote-debugging-port=%d", port),
			"--remote-allow-origins=*",
			"--user-data-dir=" + profileDir,
			"--no-first-run",
			"--no-default-browser-check",
			"--no-sandbox",
			"--disable-gpu",
			"--disable-dev-shm-usage",
			"--start-maximized",
			"--new-window",
			DefaultChatURL,
		}

		cmd := exec.Command(browserPath, args...)
		cmd.Env = buildBrowserEnv()

		log.Printf("[Copilot-AutoLogin] Đang khởi chạy Chrome đăng nhập cho %s trên DISPLAY %s (port %d)...\n", accountID, getActiveDisplay(), port)
		if err := cmd.Start(); err != nil {
			log.Printf("[Copilot-AutoLogin] Không khởi chạy được Chrome: %v\n", err)
			return
		}

		// Ensure window is raised to front after spawn
		go func() {
			time.Sleep(1500 * time.Millisecond)
			RaiseDisplayWindow(accountID)
			time.Sleep(2000 * time.Millisecond)
			RaiseDisplayWindow(accountID)
		}()

		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				time.Sleep(2 * time.Second)
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			_ = exec.Command("pkill", "-f", fmt.Sprintf("--remote-debugging-port=%d", port)).Run()
			_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
			_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
			_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))
			log.Printf("[Copilot-AutoLogin] Đã đóng Chrome sau khi hoàn tất đăng nhập cho %s.\n", accountID)
		}()

		// Poll for Copilot tab and continuously listen for WebSocket token
		clientHTTP := &http.Client{Timeout: 2 * time.Second}
		debugURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
		loginTimeout := time.After(600 * time.Second)

		for {
			select {
			case <-loginTimeout:
				log.Printf("[Copilot-AutoLogin] Timeout chờ đăng nhập %s (600s).\n", accountID)
				return
			case <-time.After(1500 * time.Millisecond):
				resp, err := clientHTTP.Get(debugURL)
				if err != nil {
					continue
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				var tabs []struct {
					URL                  string `json:"url"`
					WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
				}
				if err := json.Unmarshal(data, &tabs); err != nil {
					continue
				}

				for _, tab := range tabs {
					if strings.Contains(tab.URL, "m365.cloud.microsoft") && tab.WebSocketDebuggerURL != "" {
						log.Printf("[Copilot-AutoLogin] Đã phát hiện tab Copilot (%s), đang kết nối CDP giám sát...\n", tab.URL)

						tokenURL, err := captureWsFromCDP(tab.WebSocketDebuggerURL, 60*time.Second)
						if err == nil && tokenURL != "" {
							log.Printf("[Copilot-AutoLogin] 🎉 BẮT ĐƯỢC TOKEN COPILOT THÀNH CÔNG CHO %s!\n", accountID)
							_ = SaveTokenWithID(accountID, tokenURL)
							time.Sleep(5 * time.Second)
							return
						}
						log.Printf("[Copilot-AutoLogin] CDP session kết thúc hoặc trang chuyển hướng (%v), tiếp tục theo dõi...\n", err)
						time.Sleep(1 * time.Second)
						break
					}
				}
			}
		}
	}()

	return nil
}

// StartCopilotAutoLogin is a backwards-compatible wrapper for StartCopilotLoginWithProfile.
func StartCopilotAutoLogin() error {
	return StartCopilotLoginWithProfile("", false)
}

func captureWsFromCDP(tabWsURL string, maxWait time.Duration) (string, error) {
	ws, _, err := websocket.DefaultDialer.Dial(tabWsURL, nil)
	if err != nil {
		return "", err
	}
	defer ws.Close()

	// Enable Network, Page and Runtime
	_ = ws.WriteJSON(map[string]interface{}{
		"id":     1,
		"method": "Network.enable",
	})
	_ = ws.WriteJSON(map[string]interface{}{
		"id":     2,
		"method": "Page.enable",
	})
	_ = ws.WriteJSON(map[string]interface{}{
		"id":     3,
		"method": "Runtime.enable",
	})

	msgCh := make(chan []byte, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			msgCh <- msg
		}
	}()

	timer := time.NewTimer(maxWait)
	defer timer.Stop()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	ticksCount := 0

	for {
		select {
		case <-timer.C:
			return "", fmt.Errorf("timeout")
		case <-ticker.C:
			ticksCount++
			// Nếu sau 4 tick (12s) chưa bắt được token, reload trang để Sydney kích hoạt lại WebSocket
			if ticksCount == 4 {
				_ = ws.WriteJSON(map[string]interface{}{
					"id":     98,
					"method": "Page.reload",
				})
			}

			// Kích hoạt ô nhập Copilot gửi input event để kích hoạt Sydney Chathub WebSocket
			_ = ws.WriteJSON(map[string]interface{}{
				"id":     99,
				"method": "Runtime.evaluate",
				"params": map[string]interface{}{
					"expression": `(function(){
						let ed = document.querySelector('#m365-chat-editor-target-element') ||
						         document.querySelector('[contenteditable="true"]') ||
						         document.querySelector('textarea');
						if (ed) {
							ed.focus();
							document.execCommand('selectAll', false, null);
							document.execCommand('insertText', false, 'ping');
							ed.dispatchEvent(new Event('input', { bubbles: true }));
							setTimeout(function() {
								let btn = document.querySelector("button[aria-label*=\'Send\' i]") ||
								          document.querySelector("button[type=\'submit\']");
								if (btn && !btn.disabled) { btn.click(); }
							}, 500);
						}
					})()`,
				},
			})
		case err := <-errCh:
			return "", err
		case msg, ok := <-msgCh:
			if !ok {
				return "", fmt.Errorf("cdp connection closed")
			}
			var ev struct {
				Method string `json:"method"`
				Params struct {
					URL     string `json:"url"`
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
				} `json:"params"`
			}
			if err := json.Unmarshal(msg, &ev); err == nil {
				candidate := ev.Params.URL
				if candidate == "" {
					candidate = ev.Params.Request.URL
				}
				if strings.Contains(candidate, "Chathub") && strings.Contains(candidate, "access_token") {
					return candidate, nil
				}
			}
		}
	}
}

// StartAutoRefreshDaemon runs a background timer to refresh tokens when expiring across all accounts.
func StartAutoRefreshDaemon(ctx context.Context) {
	log.Println("[Copilot-Daemon] Auto-refresh background daemon started.")
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			accs := GetAllAccounts()
			for _, acc := range accs {
				// Nếu token còn dưới 30 phút (1800 giây) và hợp lệ
				if acc.TokenURL != "" && acc.RemainingSeconds > 0 && acc.RemainingSeconds < 1800 && !autoLoginRunning.Load() && !autoRefreshActive.Load() {
					log.Printf("[Copilot-Daemon] Tài khoản %s còn %d giây (< 30 phút). Đang tự động làm mới ngầm bằng headless Chrome...\n", acc.ID, acc.RemainingSeconds)
					go TriggerAutoRefreshHeadlessForAccount(acc.ID)
					time.Sleep(3 * time.Second)
				}
			}
		}
	}
}

// TriggerAutoRefreshHeadlessForAccount launches headless Chrome + CDP to refresh token for a specific account.
func TriggerAutoRefreshHeadlessForAccount(accountID string) bool {
	if !autoRefreshActive.CompareAndSwap(false, true) {
		return false
	}
	defer autoRefreshActive.Store(false)

	if accountID == "" {
		accs := GetAllAccounts()
		for _, acc := range accs {
			if acc.TokenURL != "" && acc.RemainingSeconds < 1800 {
				accountID = acc.ID
				break
			}
		}
		if accountID == "" && len(accs) > 0 {
			accountID = accs[0].ID
		}
		if accountID == "" {
			accountID = "Copilot-1"
		}
	}

	log.Printf("[Copilot-Refresh] Khởi chạy headless Chrome gia hạn token Copilot cho %s trên Linux...\n", accountID)

	profileDir, _ := filepath.Abs(filepath.Join(DefaultDataDir, "profiles", accountID))
	_ = os.MkdirAll(profileDir, 0755)

	// Dọn dẹp lock cũ nếu có
	_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
	_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
	_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))

	browserPath := findBrowser()
	port, err := findFreePort()
	if err != nil {
		log.Printf("[Copilot-Refresh] Không tìm được port trống: %v\n", err)
		return false
	}

		args := []string{
			fmt.Sprintf("--remote-debugging-port=%d", port),
			"--remote-allow-origins=*",
			"--user-data-dir=" + profileDir,
			"--no-first-run",
			"--no-default-browser-check",
			"--no-sandbox",
			"--disable-gpu",
			"--disable-dev-shm-usage",
			"--window-size=1280,800",
			DefaultChatURL,
		}

		xvfbPath, errXvfb := exec.LookPath("xvfb-run")
		var cmd *exec.Cmd
		if errXvfb == nil && xvfbPath != "" {
			xvfbArgs := []string{
				"-a",
				"-s", "-screen 0 1280x800x24",
				browserPath,
			}
			xvfbArgs = append(xvfbArgs, args...)
			cmd = exec.Command(xvfbPath, xvfbArgs...)
			log.Printf("[Copilot-Refresh] Chạy Chrome ngầm hoàn toàn qua Xvfb ảo (100%% ẩn, không hiện cửa sổ desktop) cho %s...\n", accountID)
		} else {
			cmd = exec.Command(browserPath, append([]string{"--window-position=-2500,-2500"}, args...)...)
			cmd.Env = buildBrowserEnv()
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := cmd.Start(); err != nil {
			log.Printf("[Copilot-Refresh] Không khởi chạy được Chrome: %v\n", err)
			return false
		}

		defer func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				_ = cmd.Process.Signal(syscall.SIGTERM)
				time.Sleep(1 * time.Second)
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			_ = exec.Command("pkill", "-f", fmt.Sprintf("--remote-debugging-port=%d", port)).Run()
			_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
			_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
			_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))
		}()

	clientHTTP := &http.Client{Timeout: 2 * time.Second}
	debugURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	var connectedTabWs string

	for i := 0; i < 20; i++ {
		time.Sleep(1500 * time.Millisecond)
		resp, err := clientHTTP.Get(debugURL)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var tabs []struct {
			URL                  string `json:"url"`
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if err := json.Unmarshal(data, &tabs); err != nil {
			continue
		}

		for _, tab := range tabs {
			if strings.Contains(tab.URL, "m365.cloud.microsoft") && tab.WebSocketDebuggerURL != "" {
				connectedTabWs = tab.WebSocketDebuggerURL
				break
			}
		}
		if connectedTabWs != "" {
			break
		}
	}

	if connectedTabWs == "" {
		log.Printf("[Copilot-Refresh] Không tìm thấy tab Copilot trong Chrome headless cho %s (có thể cần đăng nhập lại).\n", accountID)
		return false
	}

	log.Printf("[Copilot-Refresh] Đã kết nối CDP tab Copilot (%s), đang kích hoạt và bắt token Chathub...\n", accountID)
	tokenURL, err := captureWsFromCDP(connectedTabWs, 35*time.Second)
	if err != nil || tokenURL == "" {
		log.Printf("[Copilot-Refresh] Bắt token thất bại cho %s hoặc timeout: %v\n", accountID, err)
		return false
	}

	log.Printf("[Copilot-Refresh] 🎉 GIA HẠN TOKEN COPILOT THÀNH CÔNG CHO %s!\n", accountID)
	_ = SaveTokenWithID(accountID, tokenURL)
	MarkAccountHealthy(accountID)
	return true
}

// TriggerAutoRefreshHeadless is a backwards-compatible wrapper for TriggerAutoRefreshHeadlessForAccount.
func TriggerAutoRefreshHeadless() bool {
	return TriggerAutoRefreshHeadlessForAccount("")
}
