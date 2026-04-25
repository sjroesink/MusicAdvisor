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
# Pure-Go pgx driver — no cgo needed; produces a static binary.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/music-advisor ./cmd/server

# ── Stage 3: distroless runtime ────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=backend-builder /out/music-advisor /app/music-advisor
COPY --from=frontend-builder /app/frontend/dist /app/frontend/dist

USER nonroot:nonroot
ENV MA_ADDRESS=":8080"
ENV MA_FRONTEND_PATH="/app/frontend/dist"
EXPOSE 8080

ENTRYPOINT ["/app/music-advisor"]
