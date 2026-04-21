# pipe

<div align="center">
    <img src=".repo-assets/pipe.svg" width="150" alt="pipe logo" />
</div>

Stupidly Light CI runner for
[`soft-serve`](https://github.com/charmbracelet/soft-serve) and local machines.

Single binary. No database. No API keys. Pipeline defined as `.pipe.yml` (or
`.pipe/*.yml`) in each repository.

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
| `PIPE_PIPELINE` | Pipeline file used (e.g. `.pipe/ci.yml`) |
| `PIPE_ACTIONS_URL` | Base URL for shared actions (if configured) |

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

Before running, adjust `env:` values in the copied file (binary name,
entrypoint, deploy host, etc.) to match your project.

### Many pipelines in one repository

If one repository needs different pipeline goals (fast CI, release, nightly),
keep them inside `.pipe/` and select by name.

Suggested layout:

```text
.pipe/
  ci.yml
  release.yml
  nightly.yml
```

Local runs:

```bash
# fast checks on every push
pipe run --pipeline ci --branch main

# release artifacts/signing
pipe run --pipeline release --branch main

# nightly maintenance/security
pipe run --pipeline nightly --branch main
```

Server mode uses a single runner. Send `"pipeline":"ci"` for one pipeline, or
`"pipelines":["ci","release"]` to run several in one push.

---

## Server mode (soft-serve integration)

```bash
pipe server                                     # :9000, clone from http://soft-serve:23232
pipe server --port 8080
pipe server --clone ssh://git.example.com:23231
pipe server --workdir /var/lib/pipe
pipe server --actions-url "https://raw.githubusercontent.com/acme/pipe-actions/main"
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
  "new": "def5678",
  "pipeline": "ci",
  "pipelines": ["ci", "release"]
}
```

Use either `pipeline` or `pipelines` (not both). If neither is sent, server
uses the default file configured with `--file` (default `.pipe.yml`).

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

### soft-serve hook for many pipelines

Single-runner example selecting one or many `.pipe/*.yml` files by branch:

```sh
#!/bin/sh
set -eu

while read -r OLD NEW REF; do
    REPO=$(basename "$PWD" .git)
    case "$REF" in
      refs/heads/main)
        PIPELINES='["ci","release"]'
        ;;
      refs/heads/nightly)
        PIPELINES='["nightly"]'
        ;;
      *)
        PIPELINES='["ci"]'
        ;;
    esac

    curl -fsS -X POST "http://pipe:9000/run" \
      -H "Content-Type: application/json" \
      -d "{\"repo\":\"$REPO\",\"ref\":\"$REF\",\"old\":\"$OLD\",\"new\":\"$NEW\",\"pipelines\":$PIPELINES}"
done
```

That is usually enough for one `pipe` instance to cover all repositories and
all workflow types (`ci`, `release`, `nightly`, etc.).

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
    - "--actions-url"
    - "${PIPE_ACTIONS_URL:-}"
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

You can reuse actions without derived images or bind mounts.

### Shared actions by URL (GitHub/Codeberg)

Host executable scripts in a repo (for example `go/test.sh`,
`release/publish.sh`) and expose raw files.

Start server with a base URL:

```bash
pipe server --actions-url "https://raw.githubusercontent.com/acme/pipe-actions/main"
# or Codeberg raw endpoint
# pipe server --actions-url "https://codeberg.org/acme/pipe-actions/raw/branch/main"
```

Use those scripts in pipelines:

```yaml
steps:
  - name: test
    run: pipe_action go/test.sh

  - name: release
    branches: [main]
    run: pipe_action release/publish.sh v1.2.3
```

`pipe_action <path> [args...]` downloads `${PIPE_ACTIONS_URL}/<path>` with
`curl -fsSL` and executes it.

Security baseline:
- pin immutable URLs when possible (commit SHA/tag, not moving branches)
- keep the action repo private/internal when appropriate
- treat actions like code dependencies (review and version them)

### Copy-paste fallback (no network actions)

For teams that prefer zero remote scripts, keep reusable shell blocks inside
YAML anchors and copy between repos.

```yaml
x-go-vet-run: &go_vet_run |
  go vet ./...

x-go-test-run: &go_test_run |
  go test ./...

steps:
  - name: go-vet
    run: *go_vet_run

  - name: go-test
    run: *go_test_run
```

### YAML Anchors

Use anchors for policy blocks and reusable action calls.

```yaml
x-quality: &quality
  parallel: true
  branches: [main, develop]

x-release-only: &release_only
  branches: [main, release]

x-go-vet-action: &go_vet_action pipe_action go/vet.sh
x-go-test-action: &go_test_action pipe_action go/test.sh

steps:
  - name: go-vet
    <<: *quality
    run: *go_vet_action

  - name: go-test
    <<: *quality
    run: *go_test_action

  - name: release-upload
    <<: *release_only
    run: pipe_action release/upload.sh
```

---

## Examples

See the [`examples/`](./examples/) directory:

| File                     | Demonstrates                                        |
| ------------------------ | --------------------------------------------------- |
| `go-project.pipe.yml`    | Parallel lint + test, build, deploy                 |
| `rust-project.pipe.yml`  | cargo fmt + clippy, test, release build, deploy     |
| `deno-project.pipe.yml`  | deno fmt + lint, test, compile, deploy              |
| `multi-ci.pipe.yml`      | Fast CI pipeline for frequent pushes                |
| `multi-release.pipe.yml` | Release artifacts, checksums, signing               |
| `multi-nightly.pipe.yml` | Nightly maintenance/security checks                 |
| `monorepo.pipe.yml`      | Multi-service monorepo checks and packaging         |
| `gpg-sign.pipe.yml`      | GPG-signing a release binary                        |
| `attest.pipe.yml`        | SLSA provenance + cosign attestation                |
| `artifacts.pipe.yml`     | Collecting, hashing, and publishing build artifacts |

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
