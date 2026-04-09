# podproxy

A self-hosted server that transparently caches the podcasts you listen to.

Add a feed once, get back a proxied RSS URL, add that URL to any standard podcast app. Episodes are cached to disk on first play and served locally on subsequent plays. Optional background prefetch downloads new episodes before you hit play.

---

## Quick Start (local)

```bash
go build -o podproxy .
./podproxy
```

Open [http://localhost:8080/ui](http://localhost:8080/ui) to manage feeds.

---

## Docker Deployment

The image is published to the GitHub Container Registry and pulled automatically by the compose file:

```
ghcr.io/mayobytes/podproxy:latest
```

### 1. Create a config file

```bash
cp deploy/config.yaml.example deploy/config.yaml
```

Edit `deploy/config.yaml` and set `base_url` to the address your podcast app will use to reach the server (e.g. your NAS IP or hostname):

```yaml
server:
  port: 8080
  base_url: "http://192.168.1.100:8080"
```

### 2. Start the container

```bash
cd deploy
docker compose up -d
```

To update to the latest image later:

```bash
docker compose pull && docker compose up -d
```

Data and cache are stored in named Docker volumes (`podproxy-data`, `podproxy-cache`). To use host paths instead, replace the volume entries in `docker-compose.yml`:

```yaml
volumes:
  - /mnt/nas/podproxy/data:/app/data
  - /mnt/nas/podproxy/cache:/app/cache
```

---

## Deployment

### TrueNAS SCALE

1. Go to **Apps → Discover Apps → Custom App**.
2. Use `ghcr.io/mayobytes/podproxy:latest` as the image.
3. In **Network Configuration** add the host port and container port you wish to use.
4. In **Storage Configuration** use type "Host Path" (if using a path on your NAS) and add a host path for both `/app/data` (app database) and `/app/cache` (cached feeds and episodes).

### Portainer

Use **Stacks → Add stack → Web editor** and paste this compose definition. Configure via environment variables instead of a config file:

```yaml
services:
  podproxy:
    image: ghcr.io/mayobytes/podproxy:latest
    ports:
      - "8080:8080"
    environment:
      - PODPROXY_BASE_URL=http://192.168.1.100:8080
    volumes:
      - podproxy-data:/app/data
      - podproxy-cache:/app/cache
    restart: unless-stopped

volumes:
  podproxy-data:
  podproxy-cache:
```

Replace `192.168.1.100` with your server's IP or hostname. See the [Configuration](#configuration) table for additional environment variables.

To update to the latest image, go to the stack in Portainer and click **Pull and redeploy**.

---

## Nginx Reverse Proxy
Podproxy currently provides no auth and all the API endpoints are publicly accessible by default.
If you plan to have your podproxy feeds publicly visible (required to work with some podcast apps like Apple Podcasts & Overcast), then you need to expose those endpoints while protecting the rest.

If you don't know what this is, then just keep podproxy on your local network or tailnet.
```conf
server {
    listen 443 ssl;
    server_name <your domain>;

    # Public read-only access to feeds and episodes
    location /feeds/ {
        limit_except GET HEAD {
            deny all;
        }
        proxy_pass http://<your-server-ip>:<port>;

        proxy_buffering off;
        proxy_read_timeout 120s;
        proxy_send_timeout 120s;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location /episodes/ {
        limit_except GET HEAD {
            deny all;
        }
        proxy_pass http://<your-server-ip>:<port>;

        proxy_buffering off;
        proxy_read_timeout 120s;
        proxy_send_timeout 120s;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    # Everything else (API, UI, health) is blocked entirely
    location / {
        deny all;
    }

    ssl_certificate /path/to/cert; # managed by Certbot
    ssl_certificate_key /path/to/cert; # managed by Certbot
}
```

## Contributing

Commit messages must follow [Conventional Commits](https://www.conventionalcommits.org/) format (`type: description`). This is enforced in CI on pull requests.

To enable the same check locally, point Git at the checked-in hooks directory:

```bash
git config core.hooksPath .githooks
```

Run tests before pushing:

```bash
go test ./...
```


---

## A Note on Hosting
Most podcast CDNs rate-limit or block VPN IPs. If you're hosting behind a VPN and see `unexpected EOF` errors in the logs, that's likely why.


---

## Usage

### Web UI

Navigate to `http://<host>:8080/ui` in a browser:

- **Add Feed** — paste an RSS URL; the server fetches it, slugifies the title, and returns a proxy URL.
- **Episodes** — view all episodes for a feed with their cache status.
- **Refresh** — force a re-fetch of the feed's RSS to discover new episodes.
- **Prefetch** — queue all un-cached episodes for background download.
- **Cache Selected / Delete Cached** — bulk mode (toggle via "Select") lets you pick individual episodes to cache or purge from disk.
- **Delete** — remove a feed and all its metadata.

### Podcast App Setup

After adding a feed via the UI or API, copy the **Proxy URL** (shown on the Episodes page) and add it to your podcast app instead of the original RSS URL.

### API

```
POST   /api/feeds                        { "url": "https://..." }
GET    /api/feeds
DELETE /api/feeds/:id
POST   /api/feeds/:id/refresh
POST   /api/feeds/:id/prefetch
POST   /api/feeds/:id/bulk-cache         { "episode_ids": [...] }
POST   /api/backups
GET    /api/backups
GET    /health
```

---

## Configuration

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `server.port` | `PODPROXY_PORT` | `8080` | HTTP listen port |
| `server.base_url` | `PODPROXY_BASE_URL` | `http://localhost:8080` | Public URL (used in proxy URLs) |
| `storage.cache_dir` | — | `./cache` | Episode audio cache directory |
| `storage.data_dir` | — | `./data` | SQLite database directory |
| `defaults.refresh_interval_minutes` | `PODPROXY_REFRESH_INTERVAL_MINUTES` | `60` | How often feeds are re-fetched |
| `defaults.auto_prefetch` | `PODPROXY_AUTO_PREFETCH` | `false` | Download new episodes automatically after each refresh |
| `defaults.prefetch_max_age_days` | `PODPROXY_PREFETCH_MAX_AGE_DAYS` | `30` | Skip prefetch for episodes older than this (0 = no limit) |
| `defaults.prefetch_concurrency` | `PODPROXY_PREFETCH_CONCURRENCY` | `2` | Simultaneous background download workers |
| `backup.dir` | — | `{data_dir}/backups` | Directory for backup files |
| `backup.max_backups` | `PODPROXY_MAX_BACKUPS` | `5` | Backups to keep; oldest pruned when exceeded (0 = unlimited) |
| `backup.interval_minutes` | `PODPROXY_BACKUP_INTERVAL_MINUTES` | `0` | Scheduled backup interval in minutes (0 = disabled) |

Environment variables take precedence over `config.yaml`. This is useful for Docker deployments where you want to configure the server without mounting a config file:

```bash
docker run \
  -e PODPROXY_BASE_URL=http://192.168.1.100:8080 \
  -e PODPROXY_AUTO_PREFETCH=true \
  -e PODPROXY_PREFETCH_CONCURRENCY=4 \
  -e PODPROXY_BACKUP_INTERVAL_MINUTES=60 \
  ...
```
