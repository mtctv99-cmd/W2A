package browser

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GenerateGuestChat invokes Gemini Web Flash-Lite in guest mode (no Google account needed).
func GenerateGuestChat(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("empty prompt")
	}

	execCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "node", "playwright_guest.js", prompt)
	outBytes, err := cmd.CombinedOutput()
	outStr := string(outBytes)

	if err != nil {
		return "", fmt.Errorf("guest chat error (%v): %s", err, outStr)
	}

	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "__TEXT__=") {
			return strings.TrimPrefix(line, "__TEXT__="), nil
		}
	}

	return "", fmt.Errorf("no output from guest model runner: %s", outStr)
}
