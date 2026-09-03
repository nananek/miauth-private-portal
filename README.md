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
`build`, `tidy`.

## Roadmap

- Open WebUI integration: [docs/roadmap/openwebui.md](docs/roadmap/openwebui.md)
