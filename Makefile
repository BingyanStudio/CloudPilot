.PHONY: install dev frontend-dev build test worker-image clean

install:
	go mod download
	pnpm --dir frontend install --frozen-lockfile

dev:
	go run ./cmd/cloudpilot -config config.yaml

frontend-dev:
	pnpm --dir frontend dev

build:
	pnpm --dir frontend build
	mkdir -p bin
	go build -trimpath -o bin/cloudpilot ./cmd/cloudpilot

test:
	go test ./...
	go vet ./...
	pnpm --dir frontend build

worker-image:
	docker build -t cloudpilot-codex:latest deploy/worker

clean:
	rm -rf bin frontend/dist
