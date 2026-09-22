package browser

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
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
	"time"

	"geminigo/internal/client"
	"geminigo/internal/config"
)

var browserPaths = []string{
	// Windows
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	// macOS
	`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
	`/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge`,
	// Linux
	`/usr/bin/google-chrome`,
	`/usr/bin/microsoft-edge`,
	`/usr/bin/chromium`,
}

func findBrowser() string {
	for _, path := range browserPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	// Fallback to exec.LookPath
	for _, name := range []string{"google-chrome", "chrome", "msedge", "edge", "chromium"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func wsConnect(wsURL string) (net.Conn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	secKey := base64.StdEncoding.EncodeToString(keyBytes)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, secKey)

	_, err = conn.Write([]byte(req))
	if err != nil {
		conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 101 {
		conn.Close()
		return nil, fmt.Errorf("handshake failed: %d", resp.StatusCode)
	}
	return conn, nil
}

func wsWriteText(conn net.Conn, payload string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	data := []byte(payload)
	length := len(data)
	var header []byte
	header = append(header, 0x81) // Fin + Text frame
	mask := []byte{0x01, 0x02, 0x03, 0x04}

	if length <= 125 {
		header = append(header, byte(length|0x80))
	} else if length <= 65535 {
		header = append(header, 126|0x80)
		header = append(header, byte(length>>8), byte(length&0xFF))
	} else {
		return fmt.Errorf("payload too large")
	}

	header = append(header, mask...)
	masked := make([]byte, length)
	for i := 0; i < length; i++ {
		masked[i] = data[i] ^ mask[i%4]
	}
	_, err := conn.Write(append(header, masked...))
	return err
}

func wsReadText(conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		buf := make([]byte, 2)
		_, err := io.ReadFull(conn, buf)
		if err != nil {
			return "", err
		}
		opcode := buf[0] & 0x0F

		if opcode == 8 {
			return "", io.EOF
		}
		if opcode == 9 {
			pongHeader := []byte{0x8A, 0x00}
			_, _ = conn.Write(pongHeader)
			continue
		}
		if opcode == 0xA {
			continue
		}
		if opcode != 1 {
			return "", fmt.Errorf("unexpected opcode %d", opcode)
		}

		masked := (buf[1] & 0x80) != 0
		length := int(buf[1] & 0x7F)

		if length == 126 {
			lenBuf := make([]byte, 2)
			_, _ = io.ReadFull(conn, lenBuf)
			length = int(lenBuf[0])<<8 | int(lenBuf[1])
		} else if length == 127 {
			lenBuf := make([]byte, 8)
			_, _ = io.ReadFull(conn, lenBuf)
			length = int(lenBuf[4])<<24 | int(lenBuf[5])<<16 | int(lenBuf[6])<<8 | int(lenBuf[7])
		}

		var mask []byte
		if masked {
			mask = make([]byte, 4)
			_, _ = io.ReadFull(conn, mask)
		}

		payload := make([]byte, length)
		_, err = io.ReadFull(conn, payload)
		if err != nil {
			return "", err
		}

		if masked {
			for i := 0; i < length; i++ {
				payload[i] = payload[i] ^ mask[i%4]
			}
		}
		return string(payload), nil
	}
}

func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

type TabInfo struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	URL                  string `json:"url"`
}

var autoLoginRunning atomic.Bool

func CleanupZombies() {
	absProfilesDir, err := filepath.Abs("data/profiles")
	if err != nil {
		absProfilesDir = "data/profiles"
	}

	// Targeted query: Match only Chrome/Node processes with --user-data-dir pointing to geminigo profiles
	cmdStr := fmt.Sprintf(`ps aux | grep "%s" | grep -E "chrome|node" | grep -v grep | awk '{print $2}'`, absProfilesDir)
	out, err := exec.Command("sh", "-c", cmdStr).Output()
	if err == nil {
		pids := strings.Fields(string(out))
		for _, pidStr := range pids {
			var pid int
			if _, err := fmt.Sscanf(pidStr, "%d", &pid); err == nil && pid > 0 {
				KillPID(pid)
			}
		}
	}

	// Reap any defunct child processes
	ReapZombies()

	// Clean stale profile lock files in data/profiles/
	files, _ := os.ReadDir("data/profiles")
	for _, f := range files {
		if f.IsDir() {
			pDir := filepath.Join("data/profiles", f.Name())
			_ = os.Remove(filepath.Join(pDir, "SingletonLock"))
			_ = os.Remove(filepath.Join(pDir, "SingletonSocket"))
			_ = os.Remove(filepath.Join(pDir, "SingletonCookie"))
		}
	}
}

func StartCDPMediaGeneration(prompt, accountID, cookieStr, profileDir, mediaType string) (string, error) {
	outputStr, err := RunPlaywrightScript(prompt, accountID, mediaType, profileDir, "")
	if err != nil {
		return "", err
	}

	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "__MEDIA_URL__=") {
			return strings.TrimPrefix(line, "__MEDIA_URL__="), nil
		}
	}

	return "", fmt.Errorf("%s URL not found in playwright output: %s", mediaType, outputStr)
}

func IsAutoLoginRunning() bool {
	return autoLoginRunning.Load()
}

func StartChromeAutoLogin(targetAccountID string) {
	StartChromeAutoLoginWithURL(targetAccountID, "https://gemini.google.com/app")
}

func StartChromeAutoLoginWithURL(targetAccountID, targetURL string) {
	if targetURL == "" {
		targetURL = "https://gemini.google.com/app"
	}
	if !autoLoginRunning.CompareAndSwap(false, true) {
		log.Println("[AutoLogin] Một tiến trình Auto-Login đang chạy, vui lòng không yêu cầu thêm.")
		return
	}

	success := false
	defer func() {
		if !success {
			autoLoginRunning.Store(false)
		}
	}()

	browserPath := findBrowser()
	if browserPath == "" {
		log.Println("[AutoLogin] Không tìm thấy Chrome/Edge trên máy.")
		return
	}

	cdpPort, err := findFreePort()
	if err != nil {
		log.Printf("[AutoLogin] Không tìm được port trống: %v\n", err)
		return
	}

	if targetAccountID == "" {
		targetAccountID = client.GenerateNextAccountID()
	}

	profileDir, _ := filepath.Abs(fmt.Sprintf("data/profiles/%s", targetAccountID))
	_ = os.MkdirAll(profileDir, 0755)

	log.Printf("[AutoLogin] Đang khởi chạy trình duyệt cho %s: %s (CDP port %d, profile: %s, url: %s)\n", targetAccountID, browserPath, cdpPort, profileDir, targetURL)

	cmd := exec.Command(browserPath,
		fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		targetURL,
	)
	SetProcessGroup(cmd)

	err = cmd.Start()
	if err != nil {
		log.Printf("[AutoLogin] Lỗi khởi chạy trình duyệt: %v\n", err)
		return
	}

	success = true

	go func() {
		defer func() {
			autoLoginRunning.Store(false)
		}()

		clientHTTP := &http.Client{Timeout: 2 * time.Second}
		debugURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", cdpPort)
		var wsDebuggerURL string

		for i := 0; i < 60; i++ {
			time.Sleep(1 * time.Second)
			resp, err := clientHTTP.Get(debugURL)
			if err != nil {
				continue
			}
			var tabs []TabInfo
			_ = json.NewDecoder(resp.Body).Decode(&tabs)
			resp.Body.Close()

			for _, tab := range tabs {
				if strings.Contains(tab.URL, "google.com") && tab.WebSocketDebuggerURL != "" {
					wsDebuggerURL = tab.WebSocketDebuggerURL
					break
				}
			}
			if wsDebuggerURL != "" {
				break
			}
		}

		if wsDebuggerURL == "" {
			log.Printf("[AutoLogin] Không tìm thấy tab Gemini Debugger qua cổng %d.\n", cdpPort)
			return
		}
		log.Printf("[AutoLogin] Đã kết nối Debugger: %s\n", wsDebuggerURL)

		ws, err := wsConnect(wsDebuggerURL)
		if err != nil {
			log.Printf("[AutoLogin] Lỗi kết nối WebSocket Debugger: %v\n", err)
			return
		}
		defer ws.Close()

		// If account already has saved cookie, inject it into Chrome session via CDP
		if acc, found := client.GetAccountByID(targetAccountID); found && acc.Cookie != "" {
			type CDPCookie struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
				Path   string `json:"path"`
				Secure bool   `json:"secure"`
			}
			var cdpCookies []CDPCookie
			for _, p := range strings.Split(acc.Cookie, ";") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				idx := strings.Index(p, "=")
				if idx > 0 {
					name := strings.TrimSpace(p[:idx])
					value := strings.TrimSpace(p[idx+1:])
					cdpCookies = append(cdpCookies, CDPCookie{
						Name:   name,
						Value:  value,
						Domain: ".google.com",
						Path:   "/",
						Secure: true,
					})
				}
			}
			if len(cdpCookies) > 0 {
				injectPayload, _ := json.Marshal(map[string]interface{}{
					"id":     88888,
					"method": "Network.setCookies",
					"params": map[string]interface{}{
						"cookies": cdpCookies,
					},
				})
				_ = wsWriteText(ws, string(injectPayload))
				log.Printf("[AutoLogin] Đã inject %d cookie từ %s vào Chrome session.\n", len(cdpCookies), targetAccountID)

				_ = wsWriteText(ws, `{"id": 88889, "method": "Page.reload"}`)
			}
		}

			log.Println("[AutoLogin] Browser đang mở. Hãy đăng nhập Google trên trình duyệt. Đang chờ cookie/API key...")

			isAIStudioTarget := strings.Contains(targetURL, "aistudio")
			foundKeyLogged := false

			for {
				time.Sleep(3 * time.Second)
				reqID := int(time.Now().UnixNano() % 100000)
				_ = wsWriteText(ws, fmt.Sprintf(`{"id": %d, "method": "Network.getCookies", "params": {"urls": ["https://gemini.google.com", "https://aistudio.google.com", "https://google.com"]}}`, reqID))

				evalID := reqID + 500000
				if isAIStudioTarget && !foundKeyLogged {
					script := `(() => {
						try {
							// 1. Auto agree to terms of service modal if present
							const tosButtons = Array.from(document.querySelectorAll('button, a')).filter(b => {
								const t = (b.innerText || '').toLowerCase();
								return t.includes('agree') || t.includes('accept') || t.includes('tiếp tục') || t.includes('đồng ý');
							});
							for (const b of tosButtons) {
								if (!b.disabled) b.click();
							}

							// 2. Scan dialog/body for user-created Google API key (exclude Google internal frontend keys)
							const exclude = new Set([
								'AIzaSyDHAQL7kdN6lNBcBok1eNB8dG7wwo6E6io',
								'AIzaSyDdP816MREB3SkjZO04QXbjsigfcI0GWOs'
							]);

							// Look specifically inside modal dialog or key detail containers first
							const dialogText = (document.querySelector('mat-dialog-container, [role="dialog"], ms-api-key-creation-dialog, ms-api-key-detail-dialog') || {}).innerText || '';
							const mDialog = dialogText.match(/(?:AQ\.[A-Za-z0-9_-]{40,60}|AIzaSy[A-Za-z0-9_-]{33})/);
							if (mDialog && !exclude.has(mDialog[0])) {
								return JSON.stringify({status: "found", key: mDialog[0]});
							}

							const bodyText = document.body ? document.body.innerText : '';
							const allMatches = bodyText.match(/(?:AQ\.[A-Za-z0-9_-]{40,60}|AIzaSy[A-Za-z0-9_-]{33})/g) || [];
							for (const k of allMatches) {
								if (!exclude.has(k)) {
									return JSON.stringify({status: "found", key: k});
								}
							}

							// 3. If stuck on "No Cloud Projects Available", navigate to /app/projects to create project
							if (bodyText.includes("No Cloud Projects Available")) {
								const cancelBtn = Array.from(document.querySelectorAll('button')).find(b => {
									const t = (b.innerText || '').toLowerCase();
									return t === 'annuler' || t === 'cancel' || t === 'hủy';
								});
								if (cancelBtn) cancelBtn.click();
								window.location.href = "https://aistudio.google.com/app/projects";
								return JSON.stringify({status: "redirecting_to_projects"});
							}

							// 4. If on /app/projects, handle project creation
							if (window.location.href.includes('/app/projects')) {
								// In dialog: confirm create project
								const confirmProjBtn = document.querySelector('button.ms-button-primary');
								if (confirmProjBtn && (confirmProjBtn.innerText || '').toLowerCase().includes('projet')) {
									confirmProjBtn.click();
									return JSON.stringify({status: "created_project"});
								}
								// Open dialog: create project
								const createProjBtn = Array.from(document.querySelectorAll('button, a')).find(b => {
									const t = (b.innerText || '').toLowerCase();
									return (t.includes('créer') || t.includes('create') || t.includes('tạo')) && t.includes('projet');
								});
								if (createProjBtn && !createProjBtn.disabled) {
									createProjBtn.click();
									return JSON.stringify({status: "clicking_create_project"});
								}
							}

							// 5. Click confirm create key button inside dialog if open
							const confirmKeyBtn = document.querySelector('button.xap-inline-dialog.ms-button-primary, button[data-test-create-api-key-button]');
							if (confirmKeyBtn && !confirmKeyBtn.disabled) {
								confirmKeyBtn.click();
								return JSON.stringify({status: "confirmed_create_key"});
							}

							// 6. Click 'Create API key' on main table/page (multilingual: EN, FR, VI)
							const createBtn = Array.from(document.querySelectorAll('button, a')).find(el => {
								const t = (el.innerText || '').toLowerCase();
								return (t.includes('create') || t.includes('créer') || t.includes('tạo')) && (t.includes('api') || t.includes('clé') || t.includes('key') || t.includes('khóa') || t.includes('khoá'));
							});
							if (createBtn && !createBtn.disabled) {
								createBtn.click();
								return JSON.stringify({status: "clicked_create"});
							}

							// 7. Click key link (...xxxx) to open detail modal if already in table
							const keyLink = document.querySelector('button.key-string-link');
							if (keyLink) {
								keyLink.click();
								return JSON.stringify({status: "clicked_key_link"});
							}
						} catch (e) {}
						return JSON.stringify({status: "waiting"});
					})()`
					evalPayload, _ := json.Marshal(map[string]interface{}{
						"id":     evalID,
						"method": "Runtime.evaluate",
						"params": map[string]interface{}{
							"expression":    script,
							"returnByValue": true,
						},
					})
					_ = wsWriteText(ws, string(evalPayload))
				}

				for {
					raw, err := wsReadText(ws)
					if err != nil {
						log.Printf("[AutoLogin] Mất kết nối WebSocket: %v\n", err)
						return
					}

					var evalResp struct {
						ID     int `json:"id"`
						Result struct {
							Result struct {
								Value string `json:"value"`
							} `json:"result"`
						} `json:"result"`
					}
					if isAIStudioTarget && !foundKeyLogged && json.Unmarshal([]byte(raw), &evalResp) == nil && evalResp.ID == evalID {
						var parsed struct {
							Status string `json:"status"`
							Key    string `json:"key"`
						}
						if json.Unmarshal([]byte(evalResp.Result.Result.Value), &parsed) == nil {
							if parsed.Status == "found" && (strings.HasPrefix(parsed.Key, "AIzaSy") || strings.HasPrefix(parsed.Key, "AQ.")) {
								foundKeyLogged = true
								_ = client.SaveAccountGoogleAPIKey(targetAccountID, parsed.Key)
								log.Printf("[AIStudio] ⭐ ĐÃ TỰ ĐỘNG LẤY GOOGLE API KEY CHO %s: %s\n", targetAccountID, parsed.Key)
								// Tự động đóng trình duyệt sau 1.5 giây
								go func() {
									time.Sleep(1500 * time.Millisecond)
									_ = wsWriteText(ws, `{"id": 99999, "method": "Browser.close"}`)
									log.Printf("[AIStudio] Đã tự động đóng trình duyệt cho %s sau khi hoàn tất lấy key.\n", targetAccountID)
								}()
							}
						}
					}

				var responseObj struct {
					ID     int `json:"id"`
					Result struct {
						Cookies []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"cookies"`
					} `json:"result"`
				}

				if err := json.Unmarshal([]byte(raw), &responseObj); err == nil {
					if responseObj.ID == reqID {
						cookieParts := []string{}
						sapisid := ""
						secure1PSID := ""

						for _, cookie := range responseObj.Result.Cookies {
							cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", cookie.Name, cookie.Value))
							if cookie.Name == "SAPISID" {
								sapisid = cookie.Value
							}
							if cookie.Name == "__Secure-1PSID" {
								secure1PSID = cookie.Value
							}
						}

						if sapisid != "" && secure1PSID != "" {
							cookieStr := strings.Join(cookieParts, "; ")
							relProfileDir := fmt.Sprintf("data/profiles/%s", targetAccountID)
							if err := client.AddAccountCookieWithID(targetAccountID, cookieStr, relProfileDir); err != nil {
								log.Printf("[AutoLogin] Lỗi lưu cookie: %v\n", err)
							} else {
								log.Printf("[AutoLogin] Đã phát hiện cookie hợp lệ cho %s và cập nhật vào cookies.json.\n", targetAccountID)
							}
						}
						break
					}
				}
			}

			// Check if Chrome process has exited naturally
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				log.Printf("[AutoLogin] Trình duyệt cho %s đã được đóng bởi người dùng.\n", targetAccountID)
				break
			}
		}

		// Wait for process to exit if still closing
		if cmd != nil {
			_ = cmd.Wait()
		}

		// Clean up lock files after Chrome exits
		_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))
		log.Printf("[AutoLogin] Hoàn tất phiên làm việc cho %s.\n", targetAccountID)
	}()
}

