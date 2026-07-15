.PHONY: build build-init build-image-init build-gadget test vet fmt

build:
	go build -o bin/teslabox ./cmd/teslabox

build-init:
	go build -o bin/teslabox-init ./cmd/teslabox-init

build-image-init:
	go build -o bin/teslabox-image-init ./cmd/teslabox-image-init

build-gadget:
	go build -o bin/teslabox-gadget ./cmd/teslabox-gadget

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w cmd internal
