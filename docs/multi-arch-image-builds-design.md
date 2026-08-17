# Design notes: multi-platform image builds

**Branch:** `feature/multi-arch-image-builds`
**Status:** implemented end-to-end, including Helm multi-`StatefulSet` templating (see
"Addendum" at the end of this document — the original decision to cut that scope has since been
superseded by a follow-up).

This document explains *why* this feature was built the way it was, not just what changed. For
user-facing usage (how to request platforms, how to enable emulation), see
[`building.md`](./building.md).

## Background

Hephaestus dispatches `ImageBuild` resources to a leased BuildKit worker pod and runs whatever
architecture that pod happens to be. There was no way for a user to ask for a specific
platform, and no way to get a single image reference that worked across `linux/amd64` and
`linux/arm64` nodes — the two-platform reality of most Kubernetes fleets today (x86 workers plus a
growing amd64→arm64 migration for cost/power reasons). Anyone who needed a multi-arch image had to
build it outside hephaestus (e.g. `docker buildx`) and push it in separately, defeating the point of
having a build controller at all.

This is unrelated to the pre-existing `linux/arm64` work in this repo's git history (`#139`, `#382`)
— those made the **hephaestus controller's own release image** multi-arch. Nothing in this repo
previously let hephaestus build multi-arch images *on behalf of its users*.

BuildKit itself has supported multi-platform solves for years (`docker buildx build --platform
linux/amd64,linux/arm64` is exactly this). The work here is entirely about exposing that capability
through the `ImageBuild`/`ImageCache` CRD API and the worker-pool/leasing machinery hephaestus
already has, in a way that's opt-in and doesn't disturb any existing deployment.

## Goals

- Let a user request one or more target platforms on an `ImageBuild`.
- Reject unsatisfiable requests at admission time, not by hanging a build against a worker that can
  never produce what was asked for.
- Support both of BuildKit's real deployment patterns: a single worker with QEMU emulation
  registered (works today, on one buildkit pool, zero extra infra), and true native multi-arch
  (real amd64 and arm64 hardware, each in its own pool).
- Zero behavior change for every existing deployment that doesn't opt in.

## Non-goals (explicitly out of scope)

> **Superseded:** the multi-`StatefulSet` templating cut below was implemented in a follow-up once
> additional design decisions were worked out (naming/namespace scheme, which fields are safe to
> override per pool, mTLS/RBAC sharing model, cross-namespace `NetworkPolicy` correctness). See the
> "Addendum" at the end of this document for that follow-up's own decision log — the reasoning
> below for why it was cut *initially* is left intact as the historical record of that call.

- **Templating N buildkit `StatefulSet`s from one Helm values list.** The chart still deploys
  exactly one buildkit `StatefulSet`. Native multi-pool fan-out is implemented at the controller
  level and works if additional pools exist, but this chart version doesn't stand them up for you.
  This was a deliberate scope cut, not an oversight — see "Why the Helm chart stops short of
  multi-StatefulSet templating" below.
- **`ImageCache`'s cache-warming dispatch fanning out across platforms.** `ImageCache` gets the same
  `Platforms` field and the same admission validation for API consistency, but its controller
  (`imagecache.Register`) is already a no-op pending unrelated rework — there was no live dispatch
  path to extend.

## Design overview

Three ways a build can be resolved, chosen automatically based on what's configured:

1. **No `Platforms` requested** → the exact pre-existing single-solve, single-pool path. Untouched
   behavior, byte-for-byte.
2. **`Platforms` requested, all resolvable to one pool** → one BuildKit `Solve()` call with a
   comma-separated `platform` frontend attr. BuildKit does the multi-platform manifest-list
   assembly internally; hephaestus just has to read back the digest correctly.
3. **`Platforms` requested, spanning more than one pool** → the dispatcher runs one single-platform
   solve per (pool, platform) pair concurrently, each against its own leased worker, then manually
   assembles the resulting single-platform images into a real OCI manifest list/index and pushes
   that to the originally-requested tag.

Case 3 exists because a single BuildKit daemon instance can only solve against the workers *it* has,
and hephaestus's worker pool abstraction leases from one `StatefulSet` at a time. If a user's fleet
genuinely has separate native-amd64 and native-arm64 buildkit deployments (as opposed to one pool
with QEMU emulation), there's no single daemon to hand a `platform=linux/amd64,linux/arm64` request
to — the fan-out has to happen above BuildKit, in hephaestus.

## Key decisions and the reasoning behind them

### 1. `Platforms []string` in "os/arch[/variant]" syntax, not a structured type

Considered a structured `{os, arch, variant}` type instead of a string. Rejected it: BuildKit's own
CLI, `docker buildx`, and the OCI image-spec's platform matcher all use the slash-delimited string
form as the canonical human-facing representation. Using the same syntax means a user's mental model
transfers directly, and the normalization/validation logic (`NormalizePlatform` in
`pkg/api/hephaestus/v1/platform.go`) is a single well-tested string parser rather than a webhook-side
translation layer between two representations.

