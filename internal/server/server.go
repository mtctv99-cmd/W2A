package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"geminigo/internal/browser"
	"geminigo/internal/client"
	"geminigo/internal/config"
	"geminigo/internal/copilot"
	"geminigo/internal/stats"
	"geminigo/web"
)

const (
	maxRetries            = 3
	maxRequestBodySize    = 16 << 20        // 16MB for high-res multimodal vision payloads
	maxConcurrentRequests = 10              // Max concurrent requests to Gemini/Copilot API
	requestQueueTimeout   = 10 * time.Second // Queue timeout before returning 503
)

var (
	requestSem = make(chan struct{}, maxConcurrentRequests)
	xsrfRegex  = regexp.MustCompile(`"xsrf","([^"]+)"`)
)

// acquireRequestSlot attempts to acquire a concurrency slot in requestSem, waiting in queue up to timeout.
// Returns a release function and true if acquired; otherwise returns nil and false if timed out or cancelled.
func acquireRequestSlot(ctx context.Context, timeout time.Duration) (func(), bool) {
	// Fast path: if slot is immediately available
	select {
	case requestSem <- struct{}{}:
		return func() { <-requestSem }, true
	default:
	}

	// Slow path: queue waiting up to timeout
	log.Printf("[Queue] Server at max capacity (%d/%d), request waiting in queue (timeout %v)...\n",
		len(requestSem), maxConcurrentRequests, timeout)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case requestSem <- struct{}{}:
		log.Println("[Queue] Slot acquired from queue, proceeding with request.")
		return func() { <-requestSem }, true
	case <-ctx.Done():
		log.Println("[Queue] Client disconnected while waiting in queue.")
		return nil, false
	case <-timer.C:
		log.Printf("[Queue] Request timed out after %v waiting in queue.\n", timeout)
		return nil, false
	}
}

func authorized(r *http.Request) bool {
	if !config.IsAuthRequired() {
		return true
	}
	keys := config.GetApiKeys()
	if len(keys) == 0 {
		return false
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

func sendJSON(w http.ResponseWriter, data interface{}, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func setupCORS(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE, PUT")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID, Session-ID")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return true
	}
	return false
}

func logRequest(r *http.Request) {
	if config.CONFIG.LogRequests {
		log.Printf("[%s] %s %s %s\n", r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
	}
}

func InitServer() {
	loadTasksFromDisk()
	// Periodic zombie cleanup every hour
	go func() {
		for {
			browser.CleanupZombies()
			time.Sleep(1 * time.Hour)
		}
	}()

	// Periodic sweep of old video tasks every 15 minutes
	go func() {
		for {
			time.Sleep(15 * time.Minute)
			SweepOldVideoTasks(1 * time.Hour)
		}
	}()

		// Connect client pool auto-refresh callback to headless Chrome sweep
		client.SetAutoRefreshCallback(func() {
			browser.RefreshAllAccounts()
			_, _ = client.SyncAllSupportedModels()
		})

		// Periodic account cookie refresh & auto-model detection every 1 hour to keep sessions active
		go func() {
			// Run initial sweep after 5 seconds of startup
			time.Sleep(5 * time.Second)
			log.Println("[Background] Running initial profile refresh & live model registry sync...")
			_, _ = client.SyncAllSupportedModels()
			browser.RefreshAllAccounts()

			ticker := time.NewTicker(1 * time.Hour)
			for range ticker.C {
				log.Println("[Background] Running periodic 1h profile refresh & live model sync...")
				_, _ = client.SyncAllSupportedModels()
				browser.RefreshAllAccounts()
			}
		}()
}

func ServeDashboard(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	path := r.URL.Path
	if path == "/" || path == "" {
		data, err := web.StaticFS.ReadFile("index.html")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	if path == "/static/style.css" {
		data, err := web.StaticFS.ReadFile("style.css")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/css")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	if path == "/static/app.js" {
		data, err := web.StaticFS.ReadFile("app.js")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	http.Error(w, "Not Found", http.StatusNotFound)
}

// -------------------------------------------------------------
// ADMIN API ENDPOINTS
// -------------------------------------------------------------

func HandleStats(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	totalReqs, totalToks, lastReq := stats.GetStats()
	sendJSON(w, map[string]interface{}{
		"total_requests":     totalReqs,
		"total_tokens":       totalToks,
		"last_request_at":    lastReq,
		"active_cookies":     len(client.GetAccounts()),
		"api_keys_count":     len(config.GetApiKeys()),
		"require_auth":       config.IsAuthRequired(),
		"is_browser_running": browser.IsAutoLoginRunning(),
		"is_refreshing":      browser.IsRefreshing(),
	}, http.StatusOK)
}

func HandleCookies(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	accs := client.GetAccounts()
	statuses := client.GetAccountStatuses()
	statusMap := make(map[string]client.AccountStatus)
	for _, s := range statuses {
		statusMap[s.ID] = s
	}

		type SafeAcc struct {
			ID              string `json:"id"`
			Sapisid         string `json:"sapisid"`
			ProfileDir      string `json:"profile_dir"`
			Healthy         bool   `json:"healthy"`
			CooldownUntil   string `json:"cooldown_until,omitempty"`
			FailCount       int    `json:"fail_count"`
			RotateAuthError bool   `json:"rotate_auth_error"`
			GoogleAPIKey    string `json:"google_api_key,omitempty"`
			PoolType        string `json:"pool_type"`
			ConfiguredRole  string `json:"configured_role"`
		}
		safeAccs := make([]SafeAcc, len(accs))
		for i, a := range accs {
			pDir := a.ProfileDir
			if pDir == "" {
				pDir = fmt.Sprintf("data/profiles/%s", a.ID)
			}
			st := statusMap[a.ID]
			healthy := true
			if st.ID != "" {
				healthy = st.Healthy
			}
			safeAccs[i] = SafeAcc{
				ID:              a.ID,
				Sapisid:         truncateSapisid(a.Sapisid),
				ProfileDir:      pDir,
				Healthy:         healthy,
				CooldownUntil:   st.CooldownUntil,
				FailCount:       st.FailCount,
				RotateAuthError: st.RotateAuthError,
				GoogleAPIKey:    a.GoogleAPIKey,
				PoolType:        st.PoolType,
				ConfiguredRole:  st.ConfiguredRole,
			}
		}
	sendJSON(w, map[string]interface{}{"cookies": safeAccs, "is_refreshing": browser.IsRefreshing()}, http.StatusOK)
}

func HandleSetCookiePoolType(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID       string `json:"id"`
		PoolType string `json:"pool_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
		return
	}
	if err := client.SetAccountPoolType(req.ID, req.PoolType); err != nil {
		sendJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	sendJSON(w, map[string]interface{}{"status": "ok", "id": req.ID, "pool_type": req.PoolType}, http.StatusOK)
}

func HandleSetCopilotPoolType(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID       string `json:"id"`
		PoolType string `json:"pool_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
		return
	}
	if err := copilot.SetCopilotAccountPoolType(req.ID, req.PoolType); err != nil {
		sendJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	sendJSON(w, map[string]interface{}{"status": "ok", "id": req.ID, "pool_type": req.PoolType}, http.StatusOK)
}

func HandleListSessions(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	sendJSON(w, map[string]interface{}{
		"gemini_sessions":  client.GlobalGeminiSessions.GetActiveSessions(),
		"copilot_sessions": copilot.GlobalCopilotSessions.GetActiveSessions(),
	}, http.StatusOK)
}

func HandlePoolStatus(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	statuses := client.GetAccountStatuses()
	sendJSON(w, map[string]interface{}{
		"accounts":        statuses,
		"total":           len(statuses),
		"active_sessions": browser.GlobalSessionPool.GetActiveSessionCount(),
		"sessions":        browser.GlobalSessionPool.GetSessionsInfo(),
	}, http.StatusOK)
}

func HandleAutoLogin(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqData)

	browser.StartChromeAutoLoginWithURL(reqData.ID, reqData.URL)
	sendJSON(w, map[string]interface{}{"success": true, "message": "Chrome launched for auto login"}, http.StatusOK)
}

func HandleRefreshAccounts(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqData)

	if browser.IsRefreshing() {
		sendJSON(w, map[string]interface{}{
			"success":       true,
			"is_refreshing": true,
			"message":       "Auto-refresh is already in progress in background.",
		}, http.StatusOK)
		return
	}

	go func() {
		if reqData.ID != "" {
			_, _, _ = browser.RefreshAccountFromProfile(reqData.ID)
		} else {
			_, _ = browser.RefreshAllAccounts()
		}
	}()

	sendJSON(w, map[string]interface{}{
		"success":       true,
		"is_refreshing": true,
		"message":       "Triggered background profile refresh and model detection",
	}, http.StatusOK)
}

func HandleDeleteCookie(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Invalid JSON"}, http.StatusBadRequest)
		return
	}

	if err := client.DeleteAccountCookie(reqData.ID); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": err.Error()}, http.StatusInternalServerError)
		return
	}

	sendJSON(w, map[string]interface{}{"success": true}, http.StatusOK)
}

