# miauth-private-portal

## Getting started

Prerequisites: Go 1.24 or newer.

```sh
cp .env.example .env
make check   # gofmt check, go vet, go test
make run     # builds bin/server and starts it on HTTP_PORT (default 8080)
```

`make run` reads configuration from `.env` (see
[docs/operations/configuration.md](docs/operations/configuration.md) for
every key, its default, and validation rules), then process environment
variables override anything in that file. Once running:

```sh
curl -i http://localhost:8080/healthz   # always 200
curl -i http://localhost:8080/readyz    # 200 once startup has finished
```

Send `SIGINT`/`SIGTERM` (or Ctrl-C) to stop the server; it shuts down
gracefully, waiting for in-flight requests up to
`HTTP_SHUTDOWN_GRACE_PERIOD` before forcing connections closed.

Other `make` targets: `fmt`, `fmt-check`, `vet`, `test`, `test-race`,
`build`, `tidy`, `contract-test`.

## Contract tests

`make contract-test` runs `contract/aria_client`, a Dart package
depending on `misskey_dart` (the pinned client library Aria itself uses)
against a real `bin/server` instance, in place of the unautomated real-Aria
end-to-end run Issue #7's acceptance criteria otherwise call for — see
[docs/compat/aria-v1.5.11.md](docs/compat/aria-v1.5.11.md)'s "Issue #7
implementation notes". It requires the [Dart SDK](https://dart.dev/get-dart)
and `jq` in addition to this project's normal Go toolchain. The local MiAuth
session is approved with `miauthctl`; no real credentials are required.
It runs in its own CI workflow
([`.github/workflows/contract-tests.yml`](.github/workflows/contract-tests.yml)),
separate from `ci.yml`.

## Container

A multi-stage `Dockerfile` builds a fully static binary
(`modernc.org/sqlite` is pure Go, so `CGO_ENABLED=0` needs no libc) onto
`gcr.io/distroless/static-debian12:nonroot`. Images are published to
`ghcr.io/nananek/miauth-private-portal` on every push to `main` (see
[`.github/workflows/docker-publish.yml`](.github/workflows/docker-publish.yml)),
tagged `latest` and `sha-<short>`, `linux/amd64` only for now.

```sh
docker build -t miauth-private-portal .

mkdir -p ./data && chmod 777 ./data  # distroless has no shell to do this
                                      # inside the container; see below

docker run -d \
  -p 8080:8080 \
  -e LOCAL_ORIGIN=https://portal.example \
  -e APP_ENV=production \
  -v "$(pwd)/data:/data" \
  ghcr.io/nananek/miauth-private-portal:latest
```

The `nonroot` base image runs as uid `65532` and has no shell, so `/data`
must already be writable by that uid before the container starts —
either `chmod`/`chown` the host directory as above, or run the container
with `--user` matching your volume's ownership. `.env` is never baked
into the image (`.dockerignore` excludes it); configure a production
deployment entirely through `docker run -e` / your orchestrator's
environment variables instead, per
[docs/operations/configuration.md](docs/operations/configuration.md).

`cmd/miauthctl` ships in the same image; run it against the same `DB_PATH`
volume with an explicit entrypoint override. Supply the same required base
configuration as the server:

```sh
docker run --rm -it -v "$(pwd)/data:/data" --entrypoint /miauthctl \
  -e APP_ENV=production -e LOCAL_ORIGIN=https://portal.example \
  -e DB_PATH=/data/portal.db ghcr.io/nananek/miauth-private-portal:latest list
```

`make build` produces `bin/server`, the sign-in/token operator tool
`bin/miauthctl`, the host-local durable-job inspection/retry tool
`bin/jobsctl`, and `bin/mailfetch` (Issue #12's IMAP ingestion sidecar;
see below).

### IMAP ingestion (`docker-compose.yml`)

Issue #12's IMAP mail ingestion runs its untrusted IMAP/MIME parsing in a
separate process, `cmd/mailfetch`, never in the main server (see
[docs/decisions/0003-imap-mailfetch-isolation.md](docs/decisions/0003-imap-mailfetch-isolation.md)).
For a container deployment with `IMAP_ENABLED=true`, use this
repository's `docker-compose.yml` instead of a bare `docker run`:

```sh
cp .env.example .env   # then set IMAP_* keys; see docs/operations/configuration.md
docker compose --profile imap up -d --build
```

Leaving `IMAP_ENABLED=false` (the default) and running `docker compose up -d`
without the `--profile imap` flag starts only `server`; `cmd/mailfetch`
never needs to run at all. A bare-host deployment instead runs
`bin/mailfetch` as a second process/systemd unit — see
[docs/operations/configuration.md](docs/operations/configuration.md)'s
"Deploying `cmd/mailfetch`" section.

## Approving Aria sign-ins

MiAuth requests remain pending until an operator connected to the server
host approves the exact session. `make build` produces `bin/miauthctl`:

```sh
bin/miauthctl list
bin/miauthctl approve <session-id>
```

The approval command displays the request details and requires an explicit
confirmation. Use `--yes` only from trusted automation. Operators can also
run `reject`, `tokens`, and `revoke <token-id>`; all commands use the same
configuration and `DB_PATH` as the server.

## Operations runbook

[docs/operations/runbook.md](docs/operations/runbook.md) covers day-two
operator procedures this README's setup instructions above don't:
incident response/troubleshooting, secret rotation, revoking access,
database/file permissions, reverse proxy and TLS termination (this
service never terminates TLS itself), request rate/concurrency limits
(delegated to the reverse proxy), and log retention. See
[docs/operations/configuration.md](docs/operations/configuration.md) for
the config reference the runbook builds on.

## Roadmap

- Open WebUI integration: [docs/roadmap/openwebui.md](docs/roadmap/openwebui.md)