### 2. Admission-time validation against declared pool capabilities, not live pod state

`PlatformCapabilities` (`pkg/api/hephaestus/v1/platform.go`) is built once at manager startup from
`config.Buildkit.Pools()` and installed into the webhook via `SetPlatformCapabilities`. Validation
checks a request against this static snapshot, not against which buildkit pods happen to be running
right now.

This was a deliberate trade-off. The alternative — querying live pod/node state at admission time —
would let a temporarily-scaled-to-zero pool's platforms still validate (since the pool is *declared*
to serve them, just not currently running a pod), matching the existing autoscaling behavior where
`ImageBuild`s already wait for a worker to scale up. Checking live state would incorrectly reject
requests during a scale-to-zero window. The cost of the static-declaration approach is the one
documented sharp edge in `docs/building.md`: declaring a platform in `platformPools` without
matching hardware or a working `binfmt` registration will pass admission but fail at solve time.
There's no way to verify "emulation is actually registered on this node" from the control plane
without either running a synthetic build or inspecting node filesystem state, both of which felt
like solving a different problem than "reject requests that are structurally unsatisfiable."

### 3. `config.Buildkit.Pools()` synthesizes a "default" pool when `PlatformPools` is unset

This is the load-bearing decision for backward compatibility. Every existing deployment configures
`buildkit.namespace`/`podLabels`/`statefulSetName`/`serviceName` directly on the flat `Buildkit`
struct, with no concept of "pools." Rather than requiring a migration, `Pools()`
(`pkg/config/config.go`) synthesizes a single pool named `"default"` from those legacy fields when
`PlatformPools` is empty. Every other new code path — `PlatformCapabilities`, `worker.PoolSet`,
`BuildDispatcherComponent.primaryPoolName` — consumes `Pools()`, never the legacy fields directly, so
there is exactly one place that has to keep the old and new shapes equivalent.

This is also why the default-values `helm template` render is verified byte-for-byte identical
before and after this feature (see "Verification" below): the synthesized pool produces the exact
same `buildkit:` config block the old flat fields did.

### 4. `worker.PoolSet` wraps N `worker.Pool`s instead of changing `worker.Pool`'s interface

Considered adding a `platform` parameter to the existing `AutoscalingPool.Get`/`Release` methods
instead of introducing a new type. Rejected it: `AutoscalingPool` already encapsulates one
`StatefulSet`/`Service`'s worth of leasing state (pod client, endpoint-slice watch, scale arbiter) —
overloading it to span multiple `StatefulSet`s would mean threading a pool-name parameter through
every method and every piece of that internal state, for a type that's unit-tested in isolation and
already carries a lot of concurrency-sensitive logic.

`PoolSet` (`pkg/buildkit/worker/poolset.go`) instead builds one `AutoscalingPool` per
`config.Buildkit.Pools()` entry via `config.Buildkit.WithPool` (which swaps only the four
worker-identity fields, keeping every pool-agnostic setting — `DaemonPort`, `MTLS`, `Secrets`,
`Registries` — shared), and exposes `Get(name string) (Pool, bool)`. `AutoscalingPool` itself is
completely unaware that pools exist as a concept. A single configured/synthesized pool reproduces
the exact previous topology: `NewPoolSet` with one pool is equivalent to the old direct `NewPool`
call, just addressed by name.

### 5. `ResolvePools` prefers whichever pool declares fewer platforms

When a platform is servable by more than one pool (e.g. a native `arm64` pool and a broader
"emulated everything" pool both list `linux/arm64`), `ResolvePools`
(`pkg/api/hephaestus/v1/platform.go`) picks the pool with the smaller total platform count. This is
a heuristic, not a guarantee — there's no direct signal from config about which pool is "native" vs.
"emulated." The proxy is: a pool that only declares the platforms its hardware actually is
(`platforms: ["linux/arm64"]`) is far more likely to be a native pool than one declaring several
(`platforms: ["linux/amd64", "linux/arm64", "linux/arm/v7"]`), which is the shape an
emulation-catch-all pool takes. Preferring the more specific pool means a mixed native+emulated
fleet automatically routes native-capable platforms to native hardware without the user having to
express a priority order.

### 6. Fan-out fails the whole `ImageBuild` atomically, with a per-platform breakdown for visibility

When platforms span multiple pools, `reconcileFanOut` (`builddispatcher.go`) runs every
(pool, platform) solve concurrently via `errgroup`, and if any one fails, the *shared* context is
cancelled — the other in-flight solves are torn down rather than left running to complete a build
that's going to be discarded anyway. The `ImageBuild` as a whole transitions to `Failed`.

