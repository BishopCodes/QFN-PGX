# QFN-PGX — build/test/deploy helpers (make -j)
BIN     := bin/qfn
MODULE  := github.com/BishopCodes/qfn-pgx
VERSION ?= $(shell git -c safe.directory='*' describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOENV   := CGO_ENABLED=0

.PHONY: build install test vet race arm64 deploy webcss clean fmt

build:
	$(GOENV) go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/qfn

# put qfn on PATH (default /usr/local/bin needs sudo; no sudo? PREFIX=~/.local/bin)
PREFIX ?= /usr/local/bin
install: build
	install -Dm0755 $(BIN) $(DESTDIR)$(PREFIX)/qfn

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

# rebuild the embedded console stylesheet (dev machine only — the built
# style.css is committed so the Spark's `make build` needs no tooling).
TAILWIND ?= $(if $(wildcard build/tailwindcss),build/tailwindcss,build/tailwindcss-download)
webcss: build/tailwindcss
	./build/tailwindcss -c tailwind.config.cjs -i web/src/input.css -o web/style.css --minify

build/tailwindcss:
	@mkdir -p build
	curl -sL --fail https://github.com/tailwindlabs/tailwindcss/releases/download/v3.4.17/tailwindcss-linux-x64 -o $@.tmp
	chmod +x $@.tmp && mv $@.tmp $@

# rebuild the vendored chart bundle (dev machine only; output committed).
# usage: cd a temp dir, `npm i @tanstack/charts@0.16.0 esbuild`, then
#   npx esbuild web/src/chart-glue.mjs --bundle --format=iife --minify \
#       --target=es2020 --outfile=web/vendor/tanstack-charts.iife.js
chartjs:
	@echo "see recipe comment in Makefile (needs npm on the dev box)"

clean:
	rm -rf bin
