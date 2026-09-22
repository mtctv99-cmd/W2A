package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	pushIDPattern  = regexp.MustCompile(`"qKIAYe":"([^"]+)"`)
	pctxPattern    = regexp.MustCompile(`"Ylro7b":"([^"]+)"`)
	xsrfPattern    = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	altXsrfPattern = regexp.MustCompile(`"thykhd":"([^"]+)"`)
	blPattern      = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	fsidPattern    = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
)

type TokenEntry struct {
	PushID    string
	Pctx      string
	XsrfToken string
	IsAuth    bool // true if SNlM0e was found (authenticated session)
	BL        string
	FSID      string
	Ts        time.Time
}

type tokenCall struct {
	wg     sync.WaitGroup
	pushID string
	pctx   string
	xsrf   string
	isAuth bool
	bl     string
	fsid   string
}

type TokenCache struct {
	mu       sync.Mutex
	Caches   map[string]TokenEntry
	inflight map[string]*tokenCall
}

var tokenCache TokenCache

func GetPageTokens(cookieStr string) (string, string, string) {
	pushID, pctx, xsrf, _, _, _ := GetFullPageTokensWithAuth(cookieStr)
	return pushID, pctx, xsrf
}

func GetFullPageTokens(cookieStr string) (string, string, string, string, string) {
	pushID, pctx, xsrf, _, bl, fsid := GetFullPageTokensWithAuth(cookieStr)
	return pushID, pctx, xsrf, bl, fsid
}

func IsAuthenticated(cookieStr string) bool {
	tokenCache.mu.Lock()
	if entry, found := tokenCache.Caches[cookieStr]; found && entry.XsrfToken != "" {
		isAuth := entry.IsAuth
		tokenCache.mu.Unlock()
		return isAuth
	}
	tokenCache.mu.Unlock()
	return strings.Contains(cookieStr, "__Secure-1PSID=") && strings.Contains(cookieStr, "SAPISID=")
}

