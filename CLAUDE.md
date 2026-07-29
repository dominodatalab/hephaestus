# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Hephaestus is a Kubernetes controller that builds and pushes OCI images using BuildKit, driven by
custom resources (`ImageBuild`, `ImageCache`). It leases pods from a BuildKit `StatefulSet`, dispatches
builds to them over the BuildKit gRPC API, and reports build status/metrics back through the CR and
(optionally) AMQP messages.

## Common commands

```bash
make build          # Build hephaestus-controller binary (./cmd/controller)
make unit           # go test -race ./... (excludes integration)
make integration    # envtest-based integration suite (requires `make tools` once; tag: integration)
make test           # unit + integration
make lint           # golangci-lint run (uses .golangci.yml)
make check          # regenerate code, then fail if anything is out of date (used in CI as a drift check)
make tools          # installs Go tool binaries listed in tools/tools.go (controller-gen, golangci-lint, etc.)
```

Run a single unit test:
```bash
go test ./pkg/buildkit/worker/... -run TestName -v
```

Run a single integration test (requires the `integration` build tag and envtest binaries from `make tools`):
```bash
ENVTEST_K8S_VERSION=1.31.0 go test -tags integration ./test/integration/... -run TestName -v
```

Code generation (run after changing API types in `pkg/api/hephaestus/v1`):
```bash
make api      # controller-gen deepcopy/object code -> zz_generated.deepcopy.go
make crds     # controller-gen CRD YAML -> deployments/crds
make client   # generate typed clientset in pkg/clientset
make openapi  # generate OpenAPI defs -> zz_generated.openapi.go
make compiled # all of the above
make sdks     # generate non-Go client libraries (e.g. sdks/java) from the OpenAPI spec
```

`make check` runs `goimports`, `go fmt`, `go mod tidy` (root + `tools/` + `tools/testenv/`), and then
asserts the git tree is clean — so any generated-code or dependency drift must be committed.

## Module layout

This repo has **three separate Go modules**: the root module, `tools/` (pins tool binary versions),
and `tools/testenv/` (cloud test-cluster provisioning for functional tests). `test/functional` is
part of the root module but is gated behind manual CI dispatch, not `make test`.

## Architecture

### Controller framework

Reconcilers are built with `github.com/dominodatalab/controller-util/core`, an in-house wrapper
around controller-runtime. Each controller is a `core.Reconciler` composed of one or more
**Components** — small units implementing `Initialize`/`Reconcile` against a shared `*core.Context`
(carries the client, scheme, config, and a logger). This is why reconciliation logic lives under
`pkg/controller/<resource>/component/` rather than directly in the top-level reconciler file, and why
finding the actual business logic for a resource means starting at `pkg/controller/<resource>/<resource>.go`
(the `Register` function wiring components together) and following into `component/`.

### Request flow (ImageBuild)

1. `pkg/controller/imagebuild/imagebuild.go` registers the `ImageBuild` reconciler with a single
   component, `BuildDispatcher` (`pkg/controller/imagebuild/component/builddispatcher.go`), plus a
   separate GC component (`ImageBuildGC`) and a second, independent reconciler
   (`RegisterImageBuildDelete`) that only exists to catch deletes and broadcast them over a channel so
   in-flight builds can be cancelled.
2. `BuildDispatcher.Reconcile` drives the whole build lifecycle for a single `ImageBuild`: reads
   referenced secrets (`pkg/controller/support/secrets`), persists/validates registry credentials
   (`pkg/controller/support/credentials`), leases a BuildKit worker from the pool
   (`pkg/buildkit/worker`), builds a `buildkit.Client` (`pkg/buildkit`), runs the build, resolves the
   pushed image to populate status (size/digest/labels), and transitions phases via
   `pkg/controller/support/phase`.
3. Phase transitions (`Initializing` -> `Running` -> `Succeeded`/`Failed`) are tracked both as
   Kubernetes `Conditions` and as an explicit `Status.Transitions` history on the CR, and mirrored into
   the `hephaestus_imagebuild_phase_total` Prometheus counter
   (`pkg/controller/imagebuild/component/metrics.go`) — counted only after the status write durably
   succeeds, so a reconcile retry never double-counts.
4. `ImageBuildMessage` (`pkg/controller/imagebuildmessage`) is a separate controller/CR whose
   `amqpmessenger` component republishes build phase changes onto AMQP for external consumers — it is
   independent of the AMQP client wiring in `pkg/messaging`.

### BuildKit worker pool (`pkg/buildkit/worker`)

`AutoscalingPool` treats a BuildKit `StatefulSet`'s pods as a leasable resource pool rather than
talking to a fixed BuildKit daemon: it watches pod/annotation state, queues lease requests
(`RequestQueue`), decides scale-up/down via `ScaleArbiter` (idle-timeout based teardown), and resolves
a routable pod address by watching `EndpointSlices` directly rather than trusting DNS/Service caching.
Leasing is implemented via annotation server-side-apply on the Pod (`leased-at`/`leased-by`/
`manager-identity`/`expiry-time`), not via a separate CRD or lock object — this lets multiple
controller replicas coordinate without a dedicated leader for pool state.

### BuildKit client (`pkg/buildkit`)

Wraps `github.com/moby/buildkit` client to run a build/solve request against a leased worker,
including remote-context or inline-Dockerfile builds, cloud-registry auth refresh
(`NewRefreshingAuthProvider`, backed by `pkg/controller/support/credentials`'s cloud provider
registry for ACR/ECR/GCR short-lived tokens), and optional mTLS to the daemon.

### API types (`pkg/api/hephaestus/v1`)

Hand-written types (`imagebuild_types.go`, `imagecache_types.go`, `imagebuildmessage_types.go`) plus
generated code (`zz_generated.*.go` — do not hand-edit, regenerate via `make api`/`make openapi`).
Admission webhooks live alongside the types (`imagebuild_webhook.go`, `imagecache_webhook.go`) and are
registered directly on the controller-runtime webhook server in `pkg/controller/start.go` rather than
via kubebuilder-marker-generated manifests — webhook paths are also manually listed in the `/debugz`
handler there, so add new ones in both places.

### Configuration

The controller is configured entirely from a YAML file (default `hephaestus.yaml`), loaded and
validated by `pkg/config`. `config.Controller.Validate()` is the source of truth for what's
required — check it before assuming a field is optional.

### Testing structure

- `pkg/**/*_test.go` — standard unit tests, run via `make unit`.
- `test/integration` (build tag `integration`) — boots the real controller entrypoint
  (`controller.Start`) against an `envtest` API server to catch startup/wiring regressions that
  unit tests miss (e.g. webhook registration). Run via `make integration`.
- `test/functional` (own Go module, build tag-free but excluded from `make test`) — provisions real
  AKS/EKS/GKE clusters via `tools/testenv` and runs an end-to-end image build against them; triggered
  manually in CI via a `/functional-test` PR comment, not on every push.

### Deployment artifacts

`deployments/helm/hephaestus` is the Helm chart; `deployments/crds` holds generated CRD YAML consumed
by both the chart and `make apply`/`make delete`. `sdks/java` is a generated client library published
to Artifactory/GitHub Packages from the OpenAPI spec — do not hand-edit generated files under it.
