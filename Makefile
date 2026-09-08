# claude-scheduler build and packaging.

VERSION    ?= 0.1.0
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH       := $(shell dpkg --print-architecture)
MAINTAINER ?= raulsh <54923646+raulsh@users.noreply.github.com>

PKG   := claude-scheduler
BIN   := bin/$(PKG)
STAGE := build/deb
DEB   := dist/$(PKG)_$(VERSION)_$(ARCH).deb

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# The frontend build output is embedded by internal/webui.
UI_DIST := internal/webui/dist

.PHONY: all build ui test vet fmt check deb clean dev run install-local uninstall-local help

all: build

## build: compile the static binary (frontend must already be built)
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/claude-scheduler
	@echo "built $(BIN) ($(VERSION))"

## ui: build the React SPA into the embedded asset directory
ui:
	@command -v node >/dev/null || { echo "node is required to build the UI"; exit 1; }
	@node -e 'const [maj,min]=process.versions.node.split(".").map(Number); \
		if (maj<20 || (maj===20 && min<19)) { \
			console.error("Node >=20.19 required for the Vite build, found "+process.versions.node); \
			process.exit(1); }'
	cd web && npm install --no-audit --no-fund && npm run build
	@echo "built $(UI_DIST)"

## test: run the Go test suite
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -l -w ./cmd ./internal

## check: fmt verification, vet and tests - what CI should run
check:
	@unformatted=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test ./...

## deb: build the installable Debian package
deb: build
	@rm -rf $(STAGE)
	@mkdir -p $(STAGE)/DEBIAN dist
	install -D -m 0755 $(BIN) $(STAGE)/usr/bin/$(PKG)
	install -D -m 0644 packaging/systemd/$(PKG).service $(STAGE)/lib/systemd/system/$(PKG).service
	install -D -m 0644 packaging/config.yaml $(STAGE)/etc/$(PKG)/config.yaml
	install -D -m 0644 packaging/debian/conffiles $(STAGE)/DEBIAN/conffiles
	install -D -m 0755 packaging/debian/postinst $(STAGE)/DEBIAN/postinst
	install -D -m 0755 packaging/debian/prerm    $(STAGE)/DEBIAN/prerm
	install -D -m 0755 packaging/debian/postrm   $(STAGE)/DEBIAN/postrm
	@size=$$(du -sk $(STAGE) | cut -f1); \
	sed -e 's|@VERSION@|$(VERSION)|' \
	    -e 's|@ARCH@|$(ARCH)|' \
	    -e 's|@MAINTAINER@|$(MAINTAINER)|' \
	    -e "s|@SIZE@|$$size|" \
	    packaging/debian/control.in > $(STAGE)/DEBIAN/control
	dpkg-deb --build --root-owner-group $(STAGE) $(DEB)
	@echo
	@dpkg-deb --info $(DEB) | head -12
	@echo "package: $(DEB)"

## dev: run the Vite dev server against a locally running scheduler
dev:
	cd web && npm run dev

## run: run the scheduler against a local config, no install needed
run: build
	./$(BIN) serve --config ./dev-config.yaml --log-level debug

## install-local: build and install the .deb on this machine
install-local: deb
	sudo dpkg -i $(DEB)
	@echo
	systemctl --no-pager status $(PKG) || true

## uninstall-local: remove the package, keeping task definitions and history
uninstall-local:
	sudo dpkg -r $(PKG)

## clean: remove build artifacts
clean:
	rm -rf bin build dist

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
