#!/bin/bash
set -e

cd "$(dirname "$0")"

echo "=== GeminiGo — Run on Host ==="

# Stop Docker container if running
if docker ps --format '{{.Names}}' | grep -q '^geminigo$'; then
    echo "[1/4] Stopping Docker container..."
    docker compose down 2>/dev/null || true
fi

# Build binary via Docker
echo "[2/4] Building binary..."
docker build -t geminigo-build . 2>&1 | tail -3

# Copy binary out
echo "[3/4] Extracting binary..."
docker create --name geminigo-tmp geminigo-build 2>/dev/null
docker cp geminigo-tmp:/app/geminigo ./geminigo
docker rm geminigo-tmp >/dev/null 2>&1
chmod +x ./geminigo

# Run on host
echo "[4/4] Starting GeminiGo on host..."
echo "  Dashboard: http://localhost:8081"
echo "  API Base:  http://localhost:8081/v1"
echo "  Press Ctrl+C to stop"
echo ""

exec ./geminigo --config config.json
