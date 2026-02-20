# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads before copying the rest of the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary; strip debug info to reduce image size.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -trimpath -o /socktails ./cmd/proxy

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /socktails /socktails

ENTRYPOINT ["/socktails"]
