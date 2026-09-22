# Stage 1: Build the Go application
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod ./
# If go.sum exists, copy it (though go.mod says no dependencies other than stdlib, which is true)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o geminigo ./cmd/geminigo/main.go

# Stage 2: Final runner image with Chromium and Node.js for Playwright automation
FROM alpine:latest
RUN apk add --no-cache ca-certificates chromium nodejs npm
WORKDIR /app
COPY --from=builder /app/geminigo .
COPY package*.json ./
RUN npm install --omit=dev || true
COPY playwright_media.js ./
EXPOSE 8081
ENTRYPOINT ["./geminigo", "--port", "8081"]
