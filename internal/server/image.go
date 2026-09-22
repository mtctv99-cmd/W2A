package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"geminigo/internal/browser"
	"geminigo/internal/client"
	"geminigo/internal/config"
	"geminigo/internal/copilot"
)

type ImageGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

func HandleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logRequest(r)

	release, ok := acquireRequestSlot(r.Context(), requestQueueTimeout)
	if !ok {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Server busy, request timed out waiting in queue"}}, http.StatusServiceUnavailable)
		return
	}
	defer release()
	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Method not allowed"}}, http.StatusMethodNotAllowed)
		return
	}

	var req ImageGenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}

	if req.Prompt == "" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Missing prompt"}}, http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "gemini-imagen"
	}
	if req.N <= 0 {
		req.N = 1
	}
	if req.N > 4 {
		req.N = 4
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "b64_json"
	}

	prompt := req.Prompt
	lowerPrompt := strings.ToLower(req.Prompt)
	if !strings.HasPrefix(lowerPrompt, "generate") && !strings.HasPrefix(lowerPrompt, "create") && !strings.HasPrefix(lowerPrompt, "draw") && !strings.HasPrefix(lowerPrompt, "vẽ") && !strings.HasPrefix(lowerPrompt, "tạo") {
		prompt = fmt.Sprintf("Generate an image of %s", req.Prompt)
	}

		rawModel := strings.ToLower(strings.TrimSpace(req.Model))
		modelName := config.ResolveModelAlias(req.Model)
		if strings.HasPrefix(modelName, "copilot") || strings.HasPrefix(modelName, "dall-e") || strings.Contains(rawModel, "copilot") || strings.HasPrefix(rawModel, "dall-e") || strings.HasPrefix(rawModel, "dalle") {
			log.Printf("[ImageGen] Routing image request to Copilot DALL-E 3 (model: %s, prompt: %s)\n", req.Model, prompt)

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
				acc, err := copilot.GetNextHealthyAccount()
				if err != nil {
					lastErr = err
					break
				}
				dataURLs, err = copilot.GenerateImageWithAccount(acc, prompt)
				if err == nil && len(dataURLs) > 0 {
					copilot.MarkAccountHealthy(acc.ID)
					lastErr = nil
					break
				}
				log.Printf("[ImageGen] Copilot attempt %d/%d with account %s failed: %v\n", attempt+1, maxAttempts, acc.ID, err)
				lastErr = err
				copilot.MarkAccountCooldown(acc.ID, 30*time.Second)
				go copilot.TriggerAutoRefreshHeadlessForAccount(acc.ID)
			}

			if lastErr != nil || len(dataURLs) == 0 {
				errMsg := "all copilot accounts exhausted or returned no images"
				if lastErr != nil {
					errMsg = lastErr.Error()
				}
				sendJSON(w, map[string]interface{}{"error": map[string]string{"message": fmt.Sprintf("Copilot image generation failed: %s", errMsg)}}, http.StatusInternalServerError)
				return
			}

			var images []map[string]interface{}
			for _, u := range dataURLs {
				if req.ResponseFormat == "url" {
					localName, saveErr := saveBase64ImageLocally(u, r)
					if saveErr == nil && localName != "" {
						images = append(images, map[string]interface{}{"url": localName})
					} else {
						images = append(images, map[string]interface{}{"url": u})
					}
				} else {
					rawB64 := u
					if idx := strings.Index(u, "base64,"); idx != -1 {
						rawB64 = u[idx+7:]
					}
					images = append(images, map[string]interface{}{"b64_json": rawB64})
				}
			}
			resp := map[string]interface{}{
				"created": time.Now().Unix(),
				"data":    images,
			}
			sendJSON(w, resp, http.StatusOK)
			return
		}

	mCfg, ok := config.GetModelCfg(modelName)
	if !ok {
		mCfg, _ = config.GetModelCfg("gemini-imagen")
	}

	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("Session-ID")
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("anon_%d", time.Now().UnixNano())
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cookieStr, sapisid, accountID, _ := client.GetNextHealthyCookie()
		if cookieStr == "" {
			log.Printf("[ImageGen] No healthy accounts (attempt %d/3)\n", attempt+1)
			lastErr = fmt.Errorf("no healthy accounts available")
			break
		}
		log.Printf("[ImageGen] Attempt %d/3 (Session: %s, Acc: %s)\n", attempt+1, sessionID, accountID)

		var imageURLs []string
		// 1. Fast Path: Try Google Gemini HTTP RPC first (takes ~3s, returns clean original 2048x2048 CDN URLs)
		parsed, rawBody, err := client.GenerateTextRaw(prompt, mCfg.Mode, 0, nil, cookieStr, sapisid)
		if err == nil {
			imageURLs = extractImageURLs(parsed, rawBody)
		}

		// 2. Fallback: If HTTP RPC failed or returned no image URLs, try Playwright browser context
		if len(imageURLs) == 0 {
			log.Printf("[ImageGen] HTTP RPC returned no image URLs (err: %v), triggering Playwright browser fallback...\n", err)
			pwURLs, chatURL, pwErr := browser.StartCDPMediaGenerationWithSession(prompt, sessionID, "image")
			if pwErr == nil && pwURLs != "" {
				log.Printf("[ImageGen] Successfully retrieved Playwright Image URLs: %s (ChatURL: %s)\n", pwURLs, chatURL)
				imageURLs = strings.Split(pwURLs, "|||")
			} else {
				log.Printf("[ImageGen] Playwright image generation also failed: %v\n", pwErr)
				lastErr = err
				if lastErr == nil {
					lastErr = pwErr
				}
				if geminiErr, ok := lastErr.(*client.GeminiError); ok {
					client.MarkCooldown(accountID, client.GetCooldownDuration(geminiErr.Type, attempt+1))
				} else {
					client.MarkCooldown(accountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
				}
				continue
			}
		}

		client.MarkHealthy(accountID)

		if len(imageURLs) == 0 {
			log.Printf("[ImageGen] No images generated on %s, trying next attempt\n", accountID)
			lastErr = fmt.Errorf("no images generated")
			continue
		}

		var images []map[string]interface{}
		for _, url := range imageURLs {
			url = strings.TrimSpace(url)
			if url == "" || strings.Contains(url, "/image_generation_content/") {
				continue
			}

			// Check if URL is already a Data URL (base64) from Playwright
			if strings.HasPrefix(url, "data:image/") {
				if req.ResponseFormat == "url" {
					localName, saveErr := saveBase64ImageLocally(url, r)
					if saveErr == nil && localName != "" {
						images = append(images, map[string]interface{}{"url": localName})
					} else {
						images = append(images, map[string]interface{}{"url": url})
					}
				} else {
					rawB64 := url
					if idx := strings.Index(url, "base64,"); idx != -1 {
						rawB64 = url[idx+7:]
					}
					images = append(images, map[string]interface{}{"b64_json": rawB64})
				}
				continue
			}

			// Ensure high-resolution =s2048 query suffix on Google CDN URLs
			if strings.Contains(url, "googleusercontent.com") {
				reS := regexp.MustCompile(`=s\d+`)
				if reS.MatchString(url) {
					url = reS.ReplaceAllString(url, "=s2048")
				} else if !strings.Contains(url, "=s") {
					url += "=s2048"
				}
			}

			log.Printf("[ImageGen] Fetching high-resolution image asset from: %s\n", url)
			b64, mimeType := fetchImageAsBase64(url, cookieStr, sapisid)
			if b64 != "" {
				if req.ResponseFormat == "url" {
					dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)
					localName, saveErr := saveBase64ImageLocally(dataURI, r)
					if saveErr == nil && localName != "" {
						images = append(images, map[string]interface{}{"url": localName})
					} else {
						images = append(images, map[string]interface{}{"url": url})
					}
				} else {
					images = append(images, map[string]interface{}{"b64_json": b64})
				}
			} else {
				log.Printf("[ImageGen] Direct fetch failed for %s, falling back to Playwright capture...\n", url)
				pwURLs, _, pwErr := browser.StartCDPMediaGenerationWithSession(prompt, sessionID, "image")
				if pwErr == nil && pwURLs != "" {
					for _, pu := range strings.Split(pwURLs, "|||") {
						if strings.HasPrefix(pu, "data:image/") {
							if req.ResponseFormat == "url" {
								localName, saveErr := saveBase64ImageLocally(pu, r)
								if saveErr == nil && localName != "" {
									images = append(images, map[string]interface{}{"url": localName})
								} else {
									images = append(images, map[string]interface{}{"url": pu})
								}
							} else {
								rawB64 := pu
								if idx := strings.Index(pu, "base64,"); idx != -1 {
									rawB64 = pu[idx+7:]
								}
								images = append(images, map[string]interface{}{"b64_json": rawB64})
							}
						}
					}
				}
			}
		}

		if len(images) == 0 {
			log.Printf("[ImageGen] Failed to fetch generated images for %s, retrying attempt\n", accountID)
			lastErr = fmt.Errorf("failed to fetch generated images")
			continue
		}

		resp := map[string]interface{}{
			"created": time.Now().Unix(),
			"data":    images,
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

var imageURLRegex = regexp.MustCompile(`https?://[a-zA-Z0-9.-]*googleusercontent\.com/[^\s"'<>,\\]*(?:=s\d+)?`)

func extractImageURLs(parsedText, rawBody string) []string {
	var urls []string
	seen := make(map[string]bool)

	// Try deep JSON parse on raw response first
	for _, line := range strings.Split(rawBody, "\n") {
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		var arr []interface{}
		if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
			continue
		}
		subArr, ok := arr[0].([]interface{})
		if !ok || len(subArr) < 3 {
			continue
		}
		innerStr, ok := subArr[2].(string)
		if !ok {
			continue
		}
		var inner []interface{}
		if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
			continue
		}

		// Extract generated images at inner[4][0][12][7][0][index][0][3][3]
		if len(inner) > 4 {
			if candidates, ok := inner[4].([]interface{}); ok && len(candidates) > 0 {
				if c0, ok := candidates[0].([]interface{}); ok {
					// Generated images at [12][7]
					if len(c0) > 12 {
						if media, ok := c0[12].([]interface{}); ok && len(media) > 7 {
							if mediaGroups, ok := media[7].([]interface{}); ok && len(mediaGroups) > 0 {
								if genList, ok := mediaGroups[0].([]interface{}); ok {
									for _, item := range genList {
										if imgArr, ok := item.([]interface{}); ok && len(imgArr) > 0 {
											if img0, ok := imgArr[0].([]interface{}); ok && len(img0) > 3 {
												if meta, ok := img0[3].([]interface{}); ok && len(meta) > 3 {
													if u, ok := meta[3].(string); ok && u != "" && !seen[u] {
														seen[u] = true
														if !strings.Contains(u, "=s") {
															u += "=s2048"
														}
														urls = append(urls, u)
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

		// Fallback: regex on both parsed text and raw body
		if len(urls) == 0 {
			for _, src := range []string{rawBody, parsedText} {
				matches := imageURLRegex.FindAllString(src, -1)
				for _, u := range matches {
					u = strings.TrimRight(u, ".,;)\"'")
					if strings.Contains(u, "image_generation_content") {
						continue
					}
					if !seen[u] {
						seen[u] = true
						if !strings.Contains(u, "=s") {
							u += "=s2048"
						}
						urls = append(urls, u)
					}
				}
			}
		}

	return urls
}

var (
	noRedirectClient     *http.Client
	noRedirectClientOnce sync.Once
)

func getNoRedirectClient() *http.Client {
	noRedirectClientOnce.Do(func() {
		tr := &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     30 * time.Second,
		}
		noRedirectClient = &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
	return noRedirectClient
}

func fetchImageAsBase64(url, cookieStr, sapisid string) (string, string) {
	client := getNoRedirectClient()

	for redirect := 0; redirect < 5; redirect++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return "", ""
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		req.Header.Set("Referer", "https://gemini.google.com/")
		if cookieStr != "" {
			req.Header.Set("Cookie", cookieStr)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[ImageGen] Fetch failed: %v", err)
			return "", ""
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", ""
			}
			url = loc
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		if resp.StatusCode != 200 || !strings.HasPrefix(contentType, "image/") {
			log.Printf("[ImageGen] Fetch status %d, type=%s", resp.StatusCode, contentType)
			resp.Body.Close()
			return "", ""
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(data) == 0 {
			return "", ""
		}

		mimeType := strings.Split(contentType, ";")[0]
		if mimeType == "" {
			mimeType = "image/png"
		}

		return base64.StdEncoding.EncodeToString(data), mimeType
	}

	return "", ""
}

type VideoGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Async          bool   `json:"async"`
}

type VideoTask struct {
	mu        sync.Mutex
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"` // "pending", "processing", "completed", "failed"
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	videoTasks   = make(map[string]*VideoTask)
	videoTasksMu sync.RWMutex
)

func saveTasksToDiskLocked() {
	_ = os.MkdirAll("data", 0755)
	file := "data/tasks.json"
	data, err := json.MarshalIndent(videoTasks, "", "  ")
	if err == nil {
		_ = os.WriteFile(file, data, 0644)
	}
}

func loadTasksFromDisk() {
	videoTasksMu.Lock()
	defer videoTasksMu.Unlock()
	file := "data/tasks.json"
	data, err := os.ReadFile(file)
	if err == nil {
		var loaded map[string]*VideoTask
		if err := json.Unmarshal(data, &loaded); err == nil {
			now := time.Now()
			for id, t := range loaded {
				if t.Status == "processing" || t.Status == "pending" {
					t.Status = "failed"
					t.Error = "Server restarted before task could complete"
				}
				if now.Sub(t.CreatedAt) > 24*time.Hour {
					delete(loaded, id)
				}
			}
			videoTasks = loaded
			saveTasksToDiskLocked()
		}
	}
}

func SweepOldVideoTasks(maxAge time.Duration) {
	videoTasksMu.Lock()
	defer videoTasksMu.Unlock()
	now := time.Now()
	changed := false
	for id, t := range videoTasks {
		if now.Sub(t.CreatedAt) > maxAge {
			delete(videoTasks, id)
			changed = true
		}
	}
	if changed {
		saveTasksToDiskLocked()
	}
}

func setVideoTask(task *VideoTask) {
	videoTasksMu.Lock()
	defer videoTasksMu.Unlock()
	videoTasks[task.TaskID] = task
	saveTasksToDiskLocked()
}

func getVideoTask(taskID string) *VideoTask {
	videoTasksMu.RLock()
	defer videoTasksMu.RUnlock()
	return videoTasks[taskID]
}

func removeVideoTask(taskID string) {
	videoTasksMu.Lock()
	defer videoTasksMu.Unlock()
	delete(videoTasks, taskID)
	saveTasksToDiskLocked()
}

// getSessionCookie looks up the session's bound account cookie for downloading media.
// Session must already exist (created by StartCDPMediaGenerationWithSession).
func getSessionCookie(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sess, err := browser.GlobalSessionPool.GetOrCreateSession(sessionID)
	if err != nil {
		return ""
	}
	acc, ok := client.GetAccountByID(sess.AccountID)
	if !ok {
		return ""
	}
	return acc.Cookie
}

func HandleVideoGenerations(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	logRequest(r)

	release, ok := acquireRequestSlot(r.Context(), requestQueueTimeout)
	if !ok {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Server busy, request timed out waiting in queue"}}, http.StatusServiceUnavailable)
		return
	}
	defer release()
	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	if r.Method != "POST" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Method not allowed"}}, http.StatusMethodNotAllowed)
		return
	}

	var req VideoGenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": err.Error()}}, http.StatusBadRequest)
		return
	}

		if req.Prompt == "" {
			sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Missing prompt"}}, http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			req.Model = "gemini-veo"
		}
		req.Model = config.ResolveModelAlias(req.Model)

		prompt := req.Prompt
		lowerPrompt := strings.ToLower(req.Prompt)
		if !strings.HasPrefix(lowerPrompt, "generate") && !strings.HasPrefix(lowerPrompt, "create") && !strings.HasPrefix(lowerPrompt, "video") {
			prompt = fmt.Sprintf("Generate a video of %s", req.Prompt)
		}

	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("Session-ID")
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("anon_%d", time.Now().UnixNano())
	}

	// Check if Async / Task Queue mode is requested
	isAsync := req.Async || r.URL.Query().Get("async") == "true" || r.Header.Get("X-Async") == "true"

	if isAsync {
		taskID := fmt.Sprintf("task_%s", newRandID())
		task := &VideoTask{
			TaskID:    taskID,
			SessionID: sessionID,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		setVideoTask(task)

		// Dispatch background generation task
		host := r.Host
		isTLS := r.TLS != nil
		go func(tID, pStr, sessID, hStr string, tlsBool bool) {
			taskObj := getVideoTask(tID)
			if taskObj == nil {
				return
			}
			taskObj.mu.Lock()
			taskObj.Status = "processing"
			taskObj.mu.Unlock()

			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				taskObj = getVideoTask(tID)
				if taskObj == nil {
					lastErr = fmt.Errorf("task removed")
					break
				}

				cdpURL, chatURL, cdpErr := browser.StartCDPMediaGenerationWithSession(pStr, sessID, "video")
				if cdpErr != nil || cdpURL == "" {
					lastErr = cdpErr
					if sess, sessErr := browser.GlobalSessionPool.GetOrCreateSession(sessID); sessErr == nil {
						client.MarkCooldown(sess.AccountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
					}
					errStr := ""
					if cdpErr != nil {
						errStr = strings.ToLower(cdpErr.Error())
					}
					if strings.Contains(errStr, "limit") || strings.Contains(errStr, "quota") || strings.Contains(errStr, "declined") {
						log.Printf("[VideoTask %s] Account limit/quota hit (%v), stopping retry loop.\n", tID, cdpErr)
						break
					}
					continue
				}

				sess, sessErr := browser.GlobalSessionPool.GetOrCreateSession(sessID)
				if sessErr == nil {
					client.MarkHealthy(sess.AccountID)
				}
				log.Printf("[VideoTask %s] Successfully generated Video URL: %s (ChatURL: %s)\n", tID, cdpURL, chatURL)

				// Use session-bound account cookie for download, not round-robin
				cookieStr := getSessionCookie(sessID)

				finalVideoURL := cdpURL
				var saveErr error
				if strings.HasPrefix(cdpURL, "data:") {
					rawB64 := cdpURL
					if idx := strings.Index(cdpURL, "base64,"); idx != -1 {
						rawB64 = cdpURL[idx+7:]
					}
					videoData, b64Err := base64.StdEncoding.DecodeString(rawB64)
					if b64Err != nil || len(videoData) == 0 {
						log.Printf("[VideoTask %s] base64 decode failed (%v), retrying\n", tID, b64Err)
						lastErr = fmt.Errorf("base64 decode: %w", b64Err)
						continue
					}
					_ = os.MkdirAll("data/files", 0755)
					filename := fmt.Sprintf("video_%s.mp4", newRandID())
					filePath := filepath.Join("data/files", filename)
					if err := os.WriteFile(filePath, videoData, 0644); err == nil {
						scheme := "http"
						if tlsBool {
							scheme = "https"
						}
						finalVideoURL = fmt.Sprintf("%s://%s/v1/files/%s", scheme, hStr, filename)
						log.Printf("[VideoTask %s] Saved Base64 video (%d bytes) to: %s\n", tID, len(videoData), filePath)
					} else {
						saveErr = fmt.Errorf("failed writing video file: %w", err)
					}
				} else {
					localFilename, err := SaveRemoteMediaLocally(cdpURL, cookieStr, "video", ".mp4")
					if err == nil && localFilename != "" {
						scheme := "http"
						if tlsBool {
							scheme = "https"
						}
						finalVideoURL = fmt.Sprintf("%s://%s/v1/files/%s", scheme, hStr, localFilename)
					} else {
						saveErr = fmt.Errorf("failed saving remote video: %w", err)
					}
				}

				if saveErr != nil {
					log.Printf("[VideoTask %s] Save error (%v), retrying attempt %d\n", tID, saveErr, attempt+1)
					lastErr = saveErr
					continue
				}

				taskObj.mu.Lock()
				taskObj.Status = "completed"
				taskObj.URL = finalVideoURL
				taskObj.mu.Unlock()

				videoTasksMu.Lock()
				saveTasksToDiskLocked()
				videoTasksMu.Unlock()
				return
			}

			taskObj = getVideoTask(tID)
			if taskObj != nil {
				taskObj.mu.Lock()
				taskObj.Status = "failed"
				if lastErr != nil {
					taskObj.Error = lastErr.Error()
				} else {
					taskObj.Error = "generation failed"
				}
				taskObj.mu.Unlock()

				videoTasksMu.Lock()
				saveTasksToDiskLocked()
				videoTasksMu.Unlock()
			}
		}(taskID, prompt, sessionID, host, isTLS)

		// Immediate response with task_id
		sendJSON(w, map[string]interface{}{
			"status":  "pending",
			"task_id": taskID,
		}, http.StatusOK)
		return
	}

	// Synchronous Mode (Default)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cdpURL, chatURL, cdpErr := browser.StartCDPMediaGenerationWithSession(prompt, sessionID, "video")
		if cdpErr != nil || cdpURL == "" {
			log.Printf("[VideoGen] Playwright video generation failed (%v)\n", cdpErr)
			lastErr = cdpErr
			if sess, sessErr := browser.GlobalSessionPool.GetOrCreateSession(sessionID); sessErr == nil {
				client.MarkCooldown(sess.AccountID, client.GetCooldownDuration("SERVER_ERROR", attempt+1))
			}
			continue
		}

		sess, sessErr := browser.GlobalSessionPool.GetOrCreateSession(sessionID)
		if sessErr == nil {
			client.MarkHealthy(sess.AccountID)
		}
		log.Printf("[VideoGen] Successfully generated Video URL: %s (ChatURL: %s)\n", cdpURL, chatURL)

		// Use session-bound account cookie for download, not round-robin
		cookieStr := getSessionCookie(sessionID)

		finalVideoURL := cdpURL
		var saveErr error
		if strings.HasPrefix(cdpURL, "data:") {
			rawB64 := cdpURL
			if idx := strings.Index(cdpURL, "base64,"); idx != -1 {
				rawB64 = cdpURL[idx+7:]
			}
			videoData, b64Err := base64.StdEncoding.DecodeString(rawB64)
			if b64Err == nil && len(videoData) > 0 {
				_ = os.MkdirAll("data/files", 0755)
				filename := fmt.Sprintf("video_%s.mp4", newRandID())
				filePath := filepath.Join("data/files", filename)
					if err := os.WriteFile(filePath, videoData, 0644); err == nil {
						scheme := getRequestScheme(r)
						finalVideoURL = fmt.Sprintf("%s://%s/v1/files/%s", scheme, r.Host, filename)
						log.Printf("[VideoGen] Saved Base64 video (%d bytes) to public URL: %s\n", len(videoData), finalVideoURL)
					} else {
						saveErr = fmt.Errorf("failed writing video file: %w", err)
					}
				} else {
					saveErr = fmt.Errorf("bad base64 decode: %v", b64Err)
				}
			} else {
				localFilename, err := SaveRemoteMediaLocally(cdpURL, cookieStr, "video", ".mp4")
				if err == nil && localFilename != "" {
					scheme := getRequestScheme(r)
					finalVideoURL = fmt.Sprintf("%s://%s/v1/files/%s", scheme, r.Host, localFilename)
					log.Printf("[VideoGen] Hosted public video URL: %s\n", finalVideoURL)
			} else {
				saveErr = fmt.Errorf("failed saving remote video: %w", err)
			}
		}

		if saveErr != nil {
			log.Printf("[VideoGen] Save error (%v), retrying attempt %d\n", saveErr, attempt+1)
			lastErr = saveErr
			continue
		}

		resp := map[string]interface{}{
			"created": time.Now().Unix(),
			"data": []map[string]interface{}{
				{
					"url":            finalVideoURL,
					"revised_prompt": prompt,
				},
			},
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

func HandleVideoTaskStatus(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}

	if !authorized(r) {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Unauthorized"}}, http.StatusUnauthorized)
		return
	}

	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("Session-ID")
	}

	taskID := strings.TrimPrefix(r.URL.Path, "/v1/videos/tasks/")
	taskID = strings.TrimPrefix(taskID, "/")
	if taskID == "" {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Missing task_id"}}, http.StatusBadRequest)
		return
	}

	task := getVideoTask(taskID)
	if task == nil {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Task not found"}}, http.StatusNotFound)
		return
	}

	if sessionID != "" && task.SessionID != "" && task.SessionID != sessionID {
		sendJSON(w, map[string]interface{}{"error": map[string]string{"message": "Task not found"}}, http.StatusNotFound)
		return
	}

	task.mu.Lock()
	status := task.Status
	url := task.URL
	errStr := task.Error
	task.mu.Unlock()

	if status == "completed" {
		sendJSON(w, map[string]interface{}{
			"task_id": task.TaskID,
			"status":  "completed",
			"url":     url,
		}, http.StatusOK)
		return
	}

	if status == "failed" {
		sendJSON(w, map[string]interface{}{
			"task_id": task.TaskID,
			"status":  "failed",
			"error":   errStr,
		}, http.StatusOK)
		return
	}

	sendJSON(w, map[string]interface{}{
		"task_id": task.TaskID,
		"status":  status,
	}, http.StatusOK)
}

func SaveRemoteMediaLocally(remoteURL, cookieStr, filePrefix, ext string) (string, error) {
	_ = os.MkdirAll("data/files", 0755)

	filename := fmt.Sprintf("%s_%s%s", filePrefix, newRandID(), ext)
	filePath := filepath.Join("data/files", filename)

	clientObj := &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	curURL := remoteURL
	for hop := 0; hop < 6; hop++ {
		req, err := http.NewRequest("GET", curURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		req.Header.Set("Referer", "https://gemini.google.com/")
		if cookieStr != "" {
			req.Header.Set("Cookie", cookieStr)
		}

		resp, err := clientObj.Do(req)
		if err != nil {
			return "", err
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", fmt.Errorf("redirect with empty Location header")
			}
			curURL = loc
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			return "", fmt.Errorf("http status %d downloading remote media", resp.StatusCode)
		}

		outFile, err := os.Create(filePath)
		if err != nil {
			resp.Body.Close()
			return "", err
		}
		_, err = io.Copy(outFile, resp.Body)
		resp.Body.Close()
		outFile.Close()
		if err != nil {
			_ = os.Remove(filePath)
			return "", err
		}

		return filename, nil
	}

	return "", fmt.Errorf("too many redirects downloading remote media")
}

func HandleFiles(w http.ResponseWriter, r *http.Request) {
	if setupCORS(w, r) {
		return
	}
	filename := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	filename = strings.TrimPrefix(filename, "/")
	if filename == "" {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	cleanPath := filepath.Clean(filepath.Join("data/files", filename))
	absBase, err1 := filepath.Abs("data/files")
	absTarget, err2 := filepath.Abs(cleanPath)
	if err1 != nil || err2 != nil || !strings.HasPrefix(absTarget, absBase) {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	if _, err := os.Stat(absTarget); os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(filename, ".mp4") {
		w.Header().Set("Content-Type", "video/mp4")
	} else if strings.HasSuffix(filename, ".jpg") || strings.HasSuffix(filename, ".jpeg") {
		w.Header().Set("Content-Type", "image/jpeg")
	} else if strings.HasSuffix(filename, ".png") {
		w.Header().Set("Content-Type", "image/png")
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, absTarget)
}

func getRequestScheme(r *http.Request) string {
	if r == nil {
		return "http"
	}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on") {
		return "https"
	}
	return "http"
}

func saveBase64ImageLocally(dataURI string, r *http.Request) (string, error) {
	_ = os.MkdirAll("data/files", 0755)
	rawB64 := dataURI
	if idx := strings.Index(dataURI, "base64,"); idx != -1 {
		rawB64 = dataURI[idx+7:]
	}
	imgData, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil || len(imgData) == 0 {
		return "", fmt.Errorf("bad base64: %w", err)
	}
	ext := ".jpg"
	if strings.Contains(dataURI, "image/png") {
		ext = ".png"
	} else if strings.Contains(dataURI, "image/webp") {
		ext = ".webp"
	}
	filename := fmt.Sprintf("img_%s%s", newRandID(), ext)
	filePath := filepath.Join("data/files", filename)
	if err := os.WriteFile(filePath, imgData, 0644); err != nil {
		return "", err
	}
	scheme := getRequestScheme(r)
	publicURL := fmt.Sprintf("%s://%s/v1/files/%s", scheme, r.Host, filename)
	return publicURL, nil
}

func newRandID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
