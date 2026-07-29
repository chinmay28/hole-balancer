BINARY  := hole-balancer
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race lint vet fmt cover clean install run quickstart

all: fmt vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/hole-balancer

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Cross-compile for the usual home-server targets.
dist: clean
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 linux/arm darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/hole-balancer; \
	done

install: build
	install -Dm755 $(BINARY) /usr/local/bin/$(BINARY)
	install -Dm644 config.example.yaml /etc/hole-balancer/config.yaml
	install -Dm644 deploy/hole-balancer.service /etc/systemd/system/hole-balancer.service
	setcap cap_net_bind_service=+ep /usr/local/bin/$(BINARY)
	@echo "Edit /etc/hole-balancer/config.yaml, then: systemctl enable --now hole-balancer"

# Run against config.yaml on high ports, no privileges needed.
run: build
	./$(BINARY) -config config.yaml

# Build, configure, and start in one step. See QUICKSTART.md.
quickstart:
	./quickstart.sh

clean:
	rm -rf $(BINARY) dist coverage.out