func HandleSaveCookie(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Invalid JSON"}, http.StatusBadRequest)
		return
	}

	rawCookie := strings.TrimSpace(reqData.Cookie)
	if rawCookie == "" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Cookie is empty"}, http.StatusBadRequest)
		return
	}

	if err := client.AddAccountCookie(rawCookie); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": err.Error()}, http.StatusInternalServerError)
		return
	}

		sendJSON(w, map[string]interface{}{"success": true, "message": "Cookie saved"}, http.StatusOK)
	}

	func HandleSaveAccountGoogleApiKey(w http.ResponseWriter, r *http.Request) {
		if setupCORS(w, r) {
			return
		}
		if r.Method != "POST" {
			sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		var reqData struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
			sendJSON(w, map[string]interface{}{"success": false, "message": "Invalid JSON"}, http.StatusBadRequest)
			return
		}

		if reqData.ID == "" {
			sendJSON(w, map[string]interface{}{"success": false, "message": "Missing account id"}, http.StatusBadRequest)
			return
		}

		cleanKey := strings.TrimSpace(reqData.Key)
		if err := client.SaveAccountGoogleAPIKey(reqData.ID, cleanKey); err != nil {
			sendJSON(w, map[string]interface{}{"success": false, "message": err.Error()}, http.StatusInternalServerError)
			return
		}

		sendJSON(w, map[string]interface{}{"success": true, "message": "API key saved"}, http.StatusOK)
	}

func HandleApiKeys(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method == "GET" {
		sendJSON(w, map[string]interface{}{
			"keys":         config.GetApiKeys(),
			"require_auth": config.IsAuthRequired(),
		}, http.StatusOK)
		return
	}
	sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
}

func HandleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	keys := config.GetApiKeys()
	sendJSON(w, map[string]interface{}{
		"require_auth":   config.IsAuthRequired(),
		"api_keys_count": len(keys),
	}, http.StatusOK)
}

