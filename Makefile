.DEFAULT_GOAL := help

GO ?= go
IMAGE ?= futureq
TAG ?= local
ARGS ?=

.PHONY: help build run test docker-build

help:
	@printf '%s\n' \
	  'make build         Build bin/futureq' \
	  'make run           Build and start a local node (ARGS="-c config.yaml")' \
	  'make test          Run application tests with the race detector' \
	  'make docker-build  Build a local image (IMAGE=futureq TAG=local)'

build:
	@mkdir -p bin
	$(GO) build -mod=readonly -trimpath -o bin/futureq ./cmd/futureq

run: build
	./bin/futureq start $(ARGS)

test:
	$(GO) test -mod=readonly -race ./cmd/... ./internal/... ./pkg/...

docker-build:
	docker build --tag "$(IMAGE):$(TAG)" .
