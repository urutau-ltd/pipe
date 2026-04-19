# Multi-stage build — final image is ~15MB
FROM docker.io/library/golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /pipe .

# ─────────────────────────────────────────────────────────
FROM docker.io/library/alpine:latest

# git is required for clone/pull in server mode
RUN apk add --no-cache git openssh-client

COPY --from=builder /pipe /usr/local/bin/pipe

ENTRYPOINT ["pipe"]
CMD ["server"]
