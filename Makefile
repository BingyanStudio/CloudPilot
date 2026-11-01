.PHONY: install dev frontend-dev frontend-build build test worker-image clean

install:
	go mod download
	pnpm --dir frontend install --frozen-lockfile

dev: frontend-build
	go run ./cmd/cloudpilot -config config.yaml

frontend-dev:
	pnpm --dir frontend dev

frontend-build:
	pnpm --dir frontend build

build: frontend-build
	mkdir -p bin
	go build -trimpath -o bin/cloudpilot ./cmd/cloudpilot

test: frontend-build
	go test ./...
	go vet ./...
	pnpm --dir frontend test

worker-image:
	docker build -t cloudpilot-codex:latest deploy/worker

clean:
	rm -rf bin frontend/dist