func HandleAuthToggle(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		RequireAuth *bool `json:"require_auth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqData)

	newVal := !config.IsAuthRequired()
	if reqData.RequireAuth != nil {
		newVal = *reqData.RequireAuth
	}

	if err := config.SetRequireAuth(newVal); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Failed to save configuration: " + err.Error()}, http.StatusInternalServerError)
		return
	}

	sendJSON(w, map[string]interface{}{"success": true, "require_auth": newVal}, http.StatusOK)
}

func HandleAddApiKey(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Invalid JSON"}, http.StatusBadRequest)
		return
	}

	newKey, err := config.AddApiKey(reqData.Key)
	if err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Failed to save configuration: " + err.Error()}, http.StatusInternalServerError)
		return
	}

	sendJSON(w, map[string]interface{}{"success": true, "key": newKey}, http.StatusOK)
}

func HandleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	var reqData struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Invalid JSON"}, http.StatusBadRequest)
		return
	}

	if err := config.DeleteApiKey(reqData.Key); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": err.Error()}, http.StatusNotFound)
		return
	}

	sendJSON(w, map[string]interface{}{"success": true}, http.StatusOK)
}

func HandleAdminModels(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	sendJSON(w, map[string]interface{}{
		"models":           GetActiveSupportedModels(),
		"all_models":       config.GetModels(),
		"has_google_auth":  client.GetAuthenticatedAccountCount() > 0,
		"has_copilot_auth": copilot.GetHealthyAccountCount() > 0,
	}, http.StatusOK)
}

func HandleSyncModels(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	go func() {
		_, _ = client.SyncAllSupportedModels()
		_, _ = browser.RefreshAllAccounts()
	}()

	sendJSON(w, map[string]interface{}{"success": true, "message": "Triggered live model sync from Google API & Gemini Web"}, http.StatusOK)
}

func HandleLogs(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logPath := "data/geminigo.log"
	logsStr := readLastLines(logPath, 100)
	sendJSON(w, map[string]interface{}{"logs": logsStr}, http.StatusOK)
}

func HandleClearLogs(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"success": false, "message": "Method not allowed"}, http.StatusMethodNotAllowed)
		return
	}
	logPath := "data/geminigo.log"
	_ = os.WriteFile(logPath, []byte(""), 0644)
	sendJSON(w, map[string]interface{}{"success": true, "message": "Logs cleared"}, http.StatusOK)
}

func readLastLines(path string, maxLines int) string {
	file, err := os.Open(path)
	if err != nil {
		return "No logs generated yet or log file inaccessible: " + err.Error()
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil || stat.Size() == 0 {
		return ""
	}

	bufSize := int64(64 * 1024)
	if bufSize > stat.Size() {
		bufSize = stat.Size()
	}

	buf := make([]byte, bufSize)
	_, err = file.ReadAt(buf, stat.Size()-bufSize)
	if err != nil && err != io.EOF {
		return ""
	}

	content := strings.TrimRight(string(buf), "\r\n")
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

// -------------------------------------------------------------
// OPENAI API HANDLERS
// -------------------------------------------------------------

func GetActiveSupportedModels() map[string]config.ModelCfg {
	all := config.GetModels()
	hasGoogleAuth := client.GetAuthenticatedAccountCount() > 0
	hasCopilotAuth := copilot.GetHealthyAccountCount() > 0

	active := make(map[string]config.ModelCfg)
	for name, cfg := range all {
		isCopilot := strings.HasPrefix(name, "copilot") || strings.HasPrefix(name, "gpt-") || strings.HasPrefix(name, "dall-e")
		if isCopilot {
			if hasCopilotAuth {
				active[name] = cfg
			}
			continue
		}

		// Google Gemini models
		if !hasGoogleAuth {
			// Without Google login, Google only supports Guest mode on gemini-3.5-flash-lite!
			if name == "gemini-3.5-flash-lite" {
				active[name] = cfg
			}
		} else {
			active[name] = cfg
		}
	}
	return active
}

func HandleModels(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logRequest(r)
	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	activeModels := GetActiveSupportedModels()
	var data []map[string]interface{}
	for name := range activeModels {
		ownedBy := "google"
		if strings.HasPrefix(name, "copilot") {
			ownedBy = "microsoft"
		} else if strings.HasPrefix(name, "gpt-") || strings.HasPrefix(name, "dall-e") {
			ownedBy = "openai"
		}
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  1700000000,
			"owned_by": ownedBy,
		})
	}
	sendJSON(w, map[string]interface{}{"object": "list", "data": data}, http.StatusOK)
}

type ChatRequest struct {
	Model      string           `json:"model"`
	Messages   []client.Message `json:"messages"`
	Stream     bool             `json:"stream"`
	Tools      []client.Tool    `json:"tools,omitempty"`
	ToolChoice interface{}      `json:"tool_choice,omitempty"`
}

func HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logRequest(r)
	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	release, ok := acquireRequestSlot(r.Context(), requestQueueTimeout)
	if !ok {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Server busy, request timed out waiting in queue"}}, http.StatusServiceUnavailable)
		return
	}
	defer release()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}

	rawModelName := req.Model
	if rawModelName == "" {
		rawModelName = config.CONFIG.DefaultModel
	}
	modelName, routeMode := config.ResolveModelRouting(rawModelName)

	if routeMode == "copilot" || copilot.IsCopilotModel(modelName) {
		handleCopilotChat(w, r, req, modelName)
		return
	}

	mCfg, ok := config.GetModelCfg(modelName)
	if !ok {
		mCfg, _ = config.GetModelCfg("gemini-3.8-flash")
	}

	hasTools := len(req.Tools) > 0 && req.ToolChoice != "none"
	thinkMode := mCfg.Think
	if hasTools && thinkMode > 0 {
		thinkMode = 0
	}

	hasToolResults := false
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			hasToolResults = true
			break
		}
	}

		prompt, parsedImages := client.MessagesToPrompt(req.Messages, req.Tools, req.ToolChoice)
		if hasToolResults {
			prompt += "\n\nYou have received the tool results above. Now provide your final response based on these results. Only call another tool if the user explicitly requests it or if you absolutely need additional information."
		}

		explicitSessionID := r.Header.Get("X-Session-ID")
		if explicitSessionID == "" {
			explicitSessionID = r.Header.Get("Session-ID")
		}
		if explicitSessionID == "" {
			explicitSessionID = r.URL.Query().Get("session_id")
		}

		sessionID := explicitSessionID
		if sessionID == "" {
			sessionID = fmt.Sprintf("chat_%d", time.Now().UnixNano())
		}

		var geminiSess *client.GeminiSession
		poolRole := client.PoolTypeWorker
		if explicitSessionID != "" {
			poolRole = client.PoolTypeSession
			geminiSess, _ = client.GlobalGeminiSessions.GetOrCreateSession(explicitSessionID)
		}

		// ponytail: Google API completely bypassed as user requested web-only
		maxRetries := client.GetHealthyAccountCount()
		if maxRetries < 3 {
			maxRetries = 3
		}
		if maxRetries > 7 {
			maxRetries = 7
		}
		var lastErr error
		var lastErrType string
		sameErrorCount := 0
		failedAccounts := make([]string, 0, maxRetries)

		for attempt := 0; attempt < maxRetries; attempt++ {
			var cookieStr, sapisid, accountID string
			if poolRole == client.PoolTypeSession && geminiSess != nil {
				// Sticky session routing
				if attempt == 0 && geminiSess.AccountID != "" {
					c, s, id, _, healthy := client.GetCookieByAccountID(geminiSess.AccountID)
					if healthy && c != "" {
						cookieStr, sapisid, accountID = c, s, id
						log.Printf("[StickySession] Session %s routed to bound account %s (Turn #%d)\n",
							geminiSess.SessionID, accountID, geminiSess.TurnCount+1)
					} else {
						log.Printf("[StickySession] Bound account %s in cooldown/unhealthy, reallocating session account\n",
							geminiSess.AccountID)
					}
				}
				if cookieStr == "" {
					cookieStr, sapisid, accountID, _ = client.GetNextCookieForType(client.PoolTypeSession)
					if accountID != "" {
						client.GlobalGeminiSessions.BindAccount(geminiSess.SessionID, accountID)
					}
				}
			} else {
				if len(parsedImages) > 0 {
					cookieStr, sapisid, accountID, _ = client.GetNextHealthyCookieForVision()
				} else {
					cookieStr, sapisid, accountID, _ = client.GetNextCookieForType(client.PoolTypeWorker)
				}
			}
			if cookieStr == "" {
				if modelName == "gemini-3.5-flash-lite" || client.GetAuthenticatedAccountCount() == 0 {
					log.Printf("[GuestMode] Executing via Gemini Flash-Lite Guest mode (no accounts required)...\n")
					reply, guestErr := browser.GenerateGuestChat(r.Context(), prompt)
					if guestErr == nil && reply != "" {
						cid := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
						if req.Stream {
							flusher, ok := w.(http.Flusher)
							if ok {
								w.Header().Set("Content-Type", "text/event-stream")
								w.Header().Set("Cache-Control", "no-cache")
								w.Header().Set("Connection", "keep-alive")
								w.Header().Set("Access-Control-Allow-Origin", "*")
								w.WriteHeader(http.StatusOK)

								chunk := map[string]interface{}{
									"id":      cid,
									"object":  "chat.completion.chunk",
									"created": time.Now().Unix(),
									"model":   "gemini-3.5-flash-lite",
									"choices": []map[string]interface{}{
										{
											"index":         0,
											"delta":         map[string]string{"content": reply},
											"finish_reason": nil,
										},
									},
								}
								data, _ := json.Marshal(chunk)
								fmt.Fprintf(w, "data: %s\n\n", data)
								stopChunk := map[string]interface{}{
									"id":      cid,
									"object":  "chat.completion.chunk",
									"created": time.Now().Unix(),
									"model":   "gemini-3.5-flash-lite",
									"choices": []map[string]interface{}{
										{
											"index":         0,
											"delta":         map[string]interface{}{},
											"finish_reason": "stop",
										},
									},
								}
								stopData, _ := json.Marshal(stopChunk)
								fmt.Fprintf(w, "data: %s\n\n", stopData)
								fmt.Fprintf(w, "data: [DONE]\n\n")
								flusher.Flush()
								stats.UpdateStats(int64((len(prompt) + len(reply)) / 4))
								return
							}
						}
						resp := map[string]interface{}{
							"id":      cid,
							"object":  "chat.completion",
							"created": time.Now().Unix(),
							"model":   "gemini-3.5-flash-lite",
							"choices": []map[string]interface{}{
								{
									"index": 0,
									"message": map[string]interface{}{
										"role":    "assistant",
										"content": reply,
									},
									"finish_reason": "stop",
								},
							},
							"usage": map[string]interface{}{
								"prompt_tokens":     len(prompt) / 4,
								"completion_tokens": len(reply) / 4,
								"total_tokens":      (len(prompt) + len(reply)) / 4,
							},
						}
						stats.UpdateStats(int64((len(prompt) + len(reply)) / 4))
						sendJSON(w, resp, http.StatusOK)
						return
					}
					lastErr = fmt.Errorf("guest mode error: %v", guestErr)
				}
				log.Printf("[Pool] No healthy accounts available (attempt %d/%d)\n", attempt+1, maxRetries)
				if lastErr == nil {
					lastErr = fmt.Errorf("no healthy accounts available (model %s requires an authenticated account)", modelName)
				}
				break
			}
		log.Printf("[Pool] Attempt %d/%d: %s (images: %d, role: %s)\n", attempt+1, maxRetries, accountID, len(parsedImages), poolRole)

		var fileRefs []client.UploadedFile
		if len(parsedImages) > 0 {
			uploadErr := false
			for idx, img := range parsedImages {
				fileName := fmt.Sprintf("image_%d%s", idx+1, client.ExtensionForMimeType(img.Mime))
				ref, err := client.UploadImage(img.Data, fileName, img.Mime, cookieStr)
				if err != nil {
					log.Printf("[Vision] Image upload failed for %s: %v\n", accountID, err)
					lastErr = fmt.Errorf("vision upload failed: %w", err)
					failedAccounts = append(failedAccounts, accountID)
					client.MarkCooldown(accountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
					uploadErr = true
					break
				}
				fileRefs = append(fileRefs, ref)
			}
			if uploadErr {
				continue
			}
		}

		if req.Stream {
			if modelName == "gemini-veo" || modelName == "gemini-imagen" {
				req.Stream = false
			} else {
				err := handleChatStream(w, prompt, modelName, mCfg.Mode, thinkMode, fileRefs, hasTools, cookieStr, sapisid, geminiSess)
				if err == nil {
					return
				}
				lastErr = err
	
					if geminiErr, ok := err.(*client.GeminiError); ok {
						log.Printf("[Pool] %s failed: %s\n", accountID, geminiErr.Error())
						cooldown := client.GetCooldownDuration(geminiErr.Type, attempt+1)
						client.MarkCooldown(accountID, cooldown)
						if geminiErr.Type == "AUTH_ERROR" {
							log.Printf("[Pool] %s session expired, auto-refreshing in background...\n", accountID)
							go browser.RefreshAccountFromProfile(accountID)
						}
						failedAccounts = append(failedAccounts, accountID)
						if geminiErr.Type == lastErrType {
							sameErrorCount++
						} else {
							sameErrorCount = 1
							lastErrType = geminiErr.Type
						}
						continue
					}
					log.Printf("[Pool] %s network error: %v\n", accountID, err)
					client.MarkCooldown(accountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
					failedAccounts = append(failedAccounts, accountID)
					lastErrType = "SERVER_ERROR"
					sameErrorCount++
					continue
			}
		}

			var raw string
			var rawBody string
			var err error
			videoURL := ""

		if modelName == "gemini-veo" {
			log.Println("[Video] Triggering Playwright browser to generate video...")
			cdpURL, chatURL, cdpErr := browser.StartCDPMediaGenerationWithSession(prompt, sessionID, "video")
			if cdpErr != nil || cdpURL == "" {
				err = cdpErr
				if err == nil {
					err = fmt.Errorf("empty video url")
				}
			} else {
				log.Printf("[Video] Successfully retrieved Playwright Video URL: %s (Chat: %s)\n", cdpURL, chatURL)
				finalVideoURL := cdpURL
				if strings.HasPrefix(cdpURL, "data:video/") {
					filename := fmt.Sprintf("video_%s.mp4", newRandID())
					filePath := filepath.Join("data/files", filename)
					rawB64 := cdpURL
					if idx := strings.Index(cdpURL, "base64,"); idx != -1 {
						rawB64 = cdpURL[idx+7:]
					}
					vData, b64Err := base64.StdEncoding.DecodeString(rawB64)
					if b64Err == nil && len(vData) > 0 {
						_ = os.MkdirAll("data/files", 0755)
							if os.WriteFile(filePath, vData, 0644) == nil {
								scheme := getRequestScheme(r)
								finalVideoURL = fmt.Sprintf("%s://%s/v1/files/%s", scheme, r.Host, filename)
							}
					}
				}
				raw = fmt.Sprintf("[Video Output](%s)", finalVideoURL)
				videoURL = finalVideoURL
			}
			} else if modelName == "gemini-imagen" {
				// 1. Fast Path: Try Google Gemini HTTP RPC first (returns clean 2048x2048 CDN assets in ~3s)
				parsed, rawBody, rpcErr := client.GenerateTextRaw(prompt, mCfg.Mode, 0, nil, cookieStr, sapisid)
				var imgList []string
				if rpcErr == nil {
					imgList = extractImageURLs(parsed, rawBody)
				}

				if len(imgList) > 0 {
					raw = ""
					for _, u := range imgList {
						u = strings.TrimSpace(u)
						if u == "" || strings.Contains(u, "/image_generation_content/") {
							continue
						}
						// Ensure =s2048 suffix for high resolution
						if strings.Contains(u, "googleusercontent.com") {
							reS := regexp.MustCompile(`=s\d+`)
							if reS.MatchString(u) {
								u = reS.ReplaceAllString(u, "=s2048")
							} else if !strings.Contains(u, "=s") {
								u += "=s2048"
							}
						}
						b64, mimeType := fetchImageAsBase64(u, cookieStr, sapisid)
						if b64 != "" {
							dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)
							publicURL, sErr := saveBase64ImageLocally(dataURI, r)
							if sErr == nil && publicURL != "" {
								raw += fmt.Sprintf("![Image](%s)\n\n", publicURL)
							} else {
								raw += fmt.Sprintf("![Image](%s)\n\n", u)
							}
						} else {
							raw += fmt.Sprintf("![Image](%s)\n\n", u)
						}
					}
				}

				// 2. Fallback: If HTTP RPC returned no images, try Playwright browser context
				if raw == "" {
					log.Println("[Image] HTTP RPC returned no images, triggering Playwright browser...")
					cdpURLs, chatURL, cdpErr := browser.StartCDPMediaGenerationWithSession(prompt, sessionID, "image")
					if cdpErr != nil || cdpURLs == "" {
						err = cdpErr
						if err == nil {
							err = fmt.Errorf("empty image url")
						}
					} else {
						log.Printf("[Image] Successfully retrieved Playwright Image URLs: %s (Chat: %s)\n", cdpURLs, chatURL)
						urls := strings.Split(cdpURLs, "|||")
						raw = ""
						for _, u := range urls {
							imgURL := u
							if strings.HasPrefix(u, "data:image/") {
								publicURL, sErr := saveBase64ImageLocally(u, r)
								if sErr == nil && publicURL != "" {
									imgURL = publicURL
								}
							}
							raw += fmt.Sprintf("![Image](%s)\n\n", imgURL)
						}
					}
					}
				} else {
					var meta client.ConversationMetadata
					convUUID := ""
					if geminiSess != nil {
						meta = client.ConversationMetadata{
							ConversationID: geminiSess.ConversationID,
							ResponseID:     geminiSess.ResponseID,
							ChoiceID:       geminiSess.ChoiceID,
						}
						convUUID = geminiSess.ConvUUID
					}
					raw, rawBody, err = client.GenerateTextRawWithMetadata(prompt, mCfg.Mode, thinkMode, fileRefs, cookieStr, sapisid, convUUID, meta)
				}
				if err != nil {
				lastErr = err

					if geminiErr, ok := err.(*client.GeminiError); ok {
						log.Printf("[Pool] %s failed: %s\n", accountID, geminiErr.Error())
						cooldown := client.GetCooldownDuration(geminiErr.Type, attempt+1)
						client.MarkCooldown(accountID, cooldown)
						if geminiErr.Type == "AUTH_ERROR" {
							log.Printf("[Pool] %s session expired, auto-refreshing in background...\n", accountID)
							go browser.RefreshAccountFromProfile(accountID)
						}
						failedAccounts = append(failedAccounts, accountID)
						if geminiErr.Type == lastErrType {
							sameErrorCount++
						} else {
							sameErrorCount = 1
							lastErrType = geminiErr.Type
						}
						continue
					}
					log.Printf("[Pool] %s error: %v\n", accountID, err)
					client.MarkCooldown(accountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
					failedAccounts = append(failedAccounts, accountID)
					lastErrType = "SERVER_ERROR"
					sameErrorCount++
					continue
				}

				client.MarkHealthy(accountID)

			var toolCalls []client.ParsedToolCall
			cleanText := raw
			if hasTools {
				cleanText, toolCalls = client.ParseToolCalls(raw)
			}

				if modelName != "gemini-imagen" {
					imgList := extractImageURLs(cleanText, rawBody)
					if len(imgList) > 0 {
						for _, u := range imgList {
							u = strings.TrimSpace(u)
							if u == "" || strings.Contains(u, "/image_generation_content/") {
								continue
							}
							b64, mimeType := fetchImageAsBase64(u, cookieStr, sapisid)
							if b64 != "" {
								dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)
								publicURL, sErr := saveBase64ImageLocally(dataURI, r)
								if sErr == nil && publicURL != "" {
									imgMD := fmt.Sprintf("![Image](%s)", publicURL)
									if strings.Contains(cleanText, "image_generation_content") {
										rePlace := regexp.MustCompile(`https?://[^\s"'<>\\]*googleusercontent\.com/image_generation_content/\S+`)
										cleanText = rePlace.ReplaceAllString(cleanText, imgMD)
									} else {
										cleanText += "\n\n" + imgMD
									}
								}
							}
						}
					}
					rePlace := regexp.MustCompile(`https?://[^\s"'<>\\]*googleusercontent\.com/image_generation_content/\S+\n?`)
					cleanText = rePlace.ReplaceAllString(cleanText, "")

					images := client.CollectImages(cleanText)
					if len(images) > 0 {
						for _, img := range images {
							if !strings.Contains(cleanText, fmt.Sprintf("![Image](%s)", img)) {
								cleanText += fmt.Sprintf("\n\n![Image](%s)\n", img)
							}
						}
					}
				}

			if modelName != "gemini-veo" {
				videos := client.CollectVideos(raw)
				if len(videos) > 0 {
					if videoURL == "" {
						videoURL = videos[0]
					}
				}
			}

		thinking, finalContent := client.ExtractThinkingAndText(cleanText)
		messageObj := map[string]interface{}{
			"role":    "assistant",
			"content": finalContent,
		}
		if thinking != "" {
			messageObj["reasoning_content"] = thinking
		}
		if videoURL != "" {
			messageObj["video_url"] = videoURL
		}

		finishReason := "stop"
		if len(toolCalls) > 0 {
			messageObj["tool_calls"] = toolCalls
			messageObj["content"] = nil
			finishReason = "tool_calls"
		}

		resp := map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"message":       messageObj,
					"finish_reason": finishReason,
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     len(prompt) / 4,
				"completion_tokens": len(raw) / 4,
				"total_tokens":      (len(prompt) + len(raw)) / 4,
			},
		}

			stats.UpdateStats(int64((len(prompt) + len(raw)) / 4))
			if geminiSess != nil {
				cID, rID, rcID := client.ExtractConversationMetadata(rawBody)
				if cID != "" || rID != "" {
					client.GlobalGeminiSessions.UpdateSessionMetadata(geminiSess.SessionID, cID, rID, rcID)
				}
			}
			sendJSON(w, resp, http.StatusOK)
			return
	}

	if lastErr != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": lastErr.Error()}}, http.StatusServiceUnavailable)
	} else {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "all accounts exhausted"}}, http.StatusServiceUnavailable)
	}
}

