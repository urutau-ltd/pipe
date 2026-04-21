# pipe

<div align="center">
    <img src=".repo-assets/pipe.svg" width="150" alt="pipe logo" />
</div>

Stupidly Light CI runner for
[`soft-serve`](https://github.com/charmbracelet/soft-serve) and local machines.

Single binary. No database. No API keys. PIpeline defined as `.pipe.yml` file at
the root of each repository.

Two commands only. No more:

```shell
$ pipe run           # Run pipeline in current project
$ pipe server        # Receive pushes from soft-serve and run pipelines
```

## Quick Start

Add `.pipe.yml` to any repository:

```yml
name: my-app

steps:
  - name: test
    run: go test ./...

  - name: build
    run: go build -o dist/app .

  - name: deploy
    run: scp dist/app server:/usr/local/bin/app
    branches:
      - main
```

run it locally:

```bash
$ cd my-app
$ pipe run
```

### Parallel steps

Steps marked `parallel: true` that appear consecutively are grouped and executed
concurrently — **but only when `runtime.NumCPU() > 1`**. On a single-core host
(e.g. a Nanode), every step runs sequentially regardless of the flag.

Output from each parallel step is buffered and flushed in declaration order once
the group finishes, so logs are never interleaved.

```yaml
steps:
  - name: lint # ┐
    run: golangci-lint run #  ├── run in parallel on multi-core hosts
    parallel: true # │
  # │
  - name: test # │
    run: go test ./... #  │
    parallel: true # ┘

  - name: build # sequential — waits for lint + test
    run: go build .
```

### Branch filtering

```yaml
- name: deploy
  run: ./scripts/deploy.sh
  branches:
    - main # only runs when branch == "main"
```

When running locally with `pipe run`, no branch filtering is applied unless you
pass `--branch`:

```bash
pipe run --branch main
```

---

## Environment variables

`pipe` injects the following variables into every step. In server mode these
reflect the push event. In local mode they reflect the current git state.

| Variable      | Value                                 |
| ------------- | ------------------------------------- |
| `PIPE_REPO`   | Repository name                       |
| `PIPE_BRANCH` | Branch name (e.g. `main`)             |
| `PIPE_COMMIT` | Short commit SHA                      |
| `PIPE_REF`    | Full git ref (e.g. `refs/heads/main`) |

Pipeline-level `env:` keys are also available, overridable by the above.

---

## Local usage

### Install locally (CGO disabled)

```bash
# install to ~/.local/bin/pipe
make install-local

# optional: custom destination
make install-local INSTALL_PATH=/usr/local/bin/pipe

# verify
~/.local/bin/pipe version
```

If `~/.local/bin` is not in your `PATH`, add it:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

```bash
# Run all steps
pipe run

# Run in a specific directory
pipe run --dir /path/to/repo

# Run a single step by name
pipe run --step build

# Simulate a branch (enables branch-filtered steps)
pipe run --branch main

# Use a non-default pipeline file
pipe run --file .ci.yml
```

### Language quickstarts

Use these from inside your project root:

```bash
# Go
cp /path/to/pipe/examples/go-project.pipe.yml .pipe.yml
pipe run --branch main

# Rust
cp /path/to/pipe/examples/rust-project.pipe.yml .pipe.yml
pipe run --branch main

# Deno
cp /path/to/pipe/examples/deno-project.pipe.yml .pipe.yml
pipe run --branch main
```

Before running, adjust `env:` values in the copied file (binary name, entrypoint,
deploy host, etc.) to match your project.

---

## Server mode (soft-serve integration)

```bash
pipe server                                     # :9000, clone from http://soft-serve:23232
pipe server --port 8080
pipe server --clone ssh://git.example.com:23231
pipe server --workdir /var/lib/pipe
pipe server --gotify-endpoint "https://gotify.local/message" --gotify-token "$GOTIFY_TOKEN"
pipe server --gotify-endpoint "https://gotify.local/message" --gotify-token "$GOTIFY_TOKEN" --gotify-on fail
```

### Optional webhook hardening

- request body is capped at `64KiB`
- only branch refs (`refs/heads/*`) are accepted
- server uses sane read/write timeout defaults

### Endpoints

| Method | Path      | Description                             |
| ------ | --------- | --------------------------------------- |
| `POST` | `/run`    | Trigger a pipeline run                  |
| `GET`  | `/health` | Health check for Gatus / load balancers |

### Request body (`/run`)

```json
{
  "repo": "my-app",
  "ref": "refs/heads/main",
  "old": "abc1234",
  "new": "def5678"
}
```

### soft-serve post-receive hook

Place this at `/opt/containers/soft-serve/hooks/post-receive` (chmod +x):

```sh
#!/bin/sh
set -eu

while read -r OLD NEW REF; do
    REPO=$(basename "$PWD" .git)
    curl -fsS -X POST "http://pipe:9000/run" \
        -H "Content-Type: application/json" \
        -d "{\"repo\":\"$REPO\",\"ref\":\"$REF\",\"old\":\"$OLD\",\"new\":\"$NEW\"}"
done
```

### Optional Gotify notifications

When `--gotify-endpoint` is set, `pipe` can emit minimal notifications:

- `--gotify-token <token>`: app token sent as `X-Gotify-Key`
- `--gotify-on all` (default): notify success and failure
- `--gotify-on fail`: notify only failed runs
- `--gotify-priority <n>`: set Gotify message priority

### Compose service

```yaml
pipe:
  image: "ghcr.io/urutau-ltd/pipe:latest"
  container_name: pipe
  restart: always
  ports:
    - "127.0.0.1:9000:9000"
  volumes:
    - "/opt/containers/pipe/workdir:/tmp/pipe:Z"
  command:
    - "server"
    - "--clone"
    - "http://soft-serve:23232"
    - "--workdir"
    - "/tmp/pipe"
    - "--gotify-endpoint"
    - "${PIPE_GOTIFY_ENDPOINT}"
    - "--gotify-token"
    - "${PIPE_GOTIFY_TOKEN}"
    - "--gotify-priority"
    - "5"
    - "--gotify-on"
    - "all"
```

---

## Logs

In server mode, each run writes a log file to
`<workdir>/logs/<repo>-<timestamp>.log`. All output is also streamed to stdout,
visible in [Dozzle](https://dozzle.dev).

---

## Pipe Actions

If you miss GitHub Actions, you can do your own "actions" for `pipe`.

### Derivating images

Using `pipe` as the base image, like:

```bash
pipe-base        (git, curl, ssh)
  └── pipe-go    (+ go toolchain, golangci-lint)
       └── pipe-release  (+ cosign, syft, gpg)
  └── pipe-node  (+ node, npm)
  └── pipe-deploy (+ kubectl, helm, ssh)
```

### Remote Scripts

> [!WARNING]
> I think I don't need to tell you why this is kind of a bad idea in the first
> place. If you happen to not know, it's a terrible idea to run arbitrary
> internet scripts with curl-into-sh.

However, if you feel brave enough, you can setup a repository, for example
`pipe-action-go` with reusable shell scripts:

```yml
# En cualquier .pipe.yml
- name: deploy
  run: |
    curl -fsSL https://raw.githubusercontent.com/urutau-ltd/pipe-action-go/main/deploy.sh | sh
    # with submodule: source ./scripts/deploy.sh
```

### YAML Anchors

```yml
x-quality: &quality
  parallel: true
  branches: [main, develop]

steps:
  - name: lint
    <<: *quality
    run: golangci-lint run

  - name: test
    <<: *quality
    run: go test ./...
```

---

## Examples

See the [`examples/`](./examples/) directory:

| File                  | Demonstrates                                        |
| --------------------- | --------------------------------------------------- |
| `go-project.pipe.yml` | Parallel lint + test, build, deploy                 |
| `rust-project.pipe.yml` | cargo fmt + clippy, test, release build, deploy   |
| `deno-project.pipe.yml` | deno fmt + lint, test, compile, deploy             |
| `gpg-sign.pipe.yml`   | GPG-signing a release binary                        |
| `attest.pipe.yml`     | SLSA provenance + cosign attestation                |
| `artifacts.pipe.yml`  | Collecting, hashing, and publishing build artifacts |

### Pipe itself

`pipe` itself uses `pipe` to test. Look:

```bash
$ make build && make demo
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=7096b6d" -o dist/pipe .
==> built dist/pipe (7096b6d)
==> running pipe against itself inside Podman
podman run --rm \
	--name pipe-demo \
	-v "/home/fplinux/src/pipe:/src:ro,Z" \
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
( 1/13) Installing brotli-libs (1.2.0-r0)
( 2/13) Installing c-ares (1.34.6-r0)
( 3/13) Installing libunistring (1.4.1-r0)
( 4/13) Installing libidn2 (2.3.8-r0)
( 5/13) Installing nghttp2-libs (1.68.0-r0)
( 6/13) Installing nghttp3 (1.13.1-r0)
( 7/13) Installing libpsl (0.21.5-r3)
( 8/13) Installing zstd-libs (1.5.7-r2)
( 9/13) Installing libcurl (8.17.0-r1)
(10/13) Installing libexpat (2.7.5-r0)
(11/13) Installing pcre2 (10.47-r0)
(12/13) Installing git (2.52.0-r0)
(13/13) Installing git-init-template (2.52.0-r0)
Executing busybox-1.37.0-r30.trigger
OK: 20.6 MiB in 30 packages

╔══ pipe: pipe ══╗  (parallel ok  cpus=28)

[03:26:48] ⇉  vet
✓  vet (2.169s)
[03:26:48] ⇉  test
?   	github.com/urutau-ltd/pipe	[no test files]
✓  test (2.187s)
[03:26:50] ▶  build
==> pipe v0.1.0
-rwxr-xr-x    1 root     root        6.4M Apr 19 03:26 dist/pipe
✓  build (546ms)

────────────────────────────────
  PASSED  passed=3  failed=0  skipped=0  time=4.902s
```
