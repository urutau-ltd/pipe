# Multi-stage build
FROM docker.io/library/golang:1.26-bookworm AS builder

ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /pipe .

# Docker CLI from official image (avoids distro lagging packages).
FROM docker.io/library/docker:cli AS dockercli

# ─────────────────────────────────────────────────────────
FROM docker.io/library/almalinux:9-minimal

ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE=https://nest.urutau-ltd.org/pipe
ARG CREATED=unknown

LABEL org.opencontainers.image.title="pipe" \
      org.opencontainers.image.description="lightweight CI runner for soft-serve and local machines" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.created="${CREATED}"

# git is required for clone/pull in server mode.
# almalinux:9-minimal already includes curl-minimal; installing curl conflicts.
RUN microdnf install -y \
    bash \
    git \
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