func truncateSapisid(s string) string {
	if len(s) > 0 {
		return s[:min(len(s), 15)] + "..."
	}
	return ""
}

func handleChatStream(w http.ResponseWriter, prompt, modelName string, mode, think int, fileRefs []client.UploadedFile, hasTools bool, cookieStr, sapisid string, sess *client.GeminiSession) error {
	clientHTTP := client.GetStreamHTTPClient()
	_, _, xsrf, bl, fsid := client.GetFullPageTokens(cookieStr)

	var meta client.ConversationMetadata
	convUUID := ""
	if sess != nil {
		meta = client.ConversationMetadata{
			ConversationID: sess.ConversationID,
			ResponseID:     sess.ResponseID,
			ChoiceID:       sess.ChoiceID,
		}
		convUUID = sess.ConvUUID
	}

	body := client.BuildPayloadWithMetadata(prompt, modelName, mode, think, fileRefs, xsrf, convUUID, meta)
	streamURL := client.GetStreamURLWithParams(0, bl, fsid)
	req, err := http.NewRequest("POST", streamURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = client.BuildHeadersWithCookie(cookieStr, sapisid)
	req.Header.Set("x-goog-ext-525001261-jspb", client.ModelHeader(mode))

		resp, err := clientHTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if len(resp.Cookies()) > 0 {
			merged := client.MergeCookieString(cookieStr, resp.Cookies())
			if merged != cookieStr {
				client.UpdateCookieInPool(cookieStr, merged)
			}
		}

	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 2048)
		n, _ := io.ReadFull(resp.Body, buf)
		firstLines := string(buf[:n])

		if geminiErr := client.ClassifyStreamError(resp, firstLines); geminiErr != nil {
			return geminiErr
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLines)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	reader := bufio.NewReader(resp.Body)
	cid := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	prevText := ""
	var prevThinking string
	var prevTextContent string
	var totalBytesRead int64
	var rawBody strings.Builder

	var headersWritten bool
	ensureHeaders := func() {
		if !headersWritten {
			headersWritten = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		rawBody.WriteString(line)

			if !headersWritten && strings.Contains(line, `"xsrf",`) {
				if m := xsrfRegex.FindStringSubmatch(line); len(m) > 1 {
					newXsrf := m[1]
					log.Printf("[XSRF] Streaming detected XSRF error. Updating token and retrying...\n")
					client.UpdateXsrfToken(newXsrf, cookieStr)

					return handleChatStream(w, prompt, modelName, mode, think, fileRefs, hasTools, cookieStr, sapisid, sess)
				}
			}

			if strings.Contains(line, "rate limit") || strings.Contains(line, "captcha") || strings.Contains(line, "BardErrorInfo") {
				if !headersWritten {
					return client.ClassifyStreamError(resp, line)
				}
				log.Printf("[Stream] Gemini error mid-stream: %s\n", line[:min(len(line), 200)])
				break
			}

		totalBytesRead += int64(len(line))
		for _, t := range client.ExtractTextsFromLine(line) {
			if len(t) > len(prevText) {
				prevText = t
				currThinking, currText := client.ExtractThinkingAndText(t)

				if len(currThinking) > len(prevThinking) {
					deltaThinking := currThinking[len(prevThinking):]
					prevThinking = currThinking
					ensureHeaders()
					chunk := map[string]interface{}{
						"id":      cid,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   modelName,
						"choices": []map[string]interface{}{
							{
								"index": 0,
								"delta": map[string]string{
									"reasoning_content": deltaThinking,
								},
								"finish_reason": nil,
							},
						},
					}
					data, _ := json.Marshal(chunk)
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}

				if len(currText) > len(prevTextContent) {
					deltaText := currText[len(prevTextContent):]
					prevTextContent = currText

					if hasTools && strings.Contains(currText, "```tool_call") {
						continue
					}

					ensureHeaders()
					chunk := map[string]interface{}{
						"id":      cid,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   modelName,
						"choices": []map[string]interface{}{
							{
								"index": 0,
								"delta": map[string]string{
									"content": deltaText,
								},
								"finish_reason": nil,
							},
						},
					}
					data, _ := json.Marshal(chunk)
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
		}
	}

	ensureHeaders()
	cleanedText := client.CleanText(prevText)
	if hasTools {
		_, toolCalls := client.ParseToolCalls(prevText)
		if len(toolCalls) > 0 {
			chunk := map[string]interface{}{
				"id":      cid,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"delta": map[string]interface{}{
							"content":    nil,
							"tool_calls": toolCalls,
						},
						"finish_reason": "tool_calls",
					},
				},
			}
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}

	images := client.CollectImages(prevText)
	videos := client.ExtractVideoURLs(rawBody.String())
	
	if len(images) > 0 || len(videos) > 0 {
		var mediaText strings.Builder
		mediaText.WriteString("\n\n")
		for _, img := range images {
			mediaText.WriteString(fmt.Sprintf("![Image](%s)\n", img))
		}
		for _, vid := range videos {
			mediaText.WriteString(fmt.Sprintf("[Video Output](%s)\n", vid))
		}
		chunk := map[string]interface{}{
			"id":      cid,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]string{
						"content": mediaText.String(),
					},
					"finish_reason": nil,
				},
			},
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	stopChunk := map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	data, _ := json.Marshal(stopChunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	stats.UpdateStats(int64((len(prompt) + len(cleanedText)) / 4))
	if sess != nil {
		cID, rID, rcID := client.ExtractConversationMetadata(rawBody.String())
		if cID != "" || rID != "" {
			client.GlobalGeminiSessions.UpdateSessionMetadata(sess.SessionID, cID, rID, rcID)
		}
	}
	return nil
}

func handleCopilotChat(w http.ResponseWriter, r *http.Request, req ChatRequest, modelName string) {
	prompt, parsedImages := client.MessagesToPrompt(req.Messages, nil, nil)
	if prompt == "" && len(req.Messages) > 0 {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				if s, ok := req.Messages[i].Content.(string); ok && s != "" {
					prompt = s
					break
				}
			}
		}
	}
	if prompt == "" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Empty prompt or messages"}}, http.StatusBadRequest)
		return
	}

	rawModel := strings.ToLower(strings.TrimSpace(modelName))
	isImageModel := rawModel == "copilot-image" || rawModel == "copilot-dalle" || strings.HasPrefix(rawModel, "dall-e") || rawModel == "dalle"
	if isImageModel {
		imgPrompt := prompt
		lowerPrompt := strings.ToLower(imgPrompt)
		if !strings.HasPrefix(lowerPrompt, "generate") && !strings.HasPrefix(lowerPrompt, "create") && !strings.HasPrefix(lowerPrompt, "draw") && !strings.HasPrefix(lowerPrompt, "vẽ") && !strings.HasPrefix(lowerPrompt, "tạo") {
			imgPrompt = fmt.Sprintf("Generate an image of %s", prompt)
		}

		maxAttempts := copilot.GetHealthyAccountCount()
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		if maxAttempts > 3 {
			maxAttempts = 3
		}

		var dataURLs []string
		var lastErr error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			acc, err := copilot.GetNextHealthyAccountForType(copilot.PoolTypeWorker)
			if err != nil {
				lastErr = err
				break
			}
			dataURLs, err = copilot.GenerateImageWithAccount(acc, imgPrompt)
			if err == nil && len(dataURLs) > 0 {
				copilot.MarkAccountHealthy(acc.ID)
				lastErr = nil
				break
			}
			log.Printf("[CopilotImageChat] Attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
			lastErr = err
			copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
			go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
		}

		if lastErr != nil || len(dataURLs) == 0 {
			errMsg := "Copilot image generation failed"
			if lastErr != nil {
				errMsg = lastErr.Error()
			}
			sendJSON(w, map[string]interface{}{"error": map[string]string{"message": errMsg}}, http.StatusInternalServerError)
			return
		}

		var imageMD strings.Builder
		for _, u := range dataURLs {
			publicURL, sErr := saveBase64ImageLocally(u, r)
			if sErr == nil && publicURL != "" {
				imageMD.WriteString(fmt.Sprintf("![Generated Image](%s)\n\n", publicURL))
			} else {
				imageMD.WriteString(fmt.Sprintf("![Generated Image](%s)\n\n", u))
			}
		}

		cleanContent := strings.TrimSpace(imageMD.String())
		cid := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

		if req.Stream {
			flusher, ok := w.(http.Flusher)
			if ok {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusOK)

				chunk := map[string]interface{}{
					"id":      cid,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   modelName,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"delta": map[string]string{"content": cleanContent},
							"finish_reason": nil,
						},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				stopChunk := map[string]interface{}{
					"id":      cid,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   modelName,
					"choices": []map[string]interface{}{
						{
							"index":         0,
							"delta":         map[string]interface{}{},
							"finish_reason": "stop",
						},
					},
				}
				stopData, _ := json.Marshal(stopChunk)
				fmt.Fprintf(w, "data: %s\n\n", stopData)
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				stats.UpdateStats(int64((len(prompt) + len(cleanContent)) / 4))
				return
			}
		}

		resp := map[string]interface{}{
			"id":      cid,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": cleanContent,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     len(prompt) / 4,
				"completion_tokens": len(cleanContent) / 4,
				"total_tokens":      (len(prompt) + len(cleanContent)) / 4,
			},
		}
		stats.UpdateStats(int64((len(prompt) + len(cleanContent)) / 4))
		sendJSON(w, resp, http.StatusOK)
		return
	}

	explicitSessionID := r.Header.Get("X-Session-ID")
	if explicitSessionID == "" {
		explicitSessionID = r.Header.Get("Session-ID")
	}
	if explicitSessionID == "" {
		explicitSessionID = r.URL.Query().Get("session_id")
	}

	var copilotSess *copilot.CopilotSession
	poolRole := copilot.PoolTypeWorker
	if explicitSessionID != "" {
		poolRole = copilot.PoolTypeSession
		copilotSess, _ = copilot.GlobalCopilotSessions.GetOrCreateSession(explicitSessionID)
	}

	var imageDataURI string
	if len(parsedImages) > 0 {
		b64 := base64.StdEncoding.EncodeToString(parsedImages[0].Data)
		mime := parsedImages[0].Mime
		if mime == "" {
			mime = "image/png"
		}
		imageDataURI = fmt.Sprintf("data:%s;base64,%s", mime, b64)
	}

	cid := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	scheme := getRequestScheme(r)
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	maxAttempts := copilot.GetHealthyAccountCount()
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if maxAttempts > 3 {
		maxAttempts = 3
	}

	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Streaming unsupported"}}, http.StatusInternalServerError)
			return
		}

		var fullReply string
		var lastErr error
		headersSent := false

		ensureHeaders := func() {
			if !headersSent {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusOK)
				flusher.Flush()
				headersSent = true
			}
		}

		onChunk := func(delta string) {
			ensureHeaders()
			if strings.Contains(delta, "](/v1/files/") {
				delta = strings.ReplaceAll(delta, "](/v1/files/", fmt.Sprintf("](%s/v1/files/", baseURL))
			}
			chunk := map[string]interface{}{
				"id":      cid,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"delta": map[string]string{
							"content": delta,
						},
						"finish_reason": nil,
					},
				},
			}
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		for attempt := 0; attempt < maxAttempts; attempt++ {
			var acc *copilot.CopilotAccount
			var err error
			if poolRole == copilot.PoolTypeSession && copilotSess != nil {
				if attempt == 0 && copilotSess.AccountID != "" {
					if a, aErr := copilot.GetCopilotAccountByID(copilotSess.AccountID); aErr == nil {
						acc = a
						log.Printf("[CopilotSticky] Session %s routed to bound account %s (Turn #%d)\n",
							copilotSess.SessionID, acc.ID, copilotSess.TurnCount+1)
					} else {
						log.Printf("[CopilotSticky] Bound account %s in cooldown/expired: %v, reallocating\n",
							copilotSess.AccountID, aErr)
					}
				}
				if acc == nil {
					acc, err = copilot.GetNextHealthyAccountForType(copilot.PoolTypeSession)
					if err == nil && acc != nil {
						copilot.GlobalCopilotSessions.BindAccount(copilotSess.SessionID, acc.ID)
					}
				}
			} else {
				acc, err = copilot.GetNextHealthyAccountForType(copilot.PoolTypeWorker)
			}

			if err != nil || acc == nil {
				if err != nil {
					lastErr = err
				} else {
					lastErr = fmt.Errorf("no healthy copilot accounts available")
				}
				break
			}

			fullReply, err = copilot.GenerateChatWithSession(acc, copilotSess, prompt, modelName, true, imageDataURI, onChunk)
			if err == nil {
				copilot.MarkAccountHealthy(acc.ID)
				lastErr = nil
				break
			}
			log.Printf("[Copilot] Stream attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
			lastErr = err
			copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
			go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
			if headersSent {
				onChunk(fmt.Sprintf("\n[Error: %v]", err))
				break
			}
		}

		if !headersSent {
			if lastErr != nil {
				sendJSON(w, map[string]interface{}{"error": map[string]string{"message": lastErr.Error()}}, http.StatusInternalServerError)
			} else {
				sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "all copilot accounts exhausted"}}, http.StatusServiceUnavailable)
			}
			return
		}

		stopChunk := map[string]interface{}{
			"id":      cid,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "stop",
				},
			},
		}
		data, _ := json.Marshal(stopChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

		stats.UpdateStats(int64((len(prompt) + len(fullReply)) / 4))
		return
	}

	var reply string
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var acc *copilot.CopilotAccount
		var err error
		if poolRole == copilot.PoolTypeSession && copilotSess != nil {
			if attempt == 0 && copilotSess.AccountID != "" {
				if a, aErr := copilot.GetCopilotAccountByID(copilotSess.AccountID); aErr == nil {
					acc = a
					log.Printf("[CopilotSticky] Session %s routed to bound account %s (Turn #%d)\n",
						copilotSess.SessionID, acc.ID, copilotSess.TurnCount+1)
				} else {
					log.Printf("[CopilotSticky] Bound account %s in cooldown/expired: %v, reallocating\n",
						copilotSess.AccountID, aErr)
				}
			}
			if acc == nil {
				acc, err = copilot.GetNextHealthyAccountForType(copilot.PoolTypeSession)
				if err == nil && acc != nil {
					copilot.GlobalCopilotSessions.BindAccount(copilotSess.SessionID, acc.ID)
				}
			}
		} else {
			acc, err = copilot.GetNextHealthyAccountForType(copilot.PoolTypeWorker)
		}

		if err != nil || acc == nil {
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("no healthy copilot accounts available")
			}
			break
		}

		reply, err = copilot.GenerateChatWithSession(acc, copilotSess, prompt, modelName, false, imageDataURI, nil)
		if err == nil {
			copilot.MarkAccountHealthy(acc.ID)
			lastErr = nil
			break
		}
		log.Printf("[Copilot] Attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
		lastErr = err
		copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
		go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
	}

	if lastErr != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": lastErr.Error()}}, http.StatusInternalServerError)
		return
	}

	if strings.Contains(reply, "](/v1/files/") {
		reply = strings.ReplaceAll(reply, "](/v1/files/", fmt.Sprintf("](%s/v1/files/", baseURL))
	}

	sendJSON(w, map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": reply,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     len(strings.Fields(prompt)),
			"completion_tokens": len(strings.Fields(reply)),
			"total_tokens":      len(strings.Fields(prompt)) + len(strings.Fields(reply)),
		},
	}, http.StatusOK)

	stats.UpdateStats(int64((len(prompt) + len(reply)) / 4))
}

