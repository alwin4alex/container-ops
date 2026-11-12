# Container Ops

A central dashboard for Docker Swarm stacks across environments. Go + htmx, one binary.

- **Projects:** a card per project with status per environment, stacks, services, containers (running / stopped / healthy / with env issues), volumes, images, CPU, RAM and nodes.
- **Containers:** one tab per environment, with every service's image, digest and build date, replicas, env-var check and last deploy.
- **Env-var check:** compares each service with its repo's `.env.example`. It shows only *present / missing / empty*; values are never read into the dashboard.
- **Logs:** the last N lines per replica, then live following over server-sent events. Lines can be filtered by level, replica and text, and secret-looking values are masked.
- **Deploy / tear down:** starts the project's GitHub Actions workflow (`workflow_dispatch`). The self-hosted runner in each environment does the work. The dashboard never holds SSH keys or write access to Docker.

## How it fits together

```
browser ──► container-ops ──► docker-socket-proxy (read-only, per Swarm manager) ──► Docker API
                 │
                 ├──► GitHub API: read .env.example, start deploy-stack workflow, list runs
                 └──► Harbor API: build date of each running digest (optional)

GitHub Actions ──► self-hosted runner on the environment ──► docker stack deploy / rm
```

## Setup

1. **Read-only Docker API on each Swarm cluster.**
   ```
   docker stack deploy -c deploy/socket-proxy-stack.yml docker-api
   ```
   Allow TCP 2375 only from the dashboard host. Port 2375 is plain HTTP inside your network; use the `tls_*` endpoint fields if you put TLS in front of it.

2. **Deploy workflow in each project repo.** Copy `deploy/workflows/deploy-stack.yml` to `.github/workflows/` in the repo that holds `docker-compose.yml` and `.env.example`. On each environment:
   - Register a self-hosted runner with a label equal to the environment id (`DEV`, `INT`, `SIT`).
   - Keep the server `.env` at `/opt/stacks/<stack>/.env`. You can change that path with the repo variable `STACK_ROOT`.
   - Log the runner user in to Harbor.

   Add required reviewers to the GitHub environment `SIT` if SIT deploys should wait for approval.

3. **GitHub token.** Use a fine-grained token, or a GitHub App, limited to the project repos, with:
   - **Actions:** read & write (start runs, list runs)
   - **Contents:** read (`.env.example`)

4. **Config.** Copy `config.example.yaml` and fill in environments, projects and endpoints. Generate a password hash with:
   ```
   docker run --rm -i harbor.local/devops/container-ops hash-password
   ```
   Secrets can come from env vars or files instead of the config file: `GITHUB_TOKEN(_FILE)`, `HARBOR_PASSWORD(_FILE)`, `SECRET_KEY(_FILE)`.

5. **Run it.** CI (`.github/workflows/ci.yml`) builds the image and pushes it to Harbor with OCI labels. Deploy the dashboard with `deploy/dashboard-stack.yml`, behind Caddy for TLS.

## Configuration notes

- **Environments** are only defined in `environments:`. Their order is the order of tabs and pills. `require_confirm` makes the user type the project id before a deploy or teardown there; `require_approval` labels the environment as waiting for a GitHub reviewer.
- **Per-service required lists:** `required_env.services.<name>` points a service at its own `.env.example`. Services not listed use `required_env.default`.
- **Auth modes:**
  - `basic`: users in the config, PBKDF2-SHA256 hashes.
  - `header`: a reverse proxy such as Caddy `forward_auth` sets `X-Forwarded-User`, and optionally `X-Forwarded-Role`. Only requests from `trusted_proxies` are accepted.
  - `none`: local development only.

  Roles: `viewer` is read-only; `deployer` can also deploy and tear down.
- **Last known state:** when an endpoint stops answering, the dashboard keeps its last snapshot, marks the environment Down, and says the data is not live.

## Limits to know

- **Log driver:** logs need the `json-file`, `local` or `journald` log driver. Services that log elsewhere (gelf, fluentd) show no lines.
- **Log history:** only what is still on the nodes, within your log rotation, is available. Use the central log stack for history and search.
- **Masking:** it catches common patterns (`*password*=`, `*token*=`, URL credentials, bearer tokens, JWTs). Services should still not log secrets.
- **Env-var check:** only as good as `.env.example`. A variable missing from that file is never reported.
- **Workflow env check:** the workflow's pre-deploy check uses the repo's root `.env.example`. The dashboard also supports per-service lists.
- **Image counts:** the images count comes from `/info` on the manager node the proxy runs on, not from the whole cluster.
- **Build date:** comes from Harbor. It needs the image pushed with an `org.opencontainers.image.created` label (CI here does that) or Harbor's created time. Images from other registries show "build date unknown".
- **Run history:** GitHub does not return a run id when a workflow is started. The dashboard shows recent runs of the workflow, newest first, and the run name includes action, stack, environment and user.

## Development

```
go test ./...                     # unit + end-to-end tests against fake Docker/GitHub APIs
go run ./cmd/container-ops -config config.yaml
```
