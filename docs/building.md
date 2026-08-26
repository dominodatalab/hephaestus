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

2. For any platform that isn't the pool's native architecture, `buildkitd` needs a way to run
   binaries built for that platform. Set `buildkit.binfmt.enabled: true` to have the chart handle
   this: a privileged initContainer (based on `tonistiigi/binfmt` by default - override
   `buildkit.binfmt.image` to use your own) registers `/proc/sys/fs/binfmt_misc` QEMU handlers on
   that pod's node - the same registration `docker buildx` relies on for single-node multi-arch
   builds - and a second, unprivileged initContainer stages `buildkitd`'s own QEMU emulator
   binaries (named `buildkit-qemu-<arch>`, e.g. `buildkit-qemu-aarch64`) into a volume shared with
   the `buildkitd` container, on a `PATH` entry added just for this purpose.

   That second step exists because `buildkitd` doesn't actually use the `binfmt_misc` registration
   to run non-native `RUN` steps in a Dockerfile - Kubernetes gives every container in a pod its
   own private mount namespace, so a `binfmt_misc` registration made by one container's initContainer
   never becomes visible under a sibling container's own `/proc/sys/fs/binfmt_misc` (there's no
   shared-mount-namespace equivalent of `hostNetwork`/`hostPID` for this). Instead, `buildkitd` does
   a plain `$PATH` lookup for `buildkit-qemu-<arch>` and execs it directly, entirely inside its own
   container - see upstream's
   [`solver/llbsolver/ops/exec_binfmt.go`](https://github.com/moby/buildkit/blob/master/solver/llbsolver/ops/exec_binfmt.go)
   and the ["exec format error" troubleshooting section](https://github.com/moby/buildkit/blob/master/docs/multi-platform.md#error-exec-user-process-caused-exec-format-error)
   of BuildKit's own multi-platform docs. Upstream `moby/buildkit` images bundle these binaries
   under `/usr/bin` by default, which is why this can look like it "just works" without the shared
   volume - but that's an implicit assumption about whatever `buildkitd` image tag happens to be
   deployed. Losing access to that binary (an older/custom/mirrored image build that doesn't bundle
   it, for example) doesn't fail loudly and consistently: depending on which `RUN` step trips it,
   the build can either fail outright with `exec format error`, or - for a step BuildKit can quietly
   skip or fall back on - **succeed while silently running amd64 content inside an image labeled and
   pushed as the requested non-native platform**. The chart's shared-volume initContainer makes the
   dependency explicit and self-contained instead of relying on whatever the deployed `buildkitd`
   image happens to bundle.

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