The alternative — partial success, e.g. pushing whatever platforms did succeed and marking the
image "incomplete" — was rejected. A manifest list missing a requested platform is a worse failure
mode than no image at all: a consumer pulling by digest on the missing platform gets a confusing
"no matching manifest" error instead of a clear build failure, and there's no clean way to represent
"this image is half-built" in the existing `Succeeded`/`Failed` phase model without inventing a third
terminal state that every downstream consumer of `ImageBuild` status would then need to learn about.

What *is* preserved per-platform is diagnostic detail: `Status.Platforms`
(`[]hephv1.PlatformResult`) records each platform's digest/size/labels on success or its error
message on failure, and the new `hephaestus_imagebuild_platform_phase_total` metric
(`pkg/controller/imagebuild/component/metrics.go`) breaks the terminal phase down by platform. So
the failure is atomic at the resource level, but "which platform actually broke" is still answerable
without digging through pod logs.

### 7. Manual manifest-list assembly via `go-containerregistry`, not a second BuildKit solve

Once every platform's single-platform image is pushed to its own intermediate tag, something has to
combine them into one real OCI image index at the user's requested tag. Two options existed: ask
BuildKit to do it (e.g. via an `imagetools create`-equivalent RPC), or do it directly against the
registry. `AssembleManifestList` (`pkg/buildkit/manifestlist.go`) does the latter, using
`go-containerregistry`'s `mutate`/`remote` packages — the same library hephaestus already depends on
for `retrieveImage`/registry digest lookups elsewhere in this controller.

