.PHONY: build run devcontainer worker-image

build:
	go build -o agent ./cmd/agent

run: build
	./agent serve

devcontainer:
	devcontainer build --workspace-folder .

worker-image:
	docker build -f Dockerfile.worker -t agent-worker .
