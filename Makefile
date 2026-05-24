.PHONY: build run devcontainer worker-image

build:
	go build -o otto ./cmd/otto

run: build
	./otto serve

devcontainer:
	devcontainer build --workspace-folder .

worker-image:
	docker build -f Dockerfile.worker -t otto-worker .