var (
	refreshMu          sync.Mutex
	refreshingAccounts sync.Map
	isRefreshing       atomic.Bool
)

// IsRefreshing returns whether a headless CDP profile refresh sweep is currently running.
func IsRefreshing() bool {
	return isRefreshing.Load()
}

// RefreshAccountFromProfile launches Chrome in headless mode using the existing profile,
// retrieves updated cookies via CDP, extracts supported models from Gemini Web UI, and saves them.
func RefreshAccountFromProfile(targetAccountID string) (cookieUpdated bool, modelsCount int, err error) {
	if _, loaded := refreshingAccounts.LoadOrStore(targetAccountID, true); loaded {
		log.Printf("[AutoRefresh] %s is already refreshing, skipping duplicate.\n", targetAccountID)
		return false, 0, nil
	}
	defer refreshingAccounts.Delete(targetAccountID)

	refreshMu.Lock()
	defer refreshMu.Unlock()

	browserPath := findBrowser()
	if browserPath == "" {
		return false, 0, fmt.Errorf("chrome/edge browser executable not found")
	}

	cdpPort, err := findFreePort()
	if err != nil {
		return false, 0, fmt.Errorf("failed to get free port: %v", err)
	}

	profileDir, _ := filepath.Abs(fmt.Sprintf("data/profiles/%s", targetAccountID))
	if _, err := os.Stat(profileDir); os.IsNotExist(err) {
		return false, 0, fmt.Errorf("profile directory %s does not exist", profileDir)
	}

	// Clean any previous locks
	_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
	_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
	_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))

	log.Printf("[AutoRefresh] Starting headless Chrome for %s on port %d...\n", targetAccountID, cdpPort)

	cmd := exec.Command(browserPath,
		"--headless=new",
		fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--disable-translate",
		"--mute-audio",
		"--no-zygote",
		"--disk-cache-size=10485760",
		"https://gemini.google.com/app",
	)
	SetProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return false, 0, fmt.Errorf("failed to start headless browser: %v", err)
	}

	defer func() {
		if cmd.Process != nil {
			KillProcessGroup(cmd.Process.Pid)
			_ = cmd.Process.Kill()
			_ = cmd.Wait() // Reaps the child process to prevent zombie/defunct
		}
		ReapZombies()
		_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonSocket"))
		_ = os.Remove(filepath.Join(profileDir, "SingletonCookie"))
	}()

	clientHTTP := &http.Client{Timeout: 2 * time.Second}
	debugURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", cdpPort)
	var wsDebuggerURL string

	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		resp, err := clientHTTP.Get(debugURL)
		if err != nil {
			continue
		}
		var tabs []TabInfo
		_ = json.NewDecoder(resp.Body).Decode(&tabs)
		resp.Body.Close()

		for _, tab := range tabs {
			if strings.Contains(tab.URL, "gemini.google.com") && tab.WebSocketDebuggerURL != "" {
				wsDebuggerURL = tab.WebSocketDebuggerURL
				break
			}
		}
		if wsDebuggerURL != "" {
			break
		}
	}

	if wsDebuggerURL == "" {
		return false, 0, fmt.Errorf("failed to attach to Gemini tab via CDP within 30s")
	}

	ws, err := wsConnect(wsDebuggerURL)
	if err != nil {
		return false, 0, fmt.Errorf("failed to connect to CDP websocket: %v", err)
	}
	defer ws.Close()

	// Wait 2s for initial page scripts to load
	time.Sleep(2 * time.Second)

	// Inject existing cookies for targetAccountID so Chrome is authenticated
	if acc, ok := client.GetAccountByID(targetAccountID); ok && acc.Cookie != "" {
		type CDPCookieParam struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
			Path   string `json:"path"`
			Secure bool   `json:"secure"`
		}
		var cdpCookies []CDPCookieParam
		for _, part := range strings.Split(acc.Cookie, ";") {
			part = strings.TrimSpace(part)
			if idx := strings.Index(part, "="); idx > 0 {
				cdpCookies = append(cdpCookies, CDPCookieParam{
					Name:   part[:idx],
					Value:  part[idx+1:],
					Domain: ".google.com",
					Path:   "/",
					Secure: true,
				})
			}
		}
		if len(cdpCookies) > 0 {
			setPayload, _ := json.Marshal(map[string]interface{}{
				"id":     10000,
				"method": "Network.setCookies",
				"params": map[string]interface{}{
					"cookies": cdpCookies,
				},
			})
			_ = wsWriteText(ws, string(setPayload))
			time.Sleep(300 * time.Millisecond)

			// Reload page to apply newly set cookies
			_ = wsWriteText(ws, `{"id": 10005, "method": "Page.reload"}`)
			time.Sleep(3 * time.Second)
		}
	}

	// 1. Fetch updated Cookies
	reqCookieID := 10001
	_ = wsWriteText(ws, fmt.Sprintf(`{"id": %d, "method": "Network.getCookies", "params": {"urls": ["https://gemini.google.com"]}}`, reqCookieID))

	for i := 0; i < 10; i++ {
		raw, err := wsReadText(ws)
		if err != nil {
			break
		}
		var responseObj struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"cookies"`
			} `json:"result"`
		}

		if err := json.Unmarshal([]byte(raw), &responseObj); err == nil && responseObj.ID == reqCookieID {
			cookieParts := []string{}
			sapisid := ""
			secure1PSID := ""

			for _, c := range responseObj.Result.Cookies {
				cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", c.Name, c.Value))
				if c.Name == "SAPISID" {
					sapisid = c.Value
				}
				if c.Name == "__Secure-1PSID" {
					secure1PSID = c.Value
				}
			}

			if sapisid != "" && secure1PSID != "" {
				cookieStr := strings.Join(cookieParts, "; ")
				relProfileDir := fmt.Sprintf("data/profiles/%s", targetAccountID)
				if client.IsAuthenticated(cookieStr) {
					if err := client.AddAccountCookieWithID(targetAccountID, cookieStr, relProfileDir); err == nil {
						client.MarkHealthy(targetAccountID)
						client.ClearRotateAuthError(targetAccountID)
						cookieUpdated = true
						log.Printf("[AutoRefresh] %s authenticated cookie refreshed successfully.\n", targetAccountID)
					}
				} else {
					log.Printf("[AutoRefresh] %s refreshed cookie is not authenticated (guest session); keeping existing cookie.\n", targetAccountID)
				}
			}
			break
		}
	}

	// 2. Auto-discover Models from Gemini Web
	reqEvalID := 20002
	evalScript := `(async () => {
		try {
			const discovered = [];
			const seen = new Set();

			// 1. Try to open model menu button if present
			const btn = document.querySelector('[data-test-id="bard-mode-menu-button"], button.input-area-switch, button[aria-label*="chọn chế độ"], button[aria-label*="mode"]');
			if (btn) {
				btn.click();
				await new Promise(r => setTimeout(r, 600));
			}

			// 2. Query all menu items inside gem-menu-item or role=menuitem
			const items = document.querySelectorAll('gem-menu-item, [role="menuitem"]');
			items.forEach(el => {
				if (el.classList.contains('mode-picker-sign-in-button') || el.getAttribute('data-test-id') === 'mode-picker-sign-in-button') {
					return;
				}
				const labelEl = el.querySelector('.label');
				const sublabelEl = el.querySelector('.sublabel');
				const label = (labelEl ? labelEl.innerText : el.innerText).trim();
				const sublabel = (sublabelEl ? sublabelEl.innerText : '').trim();
				const modeId = el.getAttribute('data-mode-id') || '';
				const isSelected = el.classList.contains('selected') || el.getAttribute('data-active') === 'true';

				const rawLabel = label.split('\n')[0].trim();
				if (!rawLabel || rawLabel.toLowerCase().includes('đăng nhập') || rawLabel.toLowerCase().includes('sign in')) {
					return;
				}
				if (!seen.has(rawLabel.toLowerCase())) {
					seen.add(rawLabel.toLowerCase());
					discovered.push({
						label: rawLabel,
						sublabel: sublabel || (label.includes('\n') ? label.split('\n').slice(1).join(' ').trim() : ''),
						mode_id: modeId,
						selected: isSelected
					});
				}
			});

			// Close menu if opened
			if (btn) {
				btn.click();
			}

			// 3. Fallback to active model button if menu was not rendered
			if (discovered.length === 0 && btn) {
				const labelEl = btn.querySelector('.logo-pill-label-container, .label');
				const text = (labelEl ? labelEl.innerText : btn.innerText).trim();
				if (text && !seen.has(text.toLowerCase())) {
					discovered.push({
						label: text,
						sublabel: 'Active Web Model',
						mode_id: '',
						selected: true
					});
				}
			}

			return JSON.stringify(discovered);
		} catch (e) {
			return JSON.stringify([]);
		}
	})()`

	evalPayload, _ := json.Marshal(map[string]interface{}{
		"id":     reqEvalID,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    evalScript,
			"returnByValue": true,
			"awaitPromise":  true,
		},
	})
	_ = wsWriteText(ws, string(evalPayload))

	for i := 0; i < 15; i++ {
		raw, err := wsReadText(ws)
		if err != nil {
			break
		}
		var evalResp struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}

		if err := json.Unmarshal([]byte(raw), &evalResp); err == nil && evalResp.ID == reqEvalID {
			var discoveredList []WebDiscoveredModel
			if err := json.Unmarshal([]byte(evalResp.Result.Result.Value), &discoveredList); err == nil && len(discoveredList) > 0 {
				newModels := make(map[string]config.ModelCfg)
				var discoveredNames []string
				for _, item := range discoveredList {
					mName, mode, think, desc := ParseModelFromWeb(item.Label, item.Sublabel)
					if mName != "" {
						newModels[mName] = config.ModelCfg{
							Mode:  mode,
							Think: think,
							Desc:  desc,
						}
						discoveredNames = append(discoveredNames, mName)
						if item.Selected {
							log.Printf("[AutoRefresh] %s active model on Gemini Web: %s (%s)\n", targetAccountID, mName, item.Label)
						}
					}
				}

				if len(newModels) > 0 {
					config.UpdateModels(newModels)
					modelsCount = len(newModels)
					log.Printf("[AutoRefresh] Auto-discovered %d models from Gemini Web for %s: %v\n", modelsCount, targetAccountID, discoveredNames)
				}
			}
			break
		}
	}

	return cookieUpdated, modelsCount, nil
}

// RefreshAllAccounts sweeps all configured accounts that have profile directories and updates them.
func RefreshAllAccounts() (refreshedCount int, totalModels int) {
	if !isRefreshing.CompareAndSwap(false, true) {
		log.Println("[AutoRefresh] Profile refresh sweep already running, skipping overlapping sweep...")
		return 0, 0
	}
	defer isRefreshing.Store(false)

	accs := client.GetAccounts()
	if len(accs) == 0 {
		return 0, 0
	}

	for _, acc := range accs {
		pDir := acc.ProfileDir
		if pDir == "" {
			pDir = fmt.Sprintf("data/profiles/%s", acc.ID)
		}
		if _, err := os.Stat(pDir); err == nil {
			updated, models, err := RefreshAccountFromProfile(acc.ID)
			if err != nil {
				log.Printf("[AutoRefresh] Refresh %s error: %v\n", acc.ID, err)
			} else {
				if updated {
					refreshedCount++
				}
				if models > totalModels {
					totalModels = models
				}
			}
		}
	}
	return refreshedCount, totalModels
}

type WebDiscoveredModel struct {
	Label    string `json:"label"`
	Sublabel string `json:"sublabel"`
	ModeID   string `json:"mode_id"`
	Selected bool   `json:"selected"`
}

func ParseModelFromWeb(rawLabel, sublabel string) (modelName string, mode int, think int, desc string) {
	lower := strings.ToLower(rawLabel)
	desc = fmt.Sprintf("Official %s", rawLabel)
	if sublabel != "" {
		desc += fmt.Sprintf(" - %s", sublabel)
	}

	// Thinking models: "thinking", "tư duy mở rộng", "tư duy", "extended thinking"
	if strings.Contains(lower, "thinking") || strings.Contains(lower, "tư duy") || strings.Contains(lower, "extended") {
		mode = 2
		think = 4
		if strings.Contains(lower, "3.8") {
			return "gemini-3.8-thinking", mode, think, desc
		} else if strings.Contains(lower, "3.7") {
			return "gemini-3.7-thinking", mode, think, desc
		} else if strings.Contains(lower, "3.6") {
			return "gemini-3.6-thinking", mode, think, desc
		} else if strings.Contains(lower, "3.5") {
			return "gemini-3.5-flash-thinking", mode, think, desc
		}
		return "gemini-3.8-thinking", mode, think, desc
	}

	// Flash-Lite
	if strings.Contains(lower, "lite") {
		mode = 6
		think = 4
		if strings.Contains(lower, "3.5") {
			return "gemini-3.5-flash-lite", mode, think, desc
		}
		return "gemini-3.5-flash-lite", mode, think, desc
	}

	// Pro models
	if strings.Contains(lower, "pro") {
		mode = 3
		think = 4
		if strings.Contains(lower, "3.1") {
			return "gemini-3.1-pro", mode, think, desc
		} else if strings.Contains(lower, "3.7") {
			return "gemini-3.7-pro", mode, think, desc
		} else if strings.Contains(lower, "3.8") {
			return "gemini-3.8-pro", mode, think, desc
		}
		return "gemini-3.1-pro", mode, think, desc
	}

	// Flash models
	if strings.Contains(lower, "flash") {
		think = 4
		if strings.Contains(lower, "3.8") {
			mode = 10
			return "gemini-3.8-flash", mode, think, desc
		} else if strings.Contains(lower, "3.7") {
			mode = 10
			return "gemini-3.7-flash", mode, think, desc
		} else if strings.Contains(lower, "3.6") {
			mode = 10
			return "gemini-3.6-flash", mode, think, desc
		} else if strings.Contains(lower, "3.5") {
			mode = 1
			return "gemini-3.5-flash", mode, think, desc
		}
		mode = 10
		return "gemini-3.8-flash", mode, think, desc
	}

	// Fallback for custom labels
	mode = 10
	think = 4
	cleanName := "gemini-" + strings.ReplaceAll(strings.TrimSpace(lower), " ", "-")
	return cleanName, mode, think, desc
}
