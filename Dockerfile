# Multi-stage build — final image is ~15MB
FROM docker.io/library/golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /pipe .

# ─────────────────────────────────────────────────────────
FROM docker.io/library/debian:bookworm-slim

# git is required for clone/pull in server mode
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    git \
    curl \
    openssh-client \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --system --no-create-home --shell /usr/sbin/nologin pipe

RUN mkdir -p /tmp/pipe && chown pipe:pipe /tmp/pipe

COPY --from=builder /pipe /usr/local/bin/pipe

USER pipe

ENTRYPOINT ["pipe"]
CMD ["server"]
