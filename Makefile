VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fixtures test vet fmt run clean image image-test

build:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/ferrous-agent ./cmd/ferrous-agent

# fakectorn: fake WebRCON server for dev/test (see cmd/fakectorn)
fixtures:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/fakectorn ./cmd/fakectorn

# the game server image (installs Steam app 258550 into /server on first boot)
image:
	docker build -t ferrous/rustserver:latest image

# offline entrypoint behavior tests (stub game, no network, no docker)
image-test:
	bash image/tests/test_entrypoint.sh

test:
	cd agent && go test ./...

vet:
	cd agent && go vet ./...

fmt:
	cd agent && gofmt -w .

run: build
	./bin/ferrous-agent

clean:
	rm -rf bin
