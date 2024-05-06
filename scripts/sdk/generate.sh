#!/usr/bin/env bash
#
# Generates the Java SDK for Hephaestus API. This should probably be moved into
# a Docker image and possibly a separate repository.
#
# Prerequisites:
# - docker
# - kind
# - kubectl
# - golang

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PROJECT_DIR=$(cd "$SCRIPT_DIR"/../.. && pwd)
SDKS_DIR=$(cd "$PROJECT_DIR"/sdks && pwd)
GEN_DIR="$SDKS_DIR/gen"
JAVA_DIR="$SDKS_DIR/java"

KUBERNETES_SWAGGER_FILE=/tmp/dist.swagger.json
SWAGGER_FILE=api/openapi-spec/swagger.json

OPENAPI_GENERATOR_CLI_VERSION=v5.2.1

info() {
	echo -e "\033[0;32m[sdk-generate]\033[0m INFO: $*"
}

error() {
	echo -e "\033[0;31m[sdk-generate]\033[0m ERROR: $*"
}

generate_kubernetes_swagger() {
	info "Creating kind cluster"
	kind create cluster --config "$SCRIPT_DIR"/kind.yaml

	info "Apply CRDs to cluster"
	kubectl apply -f "$PROJECT_DIR"/deployments/crds/
	sleep 5

	info "Fetching Kubernetes OpenAPI specification"
	kubectl get --raw="/openapi/v2" >$KUBERNETES_SWAGGER_FILE

	info "Verifying CRD installation"
	kubectl get crd -o name |
		while read -r L; do
			if [[ $(kubectl get "$L" -o jsonpath='{.status.conditions[?(@.type=="NonStructuralSchema")].status}') == "True" ]]; then
				error "$L failed publishing openapi schema because it's attached non-structral-schema condition."
				kind delete cluster
				exit 1
			fi
			if [[ $(kubectl get "$L" -o jsonpath='{.spec.preserveUnknownFields}') == "true" ]]; then
				error "$L failed publishing openapi schema because it explicitly disabled unknown fields pruning."
				kind delete cluster
				exit 1
			fi
			info "$L successfully installed"
		done

	info "Cleaning up kind cluster"
	kind delete cluster
}

GIT_TAG=$(git describe --tags --candidates=0 --abbrev=0 2>/dev/null || echo untagged)
if [[ $GIT_TAG == "untagged" ]]; then
	VERSION="${BRANCH_NAME:-0.0.0}-SNAPSHOT"
else
	VERSION="${GIT_TAG#v}"
fi
info "Creating SDK version: $VERSION"

generate_kubernetes_swagger

info "Generating Swagger JSON"
go run "$SCRIPT_DIR"/main.go -json $KUBERNETES_SWAGGER_FILE -version "$VERSION" >$SWAGGER_FILE

info "Generating Java client library"
mkdir -p "$GEN_DIR"
docker run -q --user "$(id -u):$(id -g)" --rm -v "$PROJECT_DIR:/wd" --workdir /wd \
	openapitools/openapi-generator-cli:$OPENAPI_GENERATOR_CLI_VERSION generate \
	--input-spec /wd/$SWAGGER_FILE \
	--generator-name java \
	--output /wd/sdks/gen \
	--api-package com.dominodatalab.hephaestus.v1.apis \
	--model-package com.dominodatalab.hephaestus.v1.models \
	--invoker-package io.kubernetes.client.openapi \
	--group-id com.dominodatalab.hephaestus \
	--artifact-id hephaestus-client-java \
	--additional-properties dateLibrary=java8 \
	--import-mappings v1.Condition=io.kubernetes.client.openapi.models.V1Condition \
	--import-mappings v1.ObjectMeta=io.kubernetes.client.openapi.models.V1ObjectMeta \
	--import-mappings v1.ListMeta=io.kubernetes.client.openapi.models.V1ListMeta \
	--import-mappings v1.Patch=io.kubernetes.client.custom.V1Patch \
	--import-mappings v1.DeleteOptions=io.kubernetes.client.openapi.models.V1DeleteOptions \
	--import-mappings v1.Status=io.kubernetes.client.openapi.models.V1Status \
	--import-mappings v1.Time=java.time.OffsetDateTime \
	--http-user-agent "Hephaestus Java Client/$VERSION" \
	--generate-alias-as-model

info "Copying generated Java files to $JAVA_DIR"
rm -rf "$GEN_DIR"/src/main/{java/io,AndroidManifest.xml}
cp -r "$GEN_DIR"/docs "$JAVA_DIR"
cp -r "$GEN_DIR"/src "$JAVA_DIR"
rm -rf "$GEN_DIR"

info "Copying Maven configurations"
sed "s/0.0.0-VERSION/$VERSION/" "$SCRIPT_DIR"/pom.xml >"$JAVA_DIR"/pom.xml
cp "$SCRIPT_DIR"/settings.xml "$JAVA_DIR"/settings.xml