This was chosen because it's registry-direct and daemon-agnostic: assembly doesn't care which of the
N buildkit daemons produced which platform's image, only that they're all pushed and readable with
the credentials this controller already has (`Client.ResolveAuth`). Routing assembly back through a
BuildKit daemon would mean picking *which* daemon (there's no daemon that's "aware" of every
platform's build) and would add a dependency on a BuildKit RPC surface (`imagetools`) that isn't
part of the client library surface hephaestus already wraps.

### 8. Per-platform intermediate tags via `suffixImageRef`, not separate untagged pushes

Each platform's single-platform solve needs somewhere to push to that (a) doesn't collide with the
other platforms' pushes and (b) can be reliably found again for assembly. `suffixImageRef`
(`builddispatcher.go`) appends `-<platform-slug>` to the image's tag (`repo:v1` →
`repo:v1-linux-arm64`), falling back to tagging with `<default-tag>-<slug>` for an untagged or
digest-based input spec (both of which `name.ParseReference` resolves to *some* tag or digest that
can't be suffixed in place). This keeps every intermediate artifact addressable by a predictable,
human-readable tag in the same repository — useful for debugging a failed fan-out build by hand —
rather than relying on digests threaded purely through in-memory state.

### 9. Fan-out builds detach their terminal work from the cancelable build context

An early version of `reconcileFanOut` ran entirely inside the per-reconcile cancelable context. Two
bugs followed directly from that: releasing a leased worker after a sibling platform's failure
cancelled the shared `errgroup` context could itself get cancelled mid-release, leaking the lease;
and the terminal status write (`failBuild`/`succeedBuild`) could be skipped if the reconcile context
expired while the fan-out was still resolving. The single-pool path already guarded against exactly
this class of problem (`coreCtx.Context = context.Background()` before the terminal write, so phase
transitions are "best effort" regardless of the original context's state) — the fan-out path just
hadn't been given the same treatment yet. Fixed by applying the same pattern (detached background
context for the terminal write) and by giving worker release a `context.WithoutCancel` +
bounded-timeout context, so a sibling's cancellation can't prevent releasing a lease that would
otherwise sit until its expiry-time annotation times it out server-side.

### 10. New Prometheus metric is additive and bounded in cardinality

`hephaestus_imagebuild_platform_phase_total` was added rather than adding a `platform` label to the
existing `hephaestus_imagebuild_phase_total`. Adding a label to an existing metric changes its
identity for every existing consumer (Grafana panels, alerts) keyed on that metric's label set, and
would multiply cardinality for the overwhelming majority of builds that never request a platform.
Instead, the new counter is only ever incremented when `Status.Platforms` is populated — i.e. only
for fan-out builds — so it starts at zero cardinality on any deployment that doesn't use the
feature, and grows only with the (platform × pool) combinations actually configured.

### 11. Why the Helm chart stops short of multi-`StatefulSet` templating

*(Superseded — see the "Addendum" at the end of this document. Left as-is below as the record of
why this was cut from the initial scope.)*

This was the one substantial scope cut in the plan. Templating N buildkit `StatefulSet`s from a
single `platformPools` values list would mean:

- Reworking every buildkit-scoped Helm helper (`hephaestus.buildkit.fullname`,
  `.labels.matchLabels`, `.serviceAccountName`, the mTLS cert/secret names) to be parameterized by
  pool name instead of assuming exactly one buildkit deployment exists.
- Deciding how per-pool overrides (image, resources, node placement, mTLS) compose with the
  chart-wide `buildkit.*` defaults — essentially designing a second values schema nested inside the
  first.
- Verifying the result against a real multi-node cluster, since `helm template`/`helm lint` catch
  syntax and schema issues but not "does the resulting StatefulSet actually schedule and lease
  correctly on distinct node pools" — the actual thing this feature is for.

That verification wasn't available in this environment, and shipping an unverified rewrite of
shared naming/labeling helpers used by RBAC, cert-manager `Certificate` resources, and
`NetworkPolicy` selectors carries real regression risk for every existing deployment, not just
multi-arch adopters. The controller-side fan-out logic (milestone 3) doesn't depend on the chart
having done this — it only needs `config.Buildkit.PlatformPools` to describe pools that already
exist, however they were deployed (hand-written manifests, a second chart release with different
node placement, etc.). So the capability is real and usable today; only the "one command deploys N
buildkit pools" convenience is deferred. This is called out explicitly in
[`building.md`](./building.md) as a known follow-up rather than left implicit.

### 12. `binfmt` DaemonSet defaults to inheriting buildkit's own node placement

The optional `buildkit.binfmt.enabled` DaemonSet registers QEMU emulation on nodes. Its
`nodeSelector`/`tolerations` default to `buildkit.nodeSelector`/`buildkit.tolerations` rather than
being independently empty. Emulation is only useful on the nodes that actually run buildkit pods; if
buildkit is scheduled onto tainted/dedicated nodes (a common pattern for build workloads) and the
`binfmt` DaemonSet didn't inherit those tolerations, it would silently land on the wrong nodes and
registration would have no effect where it's needed. Both fields remain independently overridable
for the case where binfmt genuinely needs different placement.

## Implementation map

| Concern | File | Notes |
|---|---|---|
| Platform string parsing/validation, capability index, pool resolution | `pkg/api/hephaestus/v1/platform.go` | `NormalizePlatform`, `PlatformCapabilities`, `ResolvePools` |
| `Platforms`/`PlatformResult` CRD fields, webhook wiring | `pkg/api/hephaestus/v1/imagebuild_types.go`, `imagebuild_webhook.go`, `imagecache_types.go`, `imagecache_webhook.go` | generated `zz_generated.*` files follow from these |
| `PlatformPools` config, legacy-field synthesis, per-pool config derivation | `pkg/config/config.go` | `Pools()`, `PlatformCapabilities()`, `WithPool()` |
| Multi-pool worker leasing | `pkg/buildkit/worker/poolset.go` | `PoolSet` |
| `BuildOptions.Platforms` → BuildKit `platform` frontend attr, `SolveResult` (image name + digest) | `pkg/buildkit/buildkit.go` | |
| Reference parsing with insecure-registry awareness (extracted, shared) | `pkg/buildkit/reference.go` | `ParseImageReference` |
| Manual manifest-list assembly for fan-out builds | `pkg/buildkit/manifestlist.go` | `AssembleManifestList` |
| Single-pool vs. fan-out routing, concurrent per-platform builds, atomic failure, status/metric population | `pkg/controller/imagebuild/component/builddispatcher.go` | `resolvePlatformGroups`, `reconcileFanOut`, `buildAllPlatforms`, `buildPlatform` |
| Per-platform terminal-phase metric | `pkg/controller/imagebuild/component/metrics.go` | `hephaestus_imagebuild_platform_phase_total` |
| `platformPools`/`binfmt` values passthrough, optional emulation DaemonSet | `deployments/helm/hephaestus/values.yaml`, `templates/controller/secret.yaml`, `templates/buildkit/binfmt-daemonset.yaml` | |
| User-facing usage guide | `docs/building.md` | |

## Verification approach

- Unit tests for every new pure-logic unit (`NormalizePlatform`, `ResolvePools`, `Pools`/`WithPool`,
  `suffixImageRef`, `flattenPlatformAssignments`, `parsePlatform`, `PoolSet`, the metric recorders,
  including the "only increments after a durable status write" regression pattern already
  established for the existing phase metric).
- Full repo `go build`/`go vet`/`go test -race`/`golangci-lint run` clean at every commit boundary,
  not just at the end — each milestone was verified independently before moving to the next.
- `make check` (codegen + `goimports`/`go fmt`/`go mod tidy` + git-clean drift check) passing, so no
  generated-file drift shipped.
- `test/integration` (envtest-backed manager boot, webhook registration) still passes — confirms the
  `worker.Pool` → `worker.PoolSet` signature change through `start.go`/`imagebuild.Register` didn't
  break manager wiring.
- `helm lint` and `helm template` with default values, `platformPools` set, and `binfmt.enabled=true`
  all render successfully; the default-values render was diffed byte-for-byte against the pre-change
  chart and found identical, which is the concrete evidence behind the "zero behavior change for
  non-adopters" goal rather than just an assertion.

## Commit history

```
58d2347 feat(api): add platforms field to ImageBuild/ImageCache with admission validation
8e04a4f feat(buildkit): dispatch single-pool multi-platform solves
1bdcb2a feat(buildkit): fan out multi-pool builds and assemble manifest lists
1f6a97a feat(helm): platformPools passthrough, optional binfmt DaemonSet, new metric
8c89245 chore(api): regen OpenAPI defs for PlatformCapabilities.poolSize
89e39a5 fix(buildkit): detach terminal status writes from cancelable contexts in multi-platform builds
```

Each commit was independently built, tested, and linted clean before the next started — the sequence
reflects incremental, verifiable milestones rather than one large unreviewable change.

## Addendum: Helm multi-`StatefulSet` templating follow-up

The scope cut described in "Non-goals" and decision 11 above was resolved in a follow-up once the
open design questions it identified had concrete answers. This section documents those answers and
the reasoning behind them — the earlier text is left as-is as the record of why this was originally
deferred rather than attempted immediately.

### Gating, not merging, the legacy path

Rather than rewriting the existing single-`StatefulSet` templates to handle both "no pools declared"
and "N pools declared," every existing buildkit template
(`statefulset.yaml`/`service.yaml`/`serviceaccount.yaml`/`configmap.yaml`/`networkpolicy.yaml`) got a
`{{- if not .Values.buildkit.platformPools }}` wrapper and is otherwise byte-for-byte untouched. A
parallel set of `pool-*.yaml` templates, gated the opposite way, ranges over `platformPools` when
it's non-empty. This was chosen over unifying them into one parameterized set of templates because
it makes the "zero behavior change for non-adopters" guarantee trivial to verify mechanically (diff
the default-values render before and after) rather than something that has to be reasoned about
across a much larger, harder-to-review shared template. The cost is some duplication between e.g.
`configmap.yaml` and `pool-configmap.yaml` — an attempt was made to deduplicate that specific one via
a shared named template (see "The nindent/checksum trap" below for why that was abandoned), and the
duplication was judged an acceptable, low-risk trade for keeping the legacy path provably untouched.

### Computed names, not user-supplied ones

The original (pre-follow-up) values.yaml example had users specify `statefulSetName`/`serviceName`/
`podLabels` by hand for each pool, mirroring exactly what they'd have to also put in the *deployed*
resources if they were hand-writing manifests. Once the chart is doing the deploying, that's a
foot-gun: nothing stops the literal string a user types in `values.yaml` from silently drifting out
of sync with what the chart actually names things. The follow-up removes those fields from the
schema entirely — every pool's `StatefulSet`/`Service`/`ServiceAccount`/`ConfigMap`/`NetworkPolicy`
is named `<fullname>-buildkit-<pool.name>` via a new `hephaestus.buildkit.pool.fullname` helper, and
`secret.yaml`'s `platformPools` passthrough to the controller config now computes
`statefulSetName`/`serviceName`/`podLabels` the same way instead of trusting whatever the user
supplied — so the values file and the deployed objects can no longer disagree.

### A load-bearing new label, not just a naming scheme

Every pool's `StatefulSet` selector, `Service` selector, and `NetworkPolicy` podSelector previously
would have been *identical* across pools (`hephaestus.buildkit.labels.matchLabels` doesn't vary by
pool) — this isn't a cosmetic issue, it's a correctness bug: two `StatefulSet`s with the same
selector fight over the same pods. `hephaestus.buildkit.pool.labels.matchLabels` adds a
`hephaestus.dominodatalab.com/platform-pool: <name>` label specifically to fix this, not merely to
make pools distinguishable for observability (though it also does that).

### One shared mTLS cert/secret pair, with per-pool SANs

Considered minting a separate cert-manager `Certificate`/`Secret` pair per pool. Rejected it: the
*client* cert the controller uses is identical regardless of which pool it's talking to (client-auth
certs aren't hostname-checked), so N client certs would be pure duplication. The *server* cert does
need a SAN matching each pool's headless-service DNS pattern (`*.<pool-fullname>` /
`*.<pool-fullname>.<namespace>`) for TLS hostname verification to succeed against that pool's pods —
so the fix was to keep one `Certificate` resource but populate `dnsNames` with one wildcard pair per
pool (falling back to the original two legacy entries when `platformPools` is empty), rather than
creating a `Certificate`/`Secret` per pool for no benefit.

