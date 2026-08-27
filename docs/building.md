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
other platform - including an emulated one - is rejected at admission, because the manager has no
config field to tell it the legacy pool can serve anything else. Declaring any additional platform, native or emulated, requires migrating
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

2. For any platform that isn't the pool's native architecture, `buildkitd` needs a QEMU emulator
   binary to run that platform's `RUN` steps. It finds this with a plain `$PATH` lookup for a binary
   named `buildkit-qemu-<arch>` (e.g. `buildkit-qemu-aarch64`), which it bind-mounts into the build
   container and execs directly - entirely inside its own container, independent of any kernel
   configuration. See upstream's
   [`solver/llbsolver/ops/exec_binfmt.go`](https://github.com/moby/buildkit/blob/master/solver/llbsolver/ops/exec_binfmt.go)
   and the ["exec format error" troubleshooting section](https://github.com/moby/buildkit/blob/master/docs/multi-platform.md#error-exec-user-process-caused-exec-format-error)
   of BuildKit's own multi-platform docs.

   This means the classic `binfmt_misc` kernel registration (what `docker buildx` relies on for
   single-node multi-arch builds) is irrelevant to `buildkitd` - it never consults it. Nothing
   outside of `buildkit.image` itself (no privileged container, no DaemonSet, no host or node
   configuration) is required, or sufficient, to make emulation work. The only thing that matters is
   whether `buildkit-qemu-<arch>` exists somewhere on `buildkitd`'s `PATH` inside its own image.

   > Do not set `platforms = [...]` in `buildkitd.toml` to declare an emulated platform as native.
   > That makes BuildKit's `getEmulator` treat the platform as natively supported and skip the
   > emulator bind-mount entirely, which breaks emulated builds. `$PATH` is the only supported lever
   > for where BuildKit finds these binaries.

   **With the chart's default `buildkit.image`, this already works with no configuration**:
   upstream `moby/buildkit` images (both the normal and `-rootless` variants) ship
   `buildkit-qemu-<arch>` binaries under `/usr/bin`, which is on `buildkitd`'s `PATH` by default.

   **If you deploy a custom or rebuilt `buildkit.image`, it must bundle these binaries itself** -
   this chart has no mechanism to supply them from outside the image. An image missing them fails
   emulated builds at solve time with `exec format error` (or, for a `RUN` step BuildKit can quietly
   skip or fall back on, can succeed while silently running native-arch content inside an image
   labeled and pushed as the requested non-native platform). Add the binaries wherever your image is
   built, for example:

   - **Deriving from a Wolfi/Chainguard base**: install the `qemu-user` apk package - it ships
     statically-linked `qemu-<arch>` binaries (no `binfmt_misc` involved) for `aarch64`, `x86_64`,
     and `i386` - and symlink them to the names `buildkitd` looks up:

     ```dockerfile
     RUN apk add --no-cache qemu-user \
       && for a in aarch64 x86_64 i386; do ln -s qemu-$a /usr/bin/buildkit-qemu-$a; done
     ```

   - **Deriving from any other base**: copy the prebuilt, pre-renamed binaries straight out of the
     image upstream `moby/buildkit` itself sources them from:

     ```dockerfile
     COPY --link --from=tonistiigi/binfmt:buildkit-v10.2.3-66@sha256:6014c1e52b8e51a67fbf76f691ffbe20ac0204c31c2f086df3e8ef3ce134b488 / /usr/bin/
     ```

   Either way, the binaries must land somewhere on `buildkitd`'s `PATH` inside the final image -
   `/usr/bin` is the simplest choice, since it's already there.

   **Declaring a platform in `platformPools` without either native hardware or a `buildkit.image`
   that carries a working emulator for it will make builds fail at solve time, not at admission
   time** - admission only checks that *some* pool claims the platform, it can't verify the claim is
   true.

Emulated builds are slow; prefer native hardware when it is available.

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
