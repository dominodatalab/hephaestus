SHELL:=/bin/bash

##@ Development

.PHONY: build
build: ## Build controller binary
	go build -v -o bin/hephaestus-controller ./cmd/controller/

docker: ## Build docker image
	docker build -t ghcr.io/dominodatalab/hephaestus:latest .

.PHONY: test
test: ## Run test suite
	go test -v -timeout=5m -race ./...

lint: tools ## Run linter suite
	golangci-lint run ./...

apply: crds ## Apply CRDs to cluster
	kubectl apply -f deployments/crds

delete: crds ## Delete CRDs from cluster
	kubectl delete -f deployments/crds

check: compiled ## Ensure generated files and dependencies are up-to-date
	go mod tidy -v
	cd tools && go mod tidy -v
	git update-index --refresh
	git diff-index --exit-code --name-status HEAD

##@ Generators

api: tools ## Generate API objects
	controller-gen object paths="./pkg/api/hephaestus/..."

crds: tools ## Generate CRDs
	controller-gen crd paths="./..." output:crd:artifacts:config=deployments/crds

client: tools ## Generate Go client API library
	client-gen --clientset-name "clientset" --input-base "github.com/dominodatalab/hephaestus/pkg/api" --input "hephaestus/v1" --output-pkg "github.com/dominodatalab/hephaestus/pkg" --output-dir ./pkg --go-header-file "$(shell mktemp)"

compiled: api crds client openapi ## Generate all compiled code

openapi: ## Generate OpenAPI definitions for API types
	go install k8s.io/kube-openapi/cmd/openapi-gen@$(shell go list -m k8s.io/kube-openapi | awk '{print $$2}')
	openapi-gen github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1 \
		--go-header-file "$(shell mktemp)" \
		--output-pkg github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1 \
		--output-file zz_generated.openapi.go \
		--output-dir pkg/api/hephaestus/v1/ \
		--report-filename api/api-rules/violation_exceptions.list

sdks: crds openapi ## Generate non-GO client libraries
	scripts/sdk/generate.sh

##@ Misc

.PHONY: tools
tools: ## Install go tooling
	@echo Installing tools from tools/tools.go
	@cd tools && go list -e -f '{{range .Imports}}{{.}}{{"\n"}}{{end}}' tools.go | xargs -I % go install %

.DEFAULT_GOAL:=help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
