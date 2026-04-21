# Multi-stage build
FROM docker.io/library/golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /pipe .

# Docker CLI from official image (avoids distro lagging packages).
FROM docker.io/library/docker:cli AS dockercli

# ─────────────────────────────────────────────────────────
FROM docker.io/library/almalinux:9-minimal

# git is required for clone/pull in server mode
RUN microdnf install -y \
    bash \
    git \
    curl \
    openssh-clients \
    ca-certificates \
    shadow-utils \
    && microdnf clean all

RUN useradd --system --no-create-home --shell /usr/sbin/nologin pipe

RUN mkdir -p /tmp/pipe && chown pipe:pipe /tmp/pipe

COPY --from=builder /pipe /usr/local/bin/pipe
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker

USER pipe

ENTRYPOINT ["pipe"]
CMD ["server"]
