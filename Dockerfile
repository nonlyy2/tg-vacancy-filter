# syntax=docker/dockerfile:1.6

# ---- build stage ------------------------------------------------------------
FROM golang:1.22-alpine AS build

WORKDIR /src

# Leverage layer cache: only re-download deps if go.mod/go.sum changed.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped binary.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/bot .

# ---- runtime stage ----------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=build /out/bot /app/bot

# The session file will be written next to the binary by default.
# Mount a persistent volume to /app if you want the session to survive restarts.
USER app

ENTRYPOINT ["/app/bot"]
