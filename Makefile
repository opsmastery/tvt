MODULE  := github.com/Opsmastery-Summit/tvt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build test race vet fmt fmt-check lint clean hooks e2e bench bench-update kps-fixtures

all: lint test build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/tvt ./cmd/tvt

test:
	go test ./...

# cmd/tvt runs about 90 s plainly and near 18 minutes under -race on CI
# runners, past Go's default timeout, so race gets room.
race:
	go test -race -timeout 40m ./...

# The ground-truth benchmark (S5 item 1): scores evidence found and
# recommendation precision and recall on the frozen lab states in
# testdata/bench, and fails on any drop below testdata/bench/baseline.json.
# The scale regression (S5 item 7) fails when a 200k-series scan's
# allocation or peak heap more than doubles against testdata/bench/scale.json.
bench:
	go test -count=1 -run '^(TestBenchmark|TestScanScaleRegression)$$' -v ./cmd/tvt

# kube-prometheus-stack rendered at a pinned chart version (S5 item 8;
# docs/tasks/lab-backlog.md): bump KPS_VERSION only on purpose and review
# the fixture diff. Needs helm and the network; CI only reads the result.
KPS_VERSION ?= 93.3.0
KPS_DIR := testdata/kube-prometheus-stack
kps-fixtures:
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update >/dev/null
	helm repo update prometheus-community >/dev/null
	# Secrets are dropped, with the checksum of the one that changes each
	# render: helm template generates a Grafana password and webhook
	# certificates, and tvt never reads a Secret.
	helm template kps prometheus-community/kube-prometheus-stack --version $(KPS_VERSION) --namespace monitoring | \
		python3 -I -c 'import re,sys; docs=re.split(r"(?m)^---\n", sys.stdin.read()); out="".join("---\n"+d for d in docs if d.strip() and not re.search(r"(?m)^kind: Secret$$", d)); sys.stdout.write(re.sub(r"(checksum/secret: ).*", r"\1dropped-with-the-secret", out))' > $(KPS_DIR)/render.yaml
	helm show values prometheus-community/kube-prometheus-stack --version $(KPS_VERSION) > $(KPS_DIR)/values.yaml
	echo "$(KPS_VERSION)" > $(KPS_DIR)/VERSION

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