func HandleCopilotStatus(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	sendJSON(w, copilot.GetTokenInfo(), http.StatusOK)
}

type AnthropicMessageRequest struct {
	Model     string             `json:"model"`
	Messages  []AnthropicMessage `json:"messages"`
	System    interface{}        `json:"system,omitempty"`
	Stream    bool               `json:"stream"`
	MaxTokens int                `json:"max_tokens"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

func HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logRequest(r)
	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "authentication_error", "message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	release, ok := acquireRequestSlot(r.Context(), requestQueueTimeout)
	if !ok {
		sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "rate_limit_error", "message": "Server busy, request timed out waiting in queue"}}, http.StatusServiceUnavailable)
		return
	}
	defer release()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req AnthropicMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": err.Error()}}, http.StatusBadRequest)
		return
	}

	var chatReq ChatRequest
	chatReq.Model = req.Model
	chatReq.Stream = req.Stream

	if req.System != nil {
		sysText := ""
		if s, ok := req.System.(string); ok {
			sysText = s
		} else if arr, ok := req.System.([]interface{}); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if t, ok := m["text"].(string); ok {
						sysText += t + "\n"
					}
				}
			}
		}
		if sysText != "" {
			chatReq.Messages = append(chatReq.Messages, client.Message{
				Role:    "system",
				Content: strings.TrimSpace(sysText),
			})
		}
	}

	for _, m := range req.Messages {
		text := ""
		if s, ok := m.Content.(string); ok {
			text = s
		} else if arr, ok := m.Content.([]interface{}); ok {
			for _, item := range arr {
				if block, ok := item.(map[string]interface{}); ok {
					if bType, ok := block["type"].(string); ok && bType == "text" {
						if t, ok := block["text"].(string); ok {
							text += t + "\n"
						}
					}
				}
			}
		}
		chatReq.Messages = append(chatReq.Messages, client.Message{
			Role:    m.Role,
			Content: strings.TrimSpace(text),
		})
	}

	modelName, routeMode := config.ResolveModelRouting(req.Model)
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	if routeMode == "copilot" || copilot.IsCopilotModel(modelName) {
		prompt, parsedImages := client.MessagesToPrompt(chatReq.Messages, nil, nil)
		if prompt == "" && len(chatReq.Messages) > 0 {
			prompt = fmt.Sprintf("%v", chatReq.Messages[len(chatReq.Messages)-1].Content)
		}
		var imageDataURI string
		if len(parsedImages) > 0 {
			b64 := base64.StdEncoding.EncodeToString(parsedImages[0].Data)
			mime := parsedImages[0].Mime
			if mime == "" {
				mime = "image/png"
			}
			imageDataURI = fmt.Sprintf("data:%s;base64,%s", mime, b64)
		}

			maxAttempts := copilot.GetHealthyAccountCount()
			if maxAttempts < 1 {
				maxAttempts = 1
			}
			if maxAttempts > 3 {
				maxAttempts = 3
			}

			if req.Stream {
				flusher, ok := w.(http.Flusher)
				if !ok {
					sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": "Streaming unsupported"}}, http.StatusInternalServerError)
					return
				}

				totalOutputTokens := 0
				var fullReply string
				var lastErr error
				headersSent := false

				ensureHeaders := func() {
					if !headersSent {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Cache-Control", "no-cache")
						w.Header().Set("Connection", "keep-alive")
						w.Header().Set("Access-Control-Allow-Origin", "*")
						w.WriteHeader(http.StatusOK)
						flusher.Flush()

						startData, _ := json.Marshal(map[string]interface{}{
							"type": "message_start",
							"message": map[string]interface{}{
								"id":            msgID,
								"type":          "message",
								"role":          "assistant",
								"content":       []interface{}{},
								"model":         req.Model,
								"stop_reason":   nil,
								"stop_sequence": nil,
								"usage":         map[string]int{"input_tokens": len(strings.Fields(prompt)), "output_tokens": 1},
							},
						})
						fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", startData)

						blockStart, _ := json.Marshal(map[string]interface{}{
							"type":          "content_block_start",
							"index":         0,
							"content_block": map[string]string{"type": "text", "text": ""},
						})
						fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", blockStart)
						flusher.Flush()
						headersSent = true
					}
				}

				onChunk := func(delta string) {
					ensureHeaders()
					totalOutputTokens += len(strings.Fields(delta))
					chunkData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]string{"type": "text_delta", "text": delta},
					})
					fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", chunkData)
					flusher.Flush()
				}

				for attempt := 0; attempt < maxAttempts; attempt++ {
					acc, err := copilot.GetNextHealthyAccount()
					if err != nil {
						lastErr = err
						break
					}
					fullReply, err = copilot.GenerateChatWithAccount(acc, prompt, modelName, true, imageDataURI, onChunk)
					if err == nil {
						copilot.MarkAccountHealthy(acc.ID)
						lastErr = nil
						break
					}
					log.Printf("[Anthropic-Copilot] Stream attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
					lastErr = err
					copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
					go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
					if headersSent {
						onChunk(fmt.Sprintf("\n[Error: %v]", err))
						break
					}
				}

				if !headersSent {
					if lastErr != nil {
						sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": lastErr.Error()}}, http.StatusInternalServerError)
					} else {
						sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": "all copilot accounts exhausted"}}, http.StatusServiceUnavailable)
					}
					return
				}

				blockStop, _ := json.Marshal(map[string]interface{}{
					"type":  "content_block_stop",
					"index": 0,
				})
				fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", blockStop)

				msgDelta, _ := json.Marshal(map[string]interface{}{
					"type": "message_delta",
					"delta": map[string]interface{}{
						"stop_reason":   "end_turn",
						"stop_sequence": nil,
					},
					"usage": map[string]int{"output_tokens": totalOutputTokens},
				})
				fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", msgDelta)

				fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				flusher.Flush()

				stats.UpdateStats(int64((len(prompt) + len(fullReply)) / 4))
				return
			}

			var fullReply string
			var lastErr error
			for attempt := 0; attempt < maxAttempts; attempt++ {
				acc, err := copilot.GetNextHealthyAccount()
				if err != nil {
					lastErr = err
					break
				}
				fullReply, err = copilot.GenerateChatWithAccount(acc, prompt, modelName, false, imageDataURI, nil)
				if err == nil {
					copilot.MarkAccountHealthy(acc.ID)
					lastErr = nil
					break
				}
				log.Printf("[Anthropic-Copilot] Attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
				lastErr = err
				copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
				go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
			}

			if lastErr != nil {
				sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": lastErr.Error()}}, http.StatusInternalServerError)
				return
			}
		sendJSON(w, map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []map[string]string{{"type": "text", "text": fullReply}},
			"model":         req.Model,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  len(strings.Fields(prompt)),
				"output_tokens": len(strings.Fields(fullReply)),
			},
		}, http.StatusOK)
		stats.UpdateStats(int64((len(prompt) + len(fullReply)) / 4))
		return
	}

	prompt, parsedImages := client.MessagesToPrompt(chatReq.Messages, nil, nil)
	mCfg, ok := config.GetModelCfg(modelName)
	if !ok {
		mCfg, _ = config.GetModelCfg("gemini-3.8-flash")
	}
	cookieStr, sapisid, _, _ := client.GetNextHealthyCookie()
	if cookieStr == "" {
		sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": "No healthy accounts available"}}, http.StatusServiceUnavailable)
		return
	}

	var fileRefs []client.UploadedFile
	if len(parsedImages) > 0 {
		for idx, img := range parsedImages {
			fileName := fmt.Sprintf("image_%d%s", idx+1, client.ExtensionForMimeType(img.Mime))
			ref, err := client.UploadImage(img.Data, fileName, img.Mime, cookieStr)
			if err == nil {
				fileRefs = append(fileRefs, ref)
			}
		}
	}

	raw, _, err := client.GenerateTextRaw(prompt, mCfg.Mode, mCfg.Think, fileRefs, cookieStr, sapisid)
	if err != nil {
		sendJSON(w, map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": err.Error()}}, http.StatusInternalServerError)
		return
	}
	reply := raw

	sendJSON(w, map[string]interface{}{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       []map[string]string{{"type": "text", "text": reply}},
		"model":         req.Model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  len(strings.Fields(prompt)),
			"output_tokens": len(strings.Fields(reply)),
		},
	}, http.StatusOK)
	stats.UpdateStats(int64((len(prompt) + len(reply)) / 4))
}

func HandleCopilotAddAccount(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Method not allowed"}}, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	acc, err := copilot.AddAccount(req.ID)
	if err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	sendJSON(w, map[string]interface{}{"success": true, "account": acc, "info": copilot.GetTokenInfo()}, http.StatusOK)
}

func HandleCopilotSaveToken(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	var req struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	if err := copilot.SaveTokenWithID(req.ID, req.URL); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	sendJSON(w, map[string]interface{}{"success": true, "info": copilot.GetTokenInfo()}, http.StatusOK)
}

func HandleCopilotDelete(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Method not allowed"}}, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	if err := copilot.DeleteAccount(req.ID); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	sendJSON(w, map[string]interface{}{"success": true, "info": copilot.GetTokenInfo()}, http.StatusOK)
}

func HandleCopilotAutoLogin(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	var req struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := copilot.StartCopilotLoginWithProfile(req.ID, req.Force); err != nil {
		sendJSON(w, map[string]interface{}{"success": false, "message": err.Error()}, http.StatusOK)
		return
	}
	targetID := req.ID
	if targetID == "" {
		targetID = "Copilot-1"
	}
	sendJSON(w, map[string]interface{}{"success": true, "message": fmt.Sprintf("Đã mở Chrome đăng nhập cho %s trên màn hình", targetID)}, http.StatusOK)
}

func HandleCopilotStopAutoLogin(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	copilot.StopCopilotAutoLogin()
	sendJSON(w, map[string]interface{}{"success": true, "message": "Đã đóng Chrome đăng nhập và giải phóng tài nguyên"}, http.StatusOK)
}

func HandleCopilotRefresh(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	go copilot.TriggerAutoRefreshHeadlessForAccount(req.ID)
	sendJSON(w, map[string]interface{}{"success": true, "message": fmt.Sprintf("Triggered background token refresh for %s", req.ID)}, http.StatusOK)
}
