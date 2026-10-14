# Shipyard — Mini-PaaS in Go 🚢

Shipyard is a lightweight, self-hosted Platform-as-a-Service (PaaS) written in Go. It turns git repositories into live applications with automatic subdomain routing behind Traefik at `http://<app>.127.0.0.1.nip.io`.

---

## 🏗️ Architecture

```
                 +----------------------------------+
                 |           Shipyard CLI           |
                 |      (cmd/shipyard/main.go)      |
                 +-----------------+----------------+
                                   | HTTP REST API
                                   v
+----------------------------------------------------------------------+
|  Shipyard Daemon (cmd/shipyardd)                                     |
|                                                                      |
|  +------------------+   +-------------------+  +------------------+  |
|  |  REST API Layer  |-->|  Job Coordinator  |->| internal/builder |  |
|  |   (Go net/http)  |   |  & State Machine  |  | (Git + Docker)   |  |
|  +--------+---------+   +---------+---------+  +--------+---------+  |
|           |                       |                     |            |
|           v                       v                     v            |
|  +------------------+   +-------------------+  +------------------+  |
|  |  internal/store  |   | internal/runtime  |  | Log Streamer     |  |
|  | (SQLite, no CGO) |   | (Docker Go SDK)   |  | (per deployment) |  |
|  +------------------+   +---------+---------+  +------------------+  |
+-----------------------------------|----------------------------------+
                                    | Docker API (docker.sock)
                                    v
                 +----------------------------------+
                 |          Docker Engine           |
                 |                                  |
                 |   +--------------------------+   |
                 |   | Traefik v3 Reverse Proxy |   |
                 |   | (reads container labels) |   |
                 |   +-------------+------------+   |
                 |                 |                |
                 |    Network: shipyard (bridge)    |
                 |                 |                |
                 |      +----------+----------+     |
                 |      |                     |     |
                 |      v                     v     |
                 | [app-1 container]  [app-2 container]
                 +----------------------------------+
```

---

## 📦 Project Layout

- `cmd/shipyard/`: The developer CLI client (built with Cobra).
- `cmd/shipyardd/`: The Shipyard server daemon managing app lifecycles.
- `internal/store/`: Pure Go SQLite storage engine (`modernc.org/sqlite`, zero CGO) with automatic migrations.
- `internal/builder/`: Clones Git repositories and compiles Docker images using the Docker Engine SDK.
- `internal/runtime/`: Manages container lifecycles and applies Traefik dynamic labels.
- `internal/server/`: REST API handlers (`/apps`, `/deployments`, `/logs`) and authentication.
- `testapp/`: A sample multi-stage Go web application for end-to-end testing.
- `docker-compose.traefik.yml`: Traefik v3 reverse proxy configured on the `shipyard` network.

---

## 🚀 Quick Start Guide

### 1. Start Traefik Reverse Proxy
Traefik watches Docker labels and automatically routes incoming HTTP requests to the right container without restarting.
```bash
docker compose -f docker-compose.traefik.yml up -d
```

### 2. Build Shipyard Binaries
```bash
go build -o bin/shipyardd ./cmd/shipyardd
go build -o bin/shipyard ./cmd/shipyard
```

### 3. Start the Shipyard Daemon
```bash
./bin/shipyardd
```
*(By default, runs on `http://127.0.0.1:8888` and stores state in `~/.shipyard/shipyard.db`)*

### 4. Create and Deploy an Application
In another terminal:
```bash
# Register an app (supports remote git URLs or local folders)
./bin/shipyard apps create todo-api --repo /path/to/your/app

# Deploy the application
./bin/shipyard deploy todo-api
```

### 5. Access Your Live Application
Shipyard automatically routes the app on `nip.io` subdomains:
```bash
curl http://todo-api.127.0.0.1.nip.io
```
Or open `http://todo-api.127.0.0.1.nip.io` in your browser.

---

## 📋 CLI Reference

| Command | Description |
|---|---|
| `shipyard apps list` | List all registered applications and their live status |
| `shipyard apps create <name> --repo <url> [--port 8080]` | Register a new application |
| `shipyard deploy <name>` | Trigger an asynchronous build & zero-downtime rollout |
| `shipyard logs <name>` | View live runtime stdout/stderr of the container |
| `shipyard logs --build <deployment-id>` | View build logs for a specific deployment |

---

## 🛠️ REST API Reference

All endpoints return JSON and can optionally be secured by setting `SHIPYARD_API_TOKEN`.

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/apps` | Register an application |
| `GET` | `/apps` | List all applications |
| `GET` | `/apps/{name}` | Get app details & live deployment |
| `DELETE` | `/apps/{name}` | Stop container and delete app |
| `POST` | `/apps/{name}/deploy` | Trigger a new deployment |
| `GET` | `/apps/{name}/deployments` | List deployment history |
| `GET` | `/deployments/{id}` | Check status of a deployment |
| `GET` | `/deployments/{id}/logs` | Stream or view build logs |
| `GET` | `/apps/{name}/logs` | Stream container runtime logs |
| `GET` | `/healthz` | Health check |
