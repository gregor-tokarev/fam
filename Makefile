VERSION ?= 0.14.0
LDFLAGS := -X github.com/gregor-tokarev/fam/internal/version.Version=$(VERSION)

.PHONY: build pklgen

build:
	go build -ldflags "$(LDFLAGS)" -o fam ./cmd/fam

pklgen:
	./scripts/pklgen.sh
