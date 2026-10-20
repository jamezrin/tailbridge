GO ?= go
BIN := bin

.PHONY: build test integration clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/tailbridge ./cmd/tailbridge

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

clean:
	rm -rf $(BIN)
