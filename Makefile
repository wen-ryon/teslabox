.PHONY: build build-init build-image-init build-gadget build-acceptance test vet fmt

build:
	go build -o bin/teslabox ./cmd/teslabox

build-init:
	go build -o bin/teslabox-init ./cmd/teslabox-init

build-image-init:
	go build -o bin/teslabox-image-init ./cmd/teslabox-image-init

build-gadget:
	go build -o bin/teslabox-gadget ./cmd/teslabox-gadget

build-acceptance:
	go build -o bin/teslabox-acceptance ./cmd/teslabox-acceptance

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w cmd internal
