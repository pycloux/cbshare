BINARY  := cbshare
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist

PLATFORMS := linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: all build test vet fmt cross clean e2e

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cbshare

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

cross:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)-$(VERSION)-$$os-$$arch$$ext ./cmd/cbshare || exit 1; \
	done

# External tests (tests/e2e): P1 cases against a fresh build. Needs the
# Python environment described in tests/e2e/README.md.
e2e: build
	cd tests/e2e && CBSHARE_BIN=$(CURDIR)/$(BINARY) pytest -m p1 -n auto

clean:
	rm -rf $(BINARY) $(BINARY).exe $(DIST)
