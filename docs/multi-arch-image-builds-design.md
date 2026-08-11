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
