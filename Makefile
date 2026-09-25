VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist test vet clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o logviewer ./cmd/logviewer

# статические бинарники для релиза
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/logviewer-linux-amd64 ./cmd/logviewer
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/logviewer-linux-arm64 ./cmd/logviewer
	cd dist && sha256sum logviewer-* > SHA256SUMS

test: vet
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf logviewer dist
