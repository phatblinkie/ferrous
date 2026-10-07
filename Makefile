VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fixtures test vet fmt run clean image image-test release install-test

build:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/ferrous-agent ./cmd/ferrous-agent

# fakectorn: fake WebRCON server for dev/test (see cmd/fakectorn)
fixtures:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/fakectorn ./cmd/fakectorn

# release artifacts (linux amd64/arm64 tarballs + install.sh) — attach dist/* to
# a GitHub release; the one-liner in install.sh downloads from there
release:
	rm -rf dist && mkdir -p dist
	@for arch in amd64 arm64; do \
	  mkdir -p dist/.stage-$$arch; \
	  (cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o ../dist/.stage-$$arch/ferrous-agent ./cmd/ferrous-agent) || exit 1; \
	  tar -C dist/.stage-$$arch -czf dist/ferrous-agent_linux_$$arch.tar.gz ferrous-agent; \
	  rm -rf dist/.stage-$$arch; \
	done
	cp install.sh dist/
	@ls -la dist

# the game server image (installs Steam app 258550 into /server on first boot)
image:
	docker build -t ferrous/rustserver:latest image

# offline entrypoint behavior tests (stub game, no network, no docker)
image-test:
	bash image/tests/test_entrypoint.sh

# hermetic installer tests (fake prefix, no systemd, no network)
install-test:
	bash tests/test_install.sh

test:
	cd agent && go test ./...

vet:
	cd agent && go vet ./...

fmt:
	cd agent && gofmt -w .

run: build
	./bin/ferrous-agent

clean:
	rm -rf bin dist
