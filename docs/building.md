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

1. Declare the pool's platforms in the controller config:

   ```yaml
   buildkit:
     platformPools:
       - name: default
         namespace: <buildkit namespace>
         statefulSetName: <buildkit statefulset name>
         serviceName: <buildkit service name>
         platforms: ["linux/amd64", "linux/arm64"]
   ```

   Via the Helm chart, set `buildkit.platformPools` in `values.yaml` (see the chart's inline
   documentation for the exact shape).

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
(e.g. real arm64 nodes alongside real amd64 nodes) and want to avoid emulation entirely. It requires
more than one buildkit pool to actually exist - as of this chart version, that means deploying
additional buildkit `StatefulSet`s/`Service`s yourself (hand-written manifests, or a separate release
of this chart pointed at different node placement) and declaring each of them as its own entry in
`buildkit.platformPools`, each with the platform(s) it's native to. Templating multiple buildkit
`StatefulSet`s from a single list of pools in this chart is a known follow-up, not yet automated.
