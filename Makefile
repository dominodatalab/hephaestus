.DEFAULT_GOAL:=help
SHELL:=/bin/bash

.PHONY: test tools

##@ Development

build: ## Build controller
	go build -v -o bin/hephaestus-controller ./cmd/controller/

docker: ## Build docker image
	docker build -t ghcr.io/dominodatalab/hephaestus:latest .

test: ## Run test suite
	go test -v -race ./...

lint: tools ## Run linter suite
	golangci-lint run ./...

apply: crds ## Apply CRDs to cluster
	kubectl apply -f deployments/crds

delete: crds ## Delete CRDs from cluster
	kubectl delete -f deployments/crds

##@ Generators

api: tools ## Generate API objects
	controller-gen object paths="./pkg/api/hephaestus/..."

crds: tools ## Generate CRDs
	controller-gen crd paths="./..." output:crd:artifacts:config=deployments/crds

client: tools ## Generate Go client API library
	client-gen --clientset-name "clientset" --input-base "github.com/dominodatalab/hephaestus/pkg/api" --input "hephaestus/v1" --output-package "github.com/dominodatalab/hephaestus/pkg" --go-header-file "$(shell mktemp)"

sdks: crds ## Generate non-GO client libraries
	scripts/sdk/generate.sh

##@ Misc

tools: ## Install go tooling
	@echo Installing tools from tools/tools.go
	@cd tools && go list -f '{{range .Imports}}{{.}}{{"\n"}}{{end}}' tools.go | xargs -I % go install %

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
