# steam-audio-router — static Go binary, no runtime dependency besides
# pw-dump/pw-link (part of PipeWire). Only Go is needed to build.
BIN      := steam-audio-router
BUILDDIR := release
OUT      := $(BUILDDIR)/$(BIN)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOBUILD  := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test integration vet check install enable uninstall dry-run clean

build:            ## build $(OUT) for this machine
	@mkdir -p $(BUILDDIR)
	$(GOBUILD) -o $(OUT) .

test:             ## unit tests (fake /proc, fake PipeWire graph)
	go test ./...

integration: build ## end-to-end test on the real PipeWire daemon (temporary nodes only)
	tests/integration.sh $(OUT)

vet:
	go vet ./...

check: vet test build

install: build    ## install binary + config + service (not started)
	$(OUT) install

enable: build     ## install, enable and start the service
	$(OUT) install --enable

uninstall:
	$(OUT) uninstall 2>/dev/null || $(HOME)/.local/bin/$(BIN) uninstall

dry-run: build    ## evaluate the current streams without linking anything
	$(OUT) once --dry-run

clean:
	rm -rf $(BUILDDIR)
