.PHONY: build test vet fmt install release clean verify verify-full verify-ci lint doctor hooks

VERSION ?= $(shell node -p "require('./package.json').version" 2>/dev/null || echo 0.1.0)
LDFLAGS := -s -w -X github.com/lifan-builds/quotaswitch/internal/cli.Version=$(VERSION)
PREFIX ?= $(shell [ -d /opt/homebrew/bin ] && echo /opt/homebrew || echo $(HOME)/.local)
BINARY := bin/quotaswitch
HARNESS := go run ./cmd/harness

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/quotaswitch

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

verify:
	$(HARNESS) verify --profile fast

verify-full:
	$(HARNESS) verify --profile full

verify-ci:
	$(HARNESS) verify --profile ci

lint:
	$(HARNESS) lint

doctor:
	$(HARNESS) doctor

hooks:
	$(HARNESS) install-hooks

install:
	mkdir -p $(PREFIX)/bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(PREFIX)/bin/quotaswitch ./cmd/quotaswitch
	ln -sf quotaswitch $(PREFIX)/bin/agy-auth

release:
	@echo "git tag v$(VERSION) && git push origin v$(VERSION)"

clean:
	rm -rf bin dist *.tgz
