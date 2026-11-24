VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
# DEPLOY_HOST (user@host) and DEPLOY_DIR come from the environment or a git-ignored local.mk
-include local.mk
DEPLOY_DIR ?= /opt/portmesh
TARGETS := darwin-arm64 linux-amd64 linux-arm64

build:
	@mkdir -p dist/bin
	@for t in $(TARGETS); do \
	  GOOS=$${t%-*} GOARCH=$${t#*-} CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/bin/portmesh-$$t . || exit 1; \
	done

# copies the image inputs to the docker host and rebuilds the controller container; data/ and .env there are never touched
deploy: build
	@test -n "$(DEPLOY_HOST)" || { echo "set DEPLOY_HOST=user@host" >&2; exit 1; }
	ssh $(DEPLOY_HOST) 'mkdir -p $(DEPLOY_DIR)'
	rsync -a dist/bin Dockerfile deploy/docker-compose.yml deploy/.dockerignore $(DEPLOY_HOST):$(DEPLOY_DIR)/
	ssh $(DEPLOY_HOST) 'cd $(DEPLOY_DIR) && docker compose up -d --build'

.PHONY: build deploy
