package server

import (
	"os"
	"strings"
	"testing"
)

func TestReadLastLines(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "geminigo_log_test_*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	var input []string
	for i := 1; i <= 200; i++ {
		input = append(input, "line "+string(rune('A'+(i%26))))
	}
	tmpFile.WriteString(strings.Join(input, "\n") + "\n")
	tmpFile.Close()

	res := readLastLines(tmpFile.Name(), 5)
	lines := strings.Split(strings.TrimSpace(res), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
}