func GetFullPageTokensWithAuth(cookieStr string) (string, string, string, bool, string, string) {
	tokenCache.mu.Lock()
	if tokenCache.Caches == nil {
		tokenCache.Caches = make(map[string]TokenEntry)
	}
	if tokenCache.inflight == nil {
		tokenCache.inflight = make(map[string]*tokenCall)
	}
	entry, found := tokenCache.Caches[cookieStr]
	if found && time.Since(entry.Ts) < 10*time.Minute && entry.XsrfToken != "" {
		tokenCache.mu.Unlock()
		return entry.PushID, entry.Pctx, entry.XsrfToken, entry.IsAuth, entry.BL, entry.FSID
	}

	// Singleflight: if a fetch is already in progress for this cookie, wait for it
	if call, ok := tokenCache.inflight[cookieStr]; ok {
		tokenCache.mu.Unlock()
		call.wg.Wait()
		return call.pushID, call.pctx, call.xsrf, call.isAuth, call.bl, call.fsid
	}

	call := new(tokenCall)
	call.wg.Add(1)
	tokenCache.inflight[cookieStr] = call
	tokenCache.mu.Unlock()

	client := GetHTTPClient()
	req, err := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	if err != nil {
		tokenCache.mu.Lock()
		delete(tokenCache.inflight, cookieStr)
		tokenCache.mu.Unlock()
		call.pushID, call.pctx, call.bl = "feeds/mcudyrk2a4khkz", "CgcSBWjK7pYx", "boq_assistant-bard-web-server_20260907.07_p3"
		call.wg.Done()
		return call.pushID, call.pctx, "", false, call.bl, ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if cookieStr != "" {
		req.Header.Set("Cookie", cookieStr)
	}

	resp, err := client.Do(req)
	if err != nil {
		tokenCache.mu.Lock()
		delete(tokenCache.inflight, cookieStr)
		tokenCache.mu.Unlock()
		call.pushID, call.pctx, call.bl = "feeds/mcudyrk2a4khkz", "CgcSBWjK7pYx", "boq_assistant-bard-web-server_20260907.07_p3"
		call.wg.Done()
		return call.pushID, call.pctx, "", false, call.bl, ""
	}
	defer resp.Body.Close()

	if len(resp.Cookies()) > 0 {
		merged := MergeCookieString(cookieStr, resp.Cookies())
		if merged != cookieStr {
			UpdateCookieInPool(cookieStr, merged)
		}
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	pushID := "feeds/mcudyrk2a4khkz"
	pctx := "CgcSBWjK7pYx"
	xsrf := ""
	isAuth := false
	bl := "boq_assistant-bard-web-server_20260907.07_p3"
	fsid := ""

	if m := pushIDPattern.FindStringSubmatch(body); len(m) > 1 {
		pushID = m[1]
	}
	if m := pctxPattern.FindStringSubmatch(body); len(m) > 1 {
		pctx = m[1]
	}
	if m := xsrfPattern.FindStringSubmatch(body); len(m) > 1 {
		xsrf = m[1]
		isAuth = true
	}
	if xsrf == "" {
		if m := altXsrfPattern.FindStringSubmatch(body); len(m) > 1 {
			xsrf = m[1]
		}
	}
	if m := blPattern.FindStringSubmatch(body); len(m) > 1 {
		bl = m[1]
	}
	if m := fsidPattern.FindStringSubmatch(body); len(m) > 1 {
		fsid = m[1]
	}

	log.Printf("[DEBUG] getPageTokens: pushID='%s', isAuth=%v, xsrf='%s', bl='%s', fsid='%s'\n", pushID, isAuth, xsrf, bl, fsid)

	tokenCache.mu.Lock()
	tokenCache.Caches[cookieStr] = TokenEntry{
		PushID:    pushID,
		Pctx:      pctx,
		XsrfToken: xsrf,
		IsAuth:    isAuth,
		BL:        bl,
		FSID:      fsid,
		Ts:        time.Now(),
	}
	delete(tokenCache.inflight, cookieStr)
	call.pushID = pushID
	call.pctx = pctx
	call.xsrf = xsrf
	call.isAuth = isAuth
	call.bl = bl
	call.fsid = fsid
	tokenCache.mu.Unlock()
	call.wg.Done()

	return pushID, pctx, xsrf, isAuth, bl, fsid
}

type UploadedFile struct {
	ID   string
	Name string
}

func UploadImage(imageBytes []byte, filename, mimeType string, cookieStr string) (UploadedFile, error) {
	cStr, sapisid := GetNextCookie()
	if cookieStr != "" {
		cStr = cookieStr
		sapisid = ExtractSapisid(cookieStr)
	}
	pushID, _, xsrf, _, bl, fsid := GetFullPageTokensWithAuth(cStr)
	if pushID == "" {
		pushID = "feeds/mcudyrk2a4khkz"
	}

	client := GetHTTPClient()

	// Step 1: Initialize resumable upload with Google Scotty
	startReq, err := http.NewRequest("POST", "https://push.clients6.google.com/upload/", strings.NewReader(fmt.Sprintf("File name: %s", filename)))
	if err != nil {
		return UploadedFile{}, fmt.Errorf("create start request: %w", err)
	}
	startReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprintf("%d", len(imageBytes)))
	startReq.Header.Set("Push-ID", pushID)
	startReq.Header.Set("X-Tenant-Id", "bard-storage")
	startReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	startReq.Header.Set("Origin", "https://gemini.google.com")
	startReq.Header.Set("Referer", "https://gemini.google.com/")
	if cStr != "" {
		startReq.Header.Set("Cookie", cStr)
	}
	if sapisid != "" {
		startReq.Header.Set("Authorization", MakeSapisidHash(sapisid))
	}

	startResp, err := client.Do(startReq)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("start upload error: %w", err)
	}
	defer startResp.Body.Close()

	if startResp.StatusCode != 200 {
		respB, _ := io.ReadAll(startResp.Body)
		return UploadedFile{}, fmt.Errorf("start upload failed status %d: %s", startResp.StatusCode, string(respB))
	}

	uploadURL := startResp.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		if uploadID := startResp.Header.Get("X-GUploader-UploadID"); uploadID != "" {
			uploadURL = fmt.Sprintf("https://push.clients6.google.com/upload/?upload_id=%s&upload_protocol=resumable", uploadID)
		}
	}
	if uploadURL == "" {
		return UploadedFile{}, fmt.Errorf("no upload url received from Scotty")
	}

	// Step 2: Upload raw image bytes to uploadURL
	upReq, err := http.NewRequest("POST", uploadURL, bytes.NewReader(imageBytes))
	if err != nil {
		return UploadedFile{}, fmt.Errorf("create byte upload request: %w", err)
	}
	upReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	upReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	upReq.Header.Set("X-Goog-Upload-Offset", "0")
	upReq.Header.Set("Push-ID", pushID)
	upReq.Header.Set("X-Tenant-Id", "bard-storage")
	upReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	upReq.Header.Set("Origin", "https://gemini.google.com")
	upReq.Header.Set("Referer", "https://gemini.google.com/")
	if cStr != "" {
		upReq.Header.Set("Cookie", cStr)
	}
	if sapisid != "" {
		upReq.Header.Set("Authorization", MakeSapisidHash(sapisid))
	}

	upResp, err := client.Do(upReq)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("upload bytes error: %w", err)
	}
	defer upResp.Body.Close()

	upBody, err := io.ReadAll(upResp.Body)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("read upload response: %w", err)
	}

	if upResp.StatusCode != 200 {
		return UploadedFile{}, fmt.Errorf("upload bytes failed status %d: %s", upResp.StatusCode, string(upBody))
	}

	fileID := strings.TrimSpace(string(upBody))
	if fileID == "" || !strings.HasPrefix(fileID, "/contrib_service/") {
		return UploadedFile{}, fmt.Errorf("invalid file ID from upload: %s", fileID)
	}

	log.Printf("[Vision] Scotty uploaded file ID: %s\n", fileID)

	// Step 3: Register file via ProcessFile RPC to bind attachment to Gemini session
	if mimeType == "" {
		mimeType = "image/png"
	}
	if bl == "" {
		bl = "boq_assistant-bard-web-server_20260901.17_p0"
	}

	innerReq := []interface{}{
		[]interface{}{
			[]interface{}{fileID, nil, 1, mimeType},
			filename,
			nil, nil, nil, nil, nil, nil,
			[]interface{}{0},
		},
		nil,
		1,
		[]interface{}{"en"},
	}
	innerJSON, _ := json.Marshal(innerReq)
	outerReq := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outerReq)

	vals := url.Values{}
	if xsrf != "" {
		vals.Set("at", xsrf)
	}
	vals.Set("f.req", string(outerJSON))

	procURL := fmt.Sprintf("https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/ProcessFile?bl=%s&f.sid=%s&hl=en&_reqid=%d&rt=c",
		url.QueryEscape(bl), url.QueryEscape(fsid), time.Now().UnixNano()%1000000)

	procReq, err := http.NewRequest("POST", procURL, strings.NewReader(vals.Encode()))
	if err == nil {
		procReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
		procReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		procReq.Header.Set("Origin", "https://gemini.google.com")
		procReq.Header.Set("Referer", "https://gemini.google.com/")
		if cStr != "" {
			procReq.Header.Set("Cookie", cStr)
		}
		if sapisid != "" {
			procReq.Header.Set("Authorization", MakeSapisidHash(sapisid))
		}

		procResp, procErr := client.Do(procReq)
		if procErr == nil {
			procResp.Body.Close()
			log.Printf("[Vision] ProcessFile RPC status: %d\n", procResp.StatusCode)
		} else {
			log.Printf("[Vision WARN] ProcessFile RPC failed: %v\n", procErr)
		}
	}

	return UploadedFile{ID: fileID, Name: filename}, nil
}

func FetchImageBytes(url string) ([]byte, error) {
	client := GetHTTPClient()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func ExtensionForMimeType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}

func MimeByExtension(name string) string {
	ext := filepath.Ext(name)
	m := mime.TypeByExtension(ext)
	if m == "" {
		return "application/octet-stream"
	}
	return m
}

func UpdateXsrfToken(token string, cookieStr string) {
	tokenCache.mu.Lock()
	defer tokenCache.mu.Unlock()
	if tokenCache.Caches == nil {
		tokenCache.Caches = make(map[string]TokenEntry)
	}
	entry := tokenCache.Caches[cookieStr]
	entry.XsrfToken = token
	entry.Ts = time.Now()
	tokenCache.Caches[cookieStr] = entry
}
