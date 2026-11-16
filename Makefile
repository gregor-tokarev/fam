VERSION ?= 0.6.0
LDFLAGS := -X github.com/gregor-tokarev/fam/internal/version.Version=$(VERSION)

.PHONY: build pklgen

build:
	go build -ldflags "$(LDFLAGS)" -o fam ./cmd/fam

pklgen:
	./scripts/pklgen.sh
