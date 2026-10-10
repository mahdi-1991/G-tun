VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
CMDS    := g-tun-server g-tun-client g-tun-gencert

.PHONY: all build test vet fmt lint clean

all: fmt vet test build

build:
	@mkdir -p bin
	@for cmd in $(CMDS); do \
		go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$cmd ./cmd/$$cmd || exit 1; \
	done

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

lint:
	shellcheck install.sh g-tun.sh

clean:
	rm -rf bin
