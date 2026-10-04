RUN_ID ?=
UPSTREAM_REPO ?= ../go-lru
export RUN_ID UPSTREAM_REPO

.PHONY: build test race vet benchmark
build:
	go build -o bin/lrugcbench ./cmd/lrugcbench
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...

# Keep the whole workflow in one recipe so make -j cannot overlap its stages.
benchmark:
	@bash ./scripts/run-benchmark.sh
