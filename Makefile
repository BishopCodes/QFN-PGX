# QFN-PGX — build/test/deploy helpers (make -j)
BIN     := bin/qfn
MODULE  := github.com/BishopCodes/qfn-pgx
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/cli.version=$(VERSION)
GOENV   := CGO_ENABLED=0

.PHONY: build test vet race arm64 deploy clean fmt

build:
	$(GOENV) go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/qfn

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

# cross-compile for the Spark (DGX Spark / GX10 is aarch64)
arm64:
	GOOS=linux GOARCH=arm64 $(GOENV) go build -trimpath -ldflags '$(LDFLAGS)' -o bin/qfn-linux-arm64 ./cmd/qfn

# deploy to the Spark and leave the binary ready to run there (usage: make deploy SPARK=bishop@10.0.0.5)
deploy: arm64
	scp bin/qfn-linux-arm64 $(SPARK):~/.local/bin/qfn
	ssh $(SPARK) 'chmod +x ~/.local/bin/qfn && ~/.local/bin/qfn doctor --quick || true'

clean:
	rm -rf bin
