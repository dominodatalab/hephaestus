# Building multi-platform images

`ImageBuild.spec.platforms` requests one or more target platforms, in `os/arch[/variant]` syntax
(e.g. `linux/amd64`, `linux/arm64`, `linux/arm/v7`). Leaving it empty builds on whatever platform
the leased buildkit worker natively runs, exactly as before this field existed.

A requested platform must be served by at least one configured buildkit pool, or the
`ImageBuild`/`ImageCache` is rejected at admission time with a list of the platforms that are
actually available. This check is against static configuration, not live cluster state, so a
request can never hang waiting on a pod that can never be scheduled.

**If `buildkit.platformPools` is unset** (every deployment that hasn't opted into multi-arch),
the controller synthesizes a single `"default"` pool from the legacy flat `buildkit.namespace`/
`podLabels`/`statefulSetName`/`serviceName` fields, and that pool only ever declares
`linux/amd64` - the platform every such deployment's buildkit pods actually run on. Requesting
`linux/amd64` (or leaving `spec.platforms` empty) works with zero config changes; requesting any
other platform - including an emulated one, even with `buildkit.binfmt.enabled: true` - is
rejected at admission, because the manager has no config field to tell it the legacy pool has
emulation registered. Declaring any additional platform, native or emulated, requires migrating
to `buildkit.platformPools` (below), even if you only need one pool.

## Single-pool multi-platform builds (works today, out of the box)

If every requested platform is declared on one pool, hephaestus runs a single BuildKit solve
requesting all of them, and BuildKit produces a multi-platform manifest list/OCI index directly.
This is the easiest way to get multi-arch images and requires no additional buildkit deployments -
just:

1. Declare a single pool listing every platform it should serve. Via the Helm chart, set
   `buildkit.platformPools` in `values.yaml`:

   ```yaml
   buildkit:
     platformPools:
       - name: default
         platforms: ["linux/amd64", "linux/arm64"]
   ```

   This is enough on its own - the chart deploys exactly one `StatefulSet`/`Service`/etc. for this
   pool (see the chart's inline documentation for the placement/sizing fields you can override per
   pool). Configuring the controller directly (outside Helm) is the same shape, just with
   `namespace`/`statefulSetName`/`serviceName` given explicitly instead of chart-computed:

   ```yaml
   buildkit:
     platformPools:
       - name: default
         namespace: <buildkit namespace>
         statefulSetName: <buildkit statefulset name>
         serviceName: <buildkit service name>
         platforms: ["linux/amd64", "linux/arm64"]
   ```

2. For any platform that isn't the pool's native architecture, that pool's nodes need QEMU
   user-mode emulation registered (`binfmt_misc`) - the same mechanism `docker buildx` relies on for
   single-node multi-arch builds. The chart can deploy this for you: set
   `buildkit.binfmt.enabled: true` to run a privileged initContainer (based on `tonistiigi/binfmt` by
   default - override `buildkit.binfmt.image` to use your own) on every buildkit pod that registers
   the emulation handlers on that pod's node before `buildkitd` starts.

   **Declaring a platform in `platformPools` without either native hardware or working emulation
   registered on that pool's nodes will make builds fail at solve time, not at admission time** -
   admission only checks that *some* pool claims the platform, it can't verify the claim is true.

Emulated builds are correct but slow (QEMU is not free); prefer native hardware when you have it.

## Native multi-pool builds (fan-out)

If the requested platforms are declared across more than one pool, the controller leases a worker
from each pool independently, solves each platform separately (each pushing to a per-platform
intermediate tag), and assembles the results into one manifest list per requested image reference.
The build fails atomically - if any platform fails, the whole `ImageBuild` fails - and
`status.platforms` records a per-platform digest/size/labels/error breakdown regardless of outcome.

This is the path to use when you have genuinely native hardware for more than one architecture
(e.g. real arm64 nodes alongside real amd64 nodes) and want to avoid emulation entirely. Declare one
pool per architecture, each with the platform(s) it's native to and a `nodeSelector`/`tolerations`
override pinning it to the matching nodes:

```yaml
buildkit:
  platformPools:
    - name: amd64
      platforms: ["linux/amd64"]
      nodeSelector:
        kubernetes.io/arch: amd64
    - name: arm64
      platforms: ["linux/arm64"]
      nodeSelector:
        kubernetes.io/arch: arm64
      tolerations:
        - key: dedicated
          operator: Equal
          value: buildkit-arm64
          effect: NoSchedule
```

The chart deploys one `StatefulSet`/`Service`/`ServiceAccount`/`ConfigMap`/`NetworkPolicy` per pool
entry (named `<release>-hephaestus-buildkit-<name>`), sharing one mTLS certificate/secret pair and
one `ServiceAccount` identity's worth of RBAC across all of them - only placement/sizing differs per
pool. A pool's `namespace` can be set independently of the release namespace if that pool's
`StatefulSet` should live elsewhere; the generated `NetworkPolicy` allows controller ingress either
way. Outside Helm, the same fan-out just means each pool's `namespace`/`statefulSetName`/
`serviceName` in the controller config must point at whatever you deployed for it (hand-written
manifests, or a separate release of this chart), however you chose to deploy it.