### Per-pool `namespace` is real, so `NetworkPolicy` had to become namespace-aware

`config.PlatformPool.Namespace` (Go side) already let a pool live in a different namespace than the
release — the follow-up gives every pool-scoped Helm resource (`ServiceAccount`/`ConfigMap`/
`Service`/`NetworkPolicy`/`StatefulSet`) an explicit `metadata.namespace` set from the pool's own
`namespace` (defaulting to the release namespace), for parity. This surfaced a real correctness gap:
the existing buildkit `NetworkPolicy`'s ingress rule only had a bare `podSelector`, which — per
Kubernetes `NetworkPolicy` semantics — only matches peer pods in the *same* namespace as the policy
itself. A pool namespaced differently than the controller would otherwise silently block the
controller from ever leasing it. Fixed by adding a `namespaceSelector` matching the controller's
namespace (via the automatic `kubernetes.io/metadata.name` label every namespace carries) alongside
the existing `podSelector` in the same `from` peer entry — the two combine as an AND (pods with
these labels, in a namespace with this name), which is correct whether the pool is co-located with
the controller or not, and was verified against both cases via `helm template`.

### `binfmt` placement inheritance only makes sense for the single implicit pool

The previous milestone's fix (detailed in the main decision log above) made the optional `binfmt`
DaemonSet default to inheriting `buildkit.nodeSelector`/`tolerations`, reasoning that emulation is
only useful on the nodes buildkit itself runs on. With multiple explicit pools that reasoning breaks
down — there's no single "buildkit's nodes" anymore, pools may target entirely different node pools
by design (that's the whole point of native multi-arch). The follow-up keeps the inheritance
default for the `len(platformPools) == 0` case (unchanged, already shipped, tested) and switches to
an unrestricted default (no `nodeSelector`/`tolerations`, i.e. all nodes) once more than zero pools
are explicitly configured, on the reasoning that registering QEMU emulation on a node that doesn't
need it is a harmless no-op, whereas *not* registering it on a node that does need it is a silent
build failure — so "unrestricted by default" is the safer failure mode for the ambiguous case.

### The nindent/checksum trap (a dead end worth recording)

An early attempt at this follow-up extracted the `buildkitd.toml`-generation logic (identical across
every pool, since none of the settings it depends on currently vary per pool) into one shared named
template, called via `{{ include "..." . | nindent 2 }}` from both `configmap.yaml` and
`pool-configmap.yaml`. This is a completely standard Helm pattern, but it broke the byte-identical
guarantee: `nindent` indents *every* line of its input, including otherwise-blank ones — a
source-level blank line inside the shared template came out as a line containing only trailing
spaces, which is YAML/TOML-harmless but is *not* byte-identical to the original, and that changed
the `checksum/config` annotation hash on the `StatefulSet` computed from that ConfigMap's rendered
output (`include (print $.Template.BasePath "/buildkit/configmap.yaml") .`) even though the
ConfigMap's actual `data` was semantically identical. Chasing that down through Sprig's
`regexReplaceAll` argument order (its pipe form binds the piped value to the function's *last*
parameter, which for `regexReplaceAll(regex, s, repl)` is `repl`, not `s` — piping into it backwards
silently replaces the *entire* content with the empty string rather than erroring) confirmed the
fix was possible, but at that point the "shared template" refactor had cost more debugging time than
the ~35 lines of duplication it was meant to save. It was abandoned in favor of literal duplication
between `configmap.yaml` and `pool-configmap.yaml` — recorded here so a future attempt at this same
"obviously worthwhile" cleanup doesn't have to rediscover the trap from scratch.

### Verification for this follow-up specifically

- `helm lint` and `helm template` clean across four scenarios: default (empty `platformPools`),
  two-pool same-namespace (with per-pool `nodeSelector`/`tolerations`/`replicaCount` overrides),
  a single pool in a namespace other than the release namespace, and multi-pool with
  `binfmt.enabled=true`.
- Default-values `helm template` output re-diffed byte-for-byte against the pre-follow-up chart and
  found identical — re-confirming the same guarantee established for the original Helm milestone,
  after a substantially larger set of template changes.
- Manually inspected the rendered output for each scenario against the actual Go-side
  `config.PlatformPool` schema expectations (field names, that `secret.yaml`'s computed
  `statefulSetName`/`serviceName`/`podLabels` match what the corresponding `pool-*.yaml` templates
  actually name/label those resources as) rather than just checking that the YAML parses.

## Addendum: deduplicating the legacy/pool template pairs

A later review of the Helm follow-up flagged the literal duplication between each
`<resource>.yaml`/`pool-<resource>.yaml` pair as worth revisiting, given how large the `StatefulSet`
pair in particular had grown (~150 duplicated lines of container/probe/volume spec). This section
covers that pass, including the `ConfigMap` case the original follow-up had already tried and
abandoned.

### Why nindent was the actual bug, not sharing per se

The abandoned `ConfigMap` attempt (above) called the shared template with `| nindent 2` at the call
site. That's the standard Helm idiom for inserting a block at a given indent, but it was unnecessary
here: both callers need the shared content at the *exact same* indentation (both are a `data:` key's
direct children), because both are top-level manifests with the same shape. Since the indentation was
already identical at both call sites, the shared template's own literal source can simply carry that
indentation baked in, and the caller can `include` it with no `nindent` at all. No reindenting means
no blank line ever gets turned into a trailing-whitespace line, which was the entire root cause of the
original trap - so it sidesteps the bug rather than working around it. Verified this reasoning by
successfully re-extracting `buildkitd.toml` generation into `_configmap.tpl` and getting a truly
byte-identical default-values render, unlike the first attempt.

The same principle applies more broadly: any resource rendered as a full top-level YAML document
(`StatefulSet`, `Service`, `ServiceAccount`, `NetworkPolicy`) starts at column 0, so a shared template
producing the *entire* document never needs nindent at its call site either. Each of those four
resource pairs was consolidated into a `_<resource>.tpl` file holding a
`hephaestus.buildkit.<resource>.render` define that takes a `dict` of whichever fields legitimately
differ between the legacy and pool paths (name, namespace, pre-rendered labels, checksum, per-pool
overrides); both `<resource>.yaml` and `pool-<resource>.yaml` became thin callers that resolve those
fields and `include` the shared render. The `NetworkPolicy` case keeps its genuine behavioral
difference (the pool path's ingress rule needs a `namespaceSelector` the legacy path doesn't) as an
explicit `controllerNamespace` parameter rather than papering over it.

### A second, sharper trap: `{{-` eating the `---` document separator

Extracting `pool-statefulset.yaml`'s body to `{{- include "hephaestus.buildkit.statefulset.render" ... }}`
initially merged the preceding literal `---` line with the included content's first line
(`---apiVersion: apps/v1`), because the leading `{{-` trims the newline right after `---`, not just
insignificant whitespace. `helm template`'s own byte-diff against baseline did *not* catch this -
Helm auto-inserts a `# Source: <path>` comment between the `---` and each file's content in its
concatenated output, which happened to reintroduce the missing newline and mask the bug. `helm lint`
caught it immediately (`invalid Yaml document separator: apiVersion: apps/v1`), because it validates
each template file's own rendered output in isolation, without that auto-inserted comment. Fixed by
dropping the `-` on the include immediately following a literal `---` (`{{ include ...` instead of
`{{- include ...`) in every `pool-*.yaml` wrapper. Lesson: a byte-diff against `helm template` output
is necessary but not sufficient for verifying a multi-document template change - `helm lint` (which
parses each file's raw output) catches a class of bugs the former can hide.

### A real pre-existing bug this pass surfaced: `pool-serviceaccount.yaml`

While reworking `pool-serviceaccount.yaml`, discovered that the original (from the earlier follow-up)
never had a `---` document separator between iterations at all - unlike every other `pool-*.yaml`
template. With `platformPools` set to more than one entry and `buildkit.serviceAccount.create: true`
(the default), it rendered N `ServiceAccount` manifests concatenated with no separator, i.e. one
malformed YAML document with duplicate `apiVersion`/`kind`/`metadata` keys. `helm lint` did not flag
this (YAML parsers commonly tolerate duplicate mapping keys by keeping the last one rather than
erroring), so it shipped unnoticed - only the last pool's `ServiceAccount` would actually end up
applied. Fixed by adding the missing `---` in the same change that deduplicated this template.
Verified by rendering the two-pool scenario and confirming both `test-hephaestus-buildkit-amd64` and
`test-hephaestus-buildkit-arm64` `ServiceAccount` manifests now appear as separate documents.

### Verification for this pass

- Same four scenarios as the original follow-up, plus two more: `buildkit.serviceAccount.create:
  false` with two pools (confirms no pool `ServiceAccount` renders), and a minimal single-entry
  `platformPools` with no per-pool overrides at all (confirms every `default` fallback chain still
  resolves to the shared `buildkit.*` value).
- `helm lint` clean across all six scenarios - this is what caught both traps above; a byte-diff alone
  would have missed them.
- Default-values `helm template` output re-diffed byte-for-byte against pre-pass baseline and found
  identical.
- Each pool scenario's `helm template` output diffed against its pre-pass baseline: identical except
  for the `pool-serviceaccount.yaml` fix (the previously-missing `---`/`# Source:` boundary now
  appears between pool ServiceAccounts), confirming the dedup itself changed no behavior.

## Addendum: unifying the legacy/pool templates into one file each

A further review asked whether the legacy/pool file pairs could be combined at a higher level -
making the no-`platformPools` case an implicit default pool - rather than stopping at "both files
call the same shared render." They can: `.Values.buildkit.platformPools` is normalized to a single
synthetic pool (`dict "name" "" "implicit" true`) when the user declares none, and every
`buildkit/*.yaml` template now ranges over that normalized list instead of branching between "no
pools" and "pools" as two separate files. This dropped the 10 `<resource>.yaml`/`pool-<resource>.yaml`
files down to 5, one per resource, each just a `range` plus a call into its existing
`hephaestus.buildkit.<resource>.render` define.

### Where the implicit pool must diverge from a real one, and why

Three helpers (`hephaestus.buildkit.pool.namespace`, `.labels.standard`/`.labels.matchLabels`, and
`.serviceAccountName`) now branch on `.pool.implicit`, because naively treating the implicit pool as
just another entry would change what actually gets deployed for existing non-adopters:

- **matchLabels**: every explicit pool gets a `hephaestus.dominodatalab.com/platform-pool` label added
  to its selector, to keep pools' `StatefulSet`/`Service`/`NetworkPolicy` selectors from colliding (see
  "A load-bearing new label" above). The implicit pool is the only pool, so it never needed
  disambiguating - and `StatefulSet`/`Service` selectors are immutable, so adding that label to an
  existing legacy install's selector wouldn't just look different, it would make `helm upgrade` fail
  outright. The label is omitted entirely (not merely empty-valued) for `.pool.implicit`.
- **namespace**: every explicit pool gets an explicit `metadata.namespace` (defaulting to the release
  namespace) for parity with `config.PlatformPool.Namespace`. The implicit pool has no per-pool
  namespace concept and never emitted one, so `hephaestus.buildkit.pool.namespace` returns empty for
  it, and callers omit the field - avoiding an inert but visible diff against pre-pool-support output.
- **serviceAccountName**: explicit pools always use `<fullname>-<pool>` and ignore a custom
  `buildkit.serviceAccount.name` override (per "Computed names, not user-supplied ones" above); the
  legacy path always respected that override. The implicit pool preserves the legacy behavior
  (`default (pool.fullname) serviceAccount.name`) rather than silently dropping a value some
  installs may already set.

`hephaestus.buildkit.pool.fullname` needed no such branching: `printf "%s-%s" fullname pool.name` with
an empty `pool.name`, followed by `trimSuffix "-"`, already collapses to plain `fullname` - exactly the
legacy name - so it was reused as-is. The now-fully-superseded `hephaestus.buildkit.serviceAccountName`
helper (the pre-pool-support, non-pool-aware version) was deleted rather than left dead.
`controller/secret.yaml`, which also calls the `pool.*` helpers directly for its `platformPools:`
passthrough, needed no changes - it only ever ranges over the real `.Values.buildkit.platformPools`
list, never the synthetic implicit entry, so the new `.implicit` branches are inert for it.

### A third trap: the separator that only breaks between iterations

Collapsing `range .Values.buildkit.platformPools` (always emitting `---` before every iteration,
including the first) into `range $pools` (needing `---` before every iteration *except* the first, so
the implicit-pool case stays byte-identical to its old separator-free single-document output)
introduced a new trim bug distinct from the earlier `---`-merging one. The naive translation,
`{{- if $i -}}---\n{{ include ... }}{{- else -}}...`, trims the whitespace on *both* sides of the `if`
condition - which eats the newline the previous iteration's content needed on its trailing edge and
the newline this iteration's `---` needed on its leading edge simultaneously, splicing one document's
last line directly onto the next document's `---` and then directly onto `apiVersion` with no
separator at all (`...controller---apiVersion: networking.k8s.io/v1`). Every one of the 5 unified
templates had this exact bug on first pass. `helm lint` did not catch it: the corrupted text is still
syntactically valid YAML (the stray value just becomes part of the previous line's scalar and the
following keys become bogus siblings under the wrong parent), so lint reports success while quietly
producing wrong data - the same blind spot as the `pool-serviceaccount.yaml` bug, but subtler, since
that one omitted a separator entirely rather than merging text across one. Fixed by changing
`{{- if $i -}}` to `{{- if $i }}` (dropping the trailing trim), so the true branch's leading newline -
which is what actually separates one document's last line from `---` - survives. Caught this time by
manually inspecting the two-pool render's `NetworkPolicy` boundary line-by-line after the byte-diff
against the previous (pre-unification) pass showed unexpected content changes beyond the expected
`# Source:` path renames - the lesson from the prior trap (verify with more than one method) held.

### Verification for this pass

- Same six scenarios as the prior pass.
- `helm lint` clean across all six - though, per above, lint alone was not sufficient this time either.
- Default-values `helm template` output re-diffed byte-for-byte against the *original*, pre-any-dedup
  baseline (not just the previous pass) and found identical.
- Each pool scenario diffed against the previous (already-verified-correct) pass with `# Source:`
  lines stripped from both sides first (since every source file was renamed): identical except for the
  `checksum/config` annotation, which is expected to change - it's computed from `configmap.yaml`'s own
  rendered bytes, and that file's internal document-boundary formatting genuinely changed (correctly)
  as part of this unification. Manually inspected the rendered `ConfigMap` boundary between pools to
  confirm the new checksum reflects correctly-separated documents, not a lingering corruption.
- Spot-checked semantic correctness directly (not just structural validity): default render has no
  `platform-pool` label and no `namespace` field anywhere; two-pool render has both pools' labels,
  names, and `replicaCount` overrides correct; `serviceAccount.create: false` with two pools renders no
  buildkit `ServiceAccount` at all.
