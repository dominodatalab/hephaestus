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

PROJECT_NAME=github.com/dominodatalab/hephaestus
API_PACKAGE_PATH=pkg/api/hephaestus/v1
OPENAPI_GEN_PATH=$PROJECT_NAME/$API_PACKAGE_PATH
KUBERNETES_SWAGGER_FILE=/tmp/dist.swagger.json
SWAGGER_FILE=$API_PACKAGE_PATH/swagger.json

OPENAPI_GENERATOR_CLI_VERSION=v5.2.1

info () {
  echo -e "\033[0;32m[sdk-generate]\033[0m INFO: $*"
}

error () {
  echo -e "\033[0;31m[sdk-generate]\033[0m ERROR: $*"
}

sdk::generate_kubernetes_swagger () {
  info "Creating kind cluster"
  kind create cluster --config "$SCRIPT_DIR"/kind.yaml

  info "Apply CRDs to cluster"
  kubectl apply -f "$PROJECT_DIR"/deployments/crds/
  sleep 5

  info "Fetching Kubernetes OpenAPI specification"
  kubectl get --raw="/openapi/v2" > $KUBERNETES_SWAGGER_FILE

  info "Verifying CRD installation"
  kubectl get crd -o name \
    | while read -r L
      do
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

GIT_TAG=$(git describe --tags --candidates=0 --abbrev=0 2> /dev/null || echo untagged)
if [[ $GIT_TAG == "untagged" ]]; then
  VERSION=0.0.0-SNAPSHOT
else
  VERSION="${GIT_TAG#v}"
fi
info "Creating SDK version: $VERSION"


info "Ensuring GOPATH is exported"
if [[ -z ${GOPATH:-} ]]; then
  GOPATH=$(go env GOPATH)
  export GOPATH
fi

info "Installing openapi-gen"
OPENAPI_VERSION="$(go list -m k8s.io/kube-openapi | awk '{print $2}')"
go install k8s.io/kube-openapi/cmd/openapi-gen@"$OPENAPI_VERSION"

info "Generating OpenAPI definitions for API types"
openapi-gen \
  --go-header-file "$(mktemp)" \
  --input-dirs $OPENAPI_GEN_PATH \
  --output-package $OPENAPI_GEN_PATH \
  --report-filename $API_PACKAGE_PATH/violation_exceptions.list

sdk::generate_kubernetes_swagger

info "Generating Swagger JSON"
go run "$SCRIPT_DIR"/main.go -json $KUBERNETES_SWAGGER_FILE -version "$VERSION" > $SWAGGER_FILE

info "Generating Java client library"
docker run --rm -v "$PROJECT_DIR:/wd" --workdir /wd \
  openapitools/openapi-generator-cli:$OPENAPI_GENERATOR_CLI_VERSION generate \
    --input-spec /wd/$SWAGGER_FILE \
    --generator-name java \
    --output /wd/sdks/gen \
    --api-package com.dominodatalab.hephaestus.apis \
    --model-package com.dominodatalab.hephaestus.models \
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
    --import-mappings v1.Time=java.time.Instant \
    --http-user-agent "Hephaestus Java Client/$VERSION" \
    --generate-alias-as-model

set -x

chown -R "$(id -u)":"$(id -g)" "$SDKS_DIR"/gen

info "Copying generated Java files to sdk/java"
rm -rf "$SDKS_DIR"/gen/src/main/{java/io,AndroidManifest.xml}
cp -r "$SDKS_DIR"/gen/docs "$SDKS_DIR"/java/
cp -r "$SDKS_DIR"/gen/src "$SDKS_DIR"/java/
rm -rf "$SDKS_DIR"/gen

info "Copying Maven configurations"
sed "s/0.0.0-VERSION/$VERSION/" "$SCRIPT_DIR"/pom.xml > "$SDKS_DIR"/java/pom.xml
cp "$SCRIPT_DIR"/settings.xml "$SDKS_DIR"/java/settings.xml
