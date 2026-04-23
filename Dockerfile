# syntax=docker/dockerfile:1.7

# ── Stage 1: build frontend ────────────────────────────────────────────
FROM node:20-alpine AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ── Stage 2: build backend ─────────────────────────────────────────────
FROM golang:1.25-alpine AS backend-builder
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# Pure-Go SQLite means we can produce a static binary with no cgo.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/music-advisor ./cmd/server
# Pre-create the data directory owned by distroless nonroot (UID 65532) so
# the final image works out-of-the-box with a fresh anonymous volume.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ── Stage 3: distroless runtime ────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=backend-builder /out/music-advisor /app/music-advisor
COPY --from=frontend-builder /app/frontend/dist /app/frontend/dist
COPY --from=backend-builder --chown=65532:65532 /out/data /data

USER nonroot:nonroot
ENV MA_ADDRESS=":8080"
ENV MA_DATABASE_PATH="/data/music-advisor.db"
ENV MA_FRONTEND_PATH="/app/frontend/dist"
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/app/music-advisor"]
