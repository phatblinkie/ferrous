VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fixtures test vet fmt run clean

build:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/ferrous-agent ./cmd/ferrous-agent

# fakectorn: fake WebRCON server for dev/test (see cmd/fakectorn)
fixtures:
	cd agent && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/fakectorn ./cmd/fakectorn

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
