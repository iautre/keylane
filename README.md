# KeyLane

**A self-hosted OpenAI-compatible gateway that routes each model through a pool of your API accounts.**

KeyLane (模型线路网关) runs on your NAS or server and gives your clients one endpoint, with latency-aware selection, ordered fallback, and a small web console.

> Early development: the first implementation is taking shape. APIs and configuration may change before the first stable release.

## Technology

- Go + Gin HTTP server
- Vue 3 + TypeScript + Vite + Element Plus dashboard
- SQLite through `database/sql` and the CGo-free `modernc.org/sqlite` driver
- Docker Compose for NAS/server deployment

The service uses `github.com/iautre/gowk` for HTTP lifecycle and explicitly sets `DATABASE_ENABLED=false`; SQLite is initialized separately by this app. The repository's `gowk.New()` also installs a response logging middleware that buffers bodies, so the proxy creates a Gin engine directly and uses a response-safe recovery handler to preserve transparent streaming. The build uses Go 1.27.4 to match gowk's current module requirement.

## Run locally

```sh
git clone https://github.com/iautre/keylane.git
cd keylane
```

Copy `.env.example` to `.env`, set strong values for all secrets, export the configuration before starting gowk, then run:

```sh
set -a; . ./.env; set +a
go run ./cmd/keylane
```

In another terminal:

```sh
cd web
npm install
npm run dev
```

The Vite dev server proxies `/admin/api`, `/v1`, `/health`, and `/ready` to Go on port 8080.

## Docker

```sh
cp .env.example .env
# Edit ADMIN_PASSWORD, GATEWAY_API_KEY, and ENCRYPTION_SECRET.
docker compose pull
docker compose up -d
```

Open `http://localhost:18080`. Set `PORT` in `.env` to change the host port. SQLite is stored in the persistent Docker volume `keylane-data`.

The Compose file pulls the CNB artifact `docker.cnb.cool/autre/keylane:latest`. CNB also publishes each build as a numbered beta tag, such as `beta00001`; set `KEYLANE_IMAGE=docker.cnb.cool/autre/keylane:beta00001` in `.env` to pin a specific beta.

## Routing

Each public model has a pool of one or more account routes. Pool strategy is either:

- `fastest`: selects a healthy account by recent real-request and scheduled-probe latency measurements.
- `ordered`: tries accounts in configured priority order.

Both modes share a per-upstream-attempt timeout, an overall request timeout (including retries and streaming), maximum upstream attempts, failure threshold, cooldown, and retryable-status settings. Retries only happen before response headers/body are committed to the client. SSE bytes are streamed through; once downstream output starts, the gateway does not switch accounts mid-stream.

Latency selection is an estimate based on recent observations, not a guarantee about the next upstream response. Scheduled probes use a short prompt and consume upstream quota.

## Security and operation

- Keep the service on a private network or behind HTTPS when accessed remotely.
- The dashboard uses `ADMIN_PASSWORD`; downstream clients use `GATEWAY_API_KEY`.
- Provider API keys are encrypted in SQLite using `ENCRYPTION_SECRET`. Back up the database and encryption secret together.
- Prompt and completion bodies are not written to request history.


## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md) for local build steps and contribution guidance, and [SECURITY.md](SECURITY.md) for vulnerability reporting. KeyLane is licensed under the [MIT License](LICENSE).

## Docker image publishing

GitHub Actions builds multi-architecture images for `linux/amd64` and `linux/arm64` on pushes to `main`. Pull requests build the image without publishing it. Published beta images use a five-digit, zero-padded GitHub Actions run number, for example `iautre/keylane:beta00001`.

Before the first push to `main`, add a Docker Hub access token as the repository Actions secret `DOCKERHUB_TOKEN`. Images are published to `docker.io/iautre/keylane`.

CNB builds multi-architecture images for `linux/amd64` and `linux/arm64` on pushes to `main`, publishing both a numbered beta tag (from the main-branch commit count) and `latest` to its same-name Docker artifact at `${CNB_DOCKER_REGISTRY}/${CNB_REPO_SLUG_LOWERCASE}`. The CNB Docker service handles authentication to its artifact registry. GitHub Actions publishes numbered beta tags to Docker Hub independently.
