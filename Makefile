VERSION ?= 0.1.0
LDFLAGS := -X github.com/gregor-tokarev/fam/internal/version.Version=$(VERSION)

.PHONY: build skills pklgen

build: skills
	go build -ldflags "$(LDFLAGS)" -o fam ./cmd/fam

skills:
	./scripts/sync-std-skills.sh

pklgen:
	./scripts/pklgen.sh
