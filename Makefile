BINARY  := hole-balancer
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race lint vet fmt cover clean install uninstall run quickstart

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

# Install as a systemd service. Safe to re-run: an existing
# /etc/hole-balancer/config.yaml is never overwritten, so upgrading is just
# `git pull && sudo make install`.
#
# Pass CONFIG= to seed the config from a file you already have, which is what
# you want straight after trying it with quickstart.sh:
#   sudo make install CONFIG=config.yaml
install: build
	@deploy/install.sh $(if $(CONFIG),--config $(CONFIG))

# Removes the service, binary, and account. The configuration is deliberately
# left behind — reinstalling should not mean rebuilding your pool by hand.
uninstall:
	-systemctl disable --now $(BINARY) 2>/dev/null
	rm -f /etc/systemd/system/$(BINARY).service /usr/local/bin/$(BINARY)
	-systemctl daemon-reload 2>/dev/null
	-userdel $(BINARY) 2>/dev/null
	@echo "Removed. Configuration left in /etc/hole-balancer/ — delete it by hand if you want it gone."

# Run against config.yaml on high ports, no privileges needed.
run: build
	./$(BINARY) -config config.yaml

# Build, configure, and start in one step. See QUICKSTART.md.
quickstart:
	./quickstart.sh

clean:
	rm -rf $(BINARY) dist coverage.out
