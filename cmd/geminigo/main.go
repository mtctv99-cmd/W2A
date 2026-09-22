package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"geminigo/internal/browser"
	"geminigo/internal/client"
	"geminigo/internal/config"
	"geminigo/internal/copilot"
	"geminigo/internal/server"
)

func main() {
	portFlag := flag.Int("port", 0, "Port to listen on (overrides config)")
	configFlag := flag.String("config", "config.json", "Path to config file")
	flag.Parse()

	// 1. Setup Logging MultiWriter
	_ = os.MkdirAll("data", 0755)
	logFile, err := os.OpenFile("data/geminigo.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()

	mw := io.MultiWriter(os.Stderr, logFile)
	log.SetOutput(mw)

	log.Println("==================================================")
	log.Println("Starting GeminiGo...")

	// 2. Load configuration
	config.LoadConfig(*configFlag)

	// 3. Override config port if CLI flag given
	if *portFlag > 0 {
		config.CONFIG.Port = *portFlag
	}

	// 4. Load cookies pool
	client.LoadPool()

	// 4.1 Init Background Tasks
	server.InitServer()

	// 4.2 Graceful Shutdown Handler
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Println("[Shutdown] Stopping server and cleaning up browser processes...")
		browser.CleanupZombies()
		os.Exit(0)
	}()

	// 5. Register HTTP Handlers
	http.HandleFunc("/", server.ServeDashboard)
	http.HandleFunc("/static/style.css", server.ServeDashboard)
	http.HandleFunc("/static/app.js", server.ServeDashboard)

	// Admin API Route Groups
	http.HandleFunc("/admin/api/stats", server.HandleStats)
	http.HandleFunc("/admin/api/cookies", server.HandleCookies)
	http.HandleFunc("/admin/api/pool/status", server.HandlePoolStatus)
	http.HandleFunc("/admin/api/cookies/save", server.HandleSaveCookie)
	http.HandleFunc("/admin/api/cookies/google-key", server.HandleSaveAccountGoogleApiKey)
	http.HandleFunc("/admin/api/cookies/auto-login", server.HandleAutoLogin)
		http.HandleFunc("/admin/api/cookies/refresh", server.HandleRefreshAccounts)
		http.HandleFunc("/admin/api/cookies/delete", server.HandleDeleteCookie)
		http.HandleFunc("/admin/api/cookies/pool-type", server.HandleSetCookiePoolType)
		http.HandleFunc("/admin/api/copilot/pool-type", server.HandleSetCopilotPoolType)
		http.HandleFunc("/admin/api/sessions", server.HandleListSessions)
	http.HandleFunc("/admin/api/apikeys", server.HandleApiKeys)
	http.HandleFunc("/admin/api/apikeys/add", server.HandleAddApiKey)
	http.HandleFunc("/admin/api/apikeys/delete", server.HandleDeleteApiKey)
	http.HandleFunc("/admin/api/auth/status", server.HandleAuthStatus)
	http.HandleFunc("/admin/api/auth/toggle", server.HandleAuthToggle)
	http.HandleFunc("/admin/api/models", server.HandleAdminModels)
	http.HandleFunc("/admin/api/models/sync", server.HandleSyncModels)
	http.HandleFunc("/admin/api/logs", server.HandleLogs)
	http.HandleFunc("/admin/api/logs/clear", server.HandleClearLogs)
		http.HandleFunc("/admin/api/copilot/status", server.HandleCopilotStatus)
		http.HandleFunc("/admin/api/copilot/add", server.HandleCopilotAddAccount)
		http.HandleFunc("/admin/api/copilot/token", server.HandleCopilotSaveToken)
		http.HandleFunc("/admin/api/copilot/auto-login", server.HandleCopilotAutoLogin)
		http.HandleFunc("/admin/api/copilot/auto-login/stop", server.HandleCopilotStopAutoLogin)
		http.HandleFunc("/admin/api/copilot/refresh", server.HandleCopilotRefresh)
		http.HandleFunc("/admin/api/copilot/delete", server.HandleCopilotDelete)

	// Background Daemons
	go copilot.StartAutoRefreshDaemon(context.Background())

		// OpenAI & Anthropic Compatible API Route Groups
		http.HandleFunc("/v1/models", server.HandleModels)
		http.HandleFunc("/v1/chat/completions", server.HandleChatCompletions)
		http.HandleFunc("/v1/messages", server.HandleAnthropicMessages)
		http.HandleFunc("/v1/images/generations", server.HandleImageGenerations)
	http.HandleFunc("/v1/videos/generations", server.HandleVideoGenerations)
	http.HandleFunc("/v1/video/generations", server.HandleVideoGenerations)
	http.HandleFunc("/v1/videos/tasks/", server.HandleVideoTaskStatus)
	http.HandleFunc("/v1/files/", server.HandleFiles)

	// 6. Start Listener
	addr := fmt.Sprintf("%s:%d", config.CONFIG.Host, config.CONFIG.Port)
	log.Printf("geminigo listening on http://%s\n", addr)
	log.Printf("Base URL: http://localhost:%d/v1\n", config.CONFIG.Port)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server stopped with error: %v", err)
	}
}
