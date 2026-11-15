MODULE  := github.com/Opsmastery-Summit/tvt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build test race vet fmt fmt-check lint clean hooks e2e bench bench-update

all: lint test build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/tvt ./cmd/tvt

test:
	go test ./...

race:
	go test -race ./...

# The ground-truth benchmark (S5 item 1): scores evidence found and
# recommendation precision and recall on the frozen lab states in
# testdata/bench, and fails on any drop below testdata/bench/baseline.json.
# The scale regression (S5 item 7) fails when a 200k-series scan's
# allocation or peak heap more than doubles against testdata/bench/scale.json.
bench:
	go test -count=1 -run '^(TestBenchmark|TestScanScaleRegression)$$' -v ./cmd/tvt

# Re-record the baseline after a deliberate change; commit the diff.
bench-update:
	go test -count=1 -run '^(TestBenchmark|TestScanScaleRegression)$$' ./cmd/tvt -update-bench

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint: fmt-check vet

# Live-lab end-to-end suite (build tag e2e; CI and the pre-push hook skip
# it). Needs the tvt-lab running beside this checkout and TVT_GRAFANA_TOKEN
# in the environment. Exports Grafana's log first so loads are current.
LAB_DIR ?= ../tvt-lab
e2e:
	$(MAKE) -C "$(LAB_DIR)" export-logs
	TVT_LAB_DIR="$$(cd "$(LAB_DIR)" && pwd)" go test -tags e2e -count=1 -timeout 20m -v -run E2E ./cmd/tvt

# Git hooks in .githooks: pre-commit (blocked files, gofmt, vet, go mod
# tidy, schema tests, gitleaks), pre-push (go test), post-checkout and
# post-merge (rebuild bin/tvt).
hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf bin dist
