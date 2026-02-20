# SockTails

A Tailscale-based SOCKS5 proxy written in Go, designed to run on **Google Cloud Run Jobs**.

Run an ephemeral on-demand SOCKS5 proxy in any Cloud Run region (e.g. `asia-southeast1`), accessible only to your tailnet. The proxy automatically disappears from your tailnet when the job exits.

## How it works

```
Browser (SOCKS5 proxy = <tailscale-ip>:1080)
   │
   │  via Tailscale (WireGuard)
   ▼
Cloud Run Job (tsnet userspace networking)
   │
   │  regular outbound TCP (Cloud Run region egress)
   ▼
Target website / service
```

- **tsnet** (embedded Tailscale) runs in userspace — no TUN device required.
- The node registers as an **ephemeral** node and vanishes from the tailnet when the job exits.
- The SOCKS5 server listens on the Tailscale virtual interface (not exposed to the public internet).
- Outbound traffic exits through the Cloud Run region's internet gateway.

---

## Prerequisites

| Tool | Purpose |
|------|---------|
| Go 1.25+ | Build the binary |
| Docker | Build the container image |
| `gcloud` CLI | Deploy to Cloud Run |
| Tailscale account | Auth key + tailnet |

---

## Configuration

| Source | Variable | Default | Description |
|--------|----------|---------|-------------|
| Env / flag | `TAILSCALE_AUTHKEY` | *(required)* | Ephemeral, reusable, pre-authorised Tailscale auth key |
| Env / `--port` | `SOCKS_PORT` | `1080` | SOCKS5 listen port |
| Env / `--duration` | `DURATION` | `4h` | How long to run before exiting |
| Env / `--hostname` | `TS_HOSTNAME` | `socktails` | Tailscale node hostname |

---

## Step-by-step deployment

### 1 — Create a Tailscale auth key

1. Go to <https://login.tailscale.com/admin/settings/keys>.
2. Click **Generate auth key**.
3. Enable **Reusable** and **Ephemeral**.
4. Optionally add a tag such as `tag:cloudrun-jobs` (requires an ACL tag owner entry).
5. Copy the key; you will store it in Secret Manager next.

### 2 — Store the key in Secret Manager

```bash
export PROJECT_ID=my-gcp-project
export REGION=asia-southeast1

echo -n "tskey-auth-<YOUR_KEY>" | \
  gcloud secrets create tailscale-authkey \
    --data-file=- \
    --replication-policy=automatic \
    --project=$PROJECT_ID
```

Grant the Cloud Run Job's service account access:

```bash
# Default Compute service account — replace if you use a custom SA.
SA_EMAIL=$(gcloud iam service-accounts list \
  --filter="displayName='Compute Engine default service account'" \
  --format='value(email)' --project=$PROJECT_ID)

gcloud secrets add-iam-policy-binding tailscale-authkey \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/secretmanager.secretAccessor" \
  --project=$PROJECT_ID
```

### 3 — Create the Artifact Registry repository

```bash
gcloud artifacts repositories create socktails \
  --repository-format=docker \
  --location=$REGION \
  --project=$PROJECT_ID
```

Or with make:

```bash
make artifact-registry-create PROJECT_ID=$PROJECT_ID REGION=$REGION
```

### 4 — Build and push the image

```bash
# Authenticate Docker to Artifact Registry
gcloud auth configure-docker ${REGION}-docker.pkg.dev

# Build and push
make docker-push \
  PROJECT_ID=$PROJECT_ID \
  REGION=$REGION \
  TAG=latest
```

### 5 — Create the Cloud Run Job

```bash
gcloud run jobs create socktails \
  --image=${REGION}-docker.pkg.dev/${PROJECT_ID}/socktails/socktails:latest \
  --region=$REGION \
  --project=$PROJECT_ID \
  --task-timeout=14400 \
  --memory=512Mi \
  --cpu=1 \
  --max-retries=0 \
  --set-env-vars="SOCKS_PORT=1080,DURATION=4h" \
  --set-secrets="TAILSCALE_AUTHKEY=tailscale-authkey:latest"
```

Or with make:

```bash
make deploy PROJECT_ID=$PROJECT_ID REGION=$REGION DURATION=4h
```

> **Note:** Set `--task-timeout` to match `DURATION` so Cloud Run terminates the job if the process doesn't exit cleanly.

### 6 — Execute the job

```bash
gcloud run jobs execute socktails \
  --region=$REGION \
  --project=$PROJECT_ID
```

Or:

```bash
make execute PROJECT_ID=$PROJECT_ID REGION=$REGION
```

### 7 — Find the proxy's Tailscale IP

On your laptop:

```bash
tailscale status
```

Look for the node named `socktails` (or your custom `TS_HOSTNAME`). Its address will be a `100.x.y.z` IP.

### 8 — Configure your browser

#### Firefox

1. Preferences → General → Network Settings → Manual proxy configuration.
2. SOCKS Host: `<tailscale-ip>`, Port: `1080`, SOCKS v5.
3. Check **Proxy DNS when using SOCKS v5**.

#### Chrome / Chromium

Launch with a flag (browser-only, no system proxy change):

```bash
google-chrome \
  --proxy-server="socks5://<tailscale-ip>:1080" \
  --host-resolver-rules="MAP * ~DIRECT, EXCLUDE localhost"
```

#### curl (quick test)

```bash
curl --proxy socks5h://<tailscale-ip>:1080 https://ifconfig.me
```

The returned IP should be from the Cloud Run region.

---

## Local development

```bash
export TAILSCALE_AUTHKEY=tskey-auth-...

# Build and run directly
make run

# Or just build
make build
./bin/socktails --port=1080 --duration=1h
```

---

## Make targets

```
make build                  – compile local binary
make run                    – build and run locally
make docker-build           – build Docker image
make docker-push            – build and push image to Artifact Registry
make artifact-registry-create – create Artifact Registry repo (one-time)
make deploy                 – create/update Cloud Run Job
make execute                – trigger a job run
make clean                  – remove build artifacts
make help                   – show available targets
```

---

## Notes

### Cost & free tier

- Cloud Run Jobs are billed per CPU/memory second while running; a 4-hour job with 1 vCPU / 512 MiB costs roughly **$0.06** (as of 2025 pricing).
- Tailscale's free tier supports up to 3 users and 100 devices — more than sufficient for personal use.
- Artifact Registry: the first 0.5 GB/month is free; the image is typically < 20 MB.

### Timeouts

- Set `--task-timeout` (Cloud Run) ≥ `DURATION` so Cloud Run doesn't kill the job before it exits cleanly.
- The proxy sends `SIGTERM` to itself when the duration elapses; Cloud Run will also send `SIGTERM` when `--task-timeout` is reached.

### Auth key security

- Store the key in **Secret Manager**, not in environment variables or source code.
- Use an **ephemeral** key so nodes that don't de-register cleanly are automatically removed after ~30 minutes of inactivity.
- Rotate the key regularly via the Tailscale admin console.

### Tailscale ACLs (optional)

Add to your `tailscale/acls` policy to restrict which devices can reach the proxy:

```json
{
  "tagOwners": {
    "tag:cloudrun-jobs": ["autogroup:admin"]
  },
  "acls": [
    {
      "action": "accept",
      "src":    ["autogroup:member"],
      "dst":    ["tag:cloudrun-jobs:1080"]
    }
  ]
}
```

### No exit-node support

Cloud Run containers do not have a TUN device, so Tailscale **exit node** mode does not work here. This proxy is a **SOCKS5 proxy only** — configure your browser to use it rather than routing all system traffic through it.
