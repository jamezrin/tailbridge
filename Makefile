GO ?= go
BIN := bin

.PHONY: build install test integration snapshot package-test clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/tailbridge ./cmd/tailbridge

# Manual install: PREFIX=/usr/local by default; the unit goes to /etc/systemd/system.
PREFIX ?= /usr/local
SYSTEMDUNITDIR ?= /etc/systemd/system

install: build
	install -Dm755 $(BIN)/tailbridge $(DESTDIR)$(PREFIX)/bin/tailbridge
	install -d $(DESTDIR)$(SYSTEMDUNITDIR)
	sed 's|^ExecStart=.*/tailbridge |ExecStart=$(PREFIX)/bin/tailbridge |' contrib/systemd/tailbridge.service \
		> $(DESTDIR)$(SYSTEMDUNITDIR)/tailbridge.service

test:
	$(GO) vet ./...
	$(GO) test ./...

# End-to-end tests in a privileged container with its own network namespace.
# They never use host networking and never talk to the host's tailscaled.
integration:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/tailbridge ./cmd/tailbridge
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/fakets ./test/fakets
	docker build -q -t tailbridge-it test/integration
	docker run --rm --privileged --network none \
		-v $(CURDIR)/$(BIN):/tb:ro -v $(CURDIR)/test/integration:/it:ro \
		tailbridge-it bash /it/run.sh

# Release artifacts (.deb, .rpm, tarballs) without publishing, then install
# them in clean Debian, Ubuntu, Fedora and Alpine containers.
snapshot:
	goreleaser release --snapshot --clean

package-test:
	test/packaging/smoke.sh dist

clean:
	rm -rf $(BIN) dist
