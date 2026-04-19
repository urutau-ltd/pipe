BINARY  := pipe
IMAGE   := pipe
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: all build test vet image demo demo-local clean

all: vet test build

## build — compile the binary to dist/pipe
build:
	@mkdir -p dist
	CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY) .
	@echo "==> built dist/$(BINARY) ($(VERSION))"

## test — run unit tests
## -race requires CGO — disabled automatically on musl/Guix
RACE := $(shell go env CGO_ENABLED 2>/dev/null | grep -qx 1 && echo "-race" || echo "")

test:
	go test $(RACE) -count=1 ./...

## vet — static analysis
vet:
	go vet ./...

## image — build the container image with Podman
image:
	podman build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):latest \
		-t $(IMAGE):$(VERSION) \
		.
	@echo "==> image $(IMAGE):$(VERSION)"

## demo — run pipe against itself inside a Podman container
##
## Uses golang:alpine so pipe's own .pipe.yml can call go build/test.
## The repo is mounted read-only; build output goes to a tmpfs.
## No pre-built pipe image required — runs via go run .
demo:
	@echo "==> running pipe against itself inside Podman"
	podman run --rm \
		--name pipe-demo \
		-v "$(CURDIR):/src:ro,Z" \
		-e GOPATH=/tmp/go \
		-e GOCACHE=/tmp/gocache \
		-e HOME=/tmp \
		docker.io/library/golang:1.26-alpine \
		sh -c ' \
			apk add --no-cache git 2>/dev/null; \
			cp -r /src /repo; \
			cd /repo; \
			go mod download; \
			go run . run --branch main \
		'

## demo-local — run pipe against itself on the host (no container)
demo-local:
	go run . run --branch main

## clean — remove build artifacts and images
clean:
	rm -rf dist/
	podman rmi $(IMAGE):latest $(IMAGE):$(VERSION) 2>/dev/null || true
	@echo "==> clean"
