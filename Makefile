# SockTails Makefile
# ─────────────────────────────────────────────────────────────────────────────
# Required environment variables (can be set in shell or a .env file):
#   PROJECT_ID   – GCP project ID                 (e.g. my-gcp-project)
#   REGION       – Cloud Run region               (e.g. asia-southeast1)
#   DURATION     – Proxy lifetime                 (e.g. 4h)
#   SOCKS_PORT   – SOCKS5 port inside container   (default: 1080)
#
# Sensitive env vars (never committed):
#   TAILSCALE_AUTHKEY – ephemeral, pre-authorised Tailscale auth key

PROJECT_ID   ?= my-gcp-project
REGION       ?= asia-southeast1
REPO         ?= socktails
IMAGE_NAME   ?= socktails
TAG          ?= latest
SOCKS_PORT   ?= 1080
DURATION     ?= 4h
JOB_NAME     ?= socktails
TASK_TIMEOUT ?= 14400  # seconds (4 h)

REGISTRY     := $(REGION)-docker.pkg.dev/$(PROJECT_ID)/$(REPO)
IMAGE        := $(REGISTRY)/$(IMAGE_NAME):$(TAG)

.PHONY: all build run docker-build docker-push deploy execute clean help

all: build

## build: compile the proxy binary locally
build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o bin/socktails ./cmd/proxy

## run: run the proxy locally (requires TAILSCALE_AUTHKEY to be set)
run: build
	TAILSCALE_AUTHKEY=$(TAILSCALE_AUTHKEY) \
	SOCKS_PORT=$(SOCKS_PORT) \
	DURATION=$(DURATION) \
	./bin/socktails

## docker-build: build the container image
docker-build:
	docker build -t $(IMAGE) .

## docker-push: build and push the image to Artifact Registry
docker-push: docker-build
	docker push $(IMAGE)

## artifact-registry-create: create the Artifact Registry repository (one-time)
artifact-registry-create:
	gcloud artifacts repositories create $(REPO) \
	  --repository-format=docker \
	  --location=$(REGION) \
	  --project=$(PROJECT_ID)

## deploy: create (or update) the Cloud Run Job
deploy:
	gcloud run jobs create $(JOB_NAME) \
	  --image=$(IMAGE) \
	  --region=$(REGION) \
	  --project=$(PROJECT_ID) \
	  --task-timeout=$(TASK_TIMEOUT) \
	  --memory=512Mi \
	  --cpu=1 \
	  --max-retries=0 \
	  --set-env-vars="SOCKS_PORT=$(SOCKS_PORT),DURATION=$(DURATION)" \
	  --set-secrets="TAILSCALE_AUTHKEY=tailscale-authkey:latest" \
	  2>/dev/null \
	|| gcloud run jobs update $(JOB_NAME) \
	  --image=$(IMAGE) \
	  --region=$(REGION) \
	  --project=$(PROJECT_ID) \
	  --task-timeout=$(TASK_TIMEOUT) \
	  --memory=512Mi \
	  --cpu=1 \
	  --max-retries=0 \
	  --set-env-vars="SOCKS_PORT=$(SOCKS_PORT),DURATION=$(DURATION)" \
	  --set-secrets="TAILSCALE_AUTHKEY=tailscale-authkey:latest"

## execute: run the Cloud Run Job (blocks until the job starts)
execute:
	gcloud run jobs execute $(JOB_NAME) \
	  --region=$(REGION) \
	  --project=$(PROJECT_ID)

## clean: remove local build artifacts
clean:
	rm -rf bin/

## help: show this help
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
