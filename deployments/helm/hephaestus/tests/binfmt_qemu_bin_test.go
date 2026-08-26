// Package tests contains chart-rendering tests for the hephaestus Helm chart.
//
// These render the chart with `helm template` and assert on the resulting manifests. This layer
// exists specifically to catch the class of bug a Go unit test can't: two containers in the same
// pod silently disagreeing about a filesystem path (e.g. an initContainer writing to a volume the
// main container doesn't mount at the same place, or at all).
package tests

import (
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// chartDir is relative to this package's directory (deployments/helm/hephaestus/tests).
const chartDir = ".."

// renderBuildkitStatefulSet runs `helm template` against the chart with the given extra --set/--set-string
// style arguments and returns the buildkit StatefulSet's pod template spec. It skips the test if
// no `helm` binary is available on PATH.
func renderBuildkitStatefulSet(t *testing.T, extraArgs ...string) *corev1.PodSpec {
	t.Helper()

	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not found on PATH; skipping chart-rendering test")
	}

	args := append([]string{
		"template", chartDir,
		"--show-only", "templates/buildkit/statefulset.yaml",
	}, extraArgs...)

	out, err := exec.Command(helmPath, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}

	// `helm template --show-only` for a single file produces one YAML document (with a leading
	// "---" separator and a "# Source:" comment), so we can decode it directly. We only care
	// about the pod template's spec, so decode into a narrow anonymous shape rather than pulling
	// in the full apps/v1 StatefulSet type.
	var rendered struct {
		Spec struct {
			Template struct {
				Spec corev1.PodSpec `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("failed to parse rendered buildkit statefulset: %v\n%s", err, out)
	}
	return &rendered.Spec.Template.Spec
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func volumeMountPath(c *corev1.Container, volumeName string) (string, bool) {
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName {
			return m.MountPath, true
		}
	}
	return "", false
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// TestBuildkitQemuBinVolumeSharing guards against the binfmt/emulation bug this test suite was
// added for: buildkitd finds its QEMU emulator binaries ("buildkit-qemu-<arch>") via a plain
// $PATH lookup done entirely inside its own container (see moby/buildkit's
// solver/llbsolver/ops/exec_binfmt.go) - this is independent of the classic binfmt_misc kernel
// registration, which does not cross container boundaries within a pod. That means the
// buildkit-qemu-bin initContainer and the buildkitd container MUST mount the same volume at the
// same path, and that path MUST be on buildkitd's PATH, or multi-arch RUN steps silently
// misbehave (see docs/building.md for the full writeup).
func TestBuildkitQemuBinVolumeSharing(t *testing.T) {
	podSpec := renderBuildkitStatefulSet(t, "--set", "buildkit.binfmt.enabled=true")

	qemuBinInit := findContainer(podSpec.InitContainers, "buildkit-qemu-bin")
	if qemuBinInit == nil {
		t.Fatal(`expected an initContainer named "buildkit-qemu-bin" when buildkit.binfmt.enabled is true`)
	}

	buildkitd := findContainer(podSpec.Containers, "buildkitd")
	if buildkitd == nil {
		t.Fatal(`expected a container named "buildkitd"`)
	}

	if len(qemuBinInit.VolumeMounts) != 1 {
		t.Fatalf("expected the buildkit-qemu-bin initContainer to have exactly one volumeMount, got %d: %+v",
			len(qemuBinInit.VolumeMounts), qemuBinInit.VolumeMounts)
	}
	volumeName := qemuBinInit.VolumeMounts[0].Name
	initMountPath := qemuBinInit.VolumeMounts[0].MountPath

	if !hasVolume(podSpec.Volumes, volumeName) {
		t.Fatalf("pod spec has no volume named %q backing the buildkit-qemu-bin initContainer's mount", volumeName)
	}

	buildkitdMountPath, ok := volumeMountPath(buildkitd, volumeName)
	if !ok {
		t.Fatalf("buildkitd container has no volumeMount for volume %q, but buildkit-qemu-bin writes into it - "+
			"buildkitd will never see what got staged there", volumeName)
	}
	if buildkitdMountPath != initMountPath {
		t.Fatalf("buildkit-qemu-bin initContainer mounts volume %q at %q, but buildkitd mounts it at %q - "+
			"these must be the same path", volumeName, initMountPath, buildkitdMountPath)
	}

	// Never mount the shared volume over a path the buildkitd image already populates itself -
	// doing so would hide the image's own binaries from buildkitd (see the chart's inline comment).
	for _, shadowed := range []string{"/usr/bin", "/usr/local/bin", "/bin", "/sbin"} {
		if buildkitdMountPath == shadowed {
			t.Fatalf("qemu-bin volume must not be mounted at %q - this would shadow buildkitd's own image binaries", buildkitdMountPath)
		}
	}

	var pathValue string
	var sawPath bool
	for _, e := range buildkitd.Env {
		if e.Name == "PATH" {
			pathValue = e.Value
			sawPath = true
		}
	}
	if !sawPath {
		t.Fatal("buildkitd container has no PATH env var set, so it can't find buildkit-qemu-<arch> binaries staged in the shared volume")
	}

	found := false
	for _, entry := range strings.Split(pathValue, ":") {
		if entry == buildkitdMountPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildkitd's PATH (%q) does not include the shared qemu-bin mount path %q", pathValue, buildkitdMountPath)
	}
}

// TestBuildkitQemuBinDisabledByDefault confirms the qemu-bin volume/initContainer/PATH override
// only appear when buildkit.binfmt.enabled is true, leaving default (binfmt-disabled)
// deployments byte-for-byte unaffected by this change.
func TestBuildkitQemuBinDisabledByDefault(t *testing.T) {
	podSpec := renderBuildkitStatefulSet(t)

	if len(podSpec.InitContainers) != 0 {
		t.Fatalf("expected no initContainers when buildkit.binfmt.enabled is false (the default), got %d", len(podSpec.InitContainers))
	}

	buildkitd := findContainer(podSpec.Containers, "buildkitd")
	if buildkitd == nil {
		t.Fatal(`expected a container named "buildkitd"`)
	}
	if len(buildkitd.Env) != 0 {
		t.Fatalf("expected no env vars on buildkitd when buildkit.binfmt.enabled is false and podEnv is unset, got %+v", buildkitd.Env)
	}
	if _, ok := volumeMountPath(buildkitd, "qemu-bin"); ok {
		t.Fatal("did not expect a qemu-bin volumeMount on buildkitd when buildkit.binfmt.enabled is false")
	}
	if hasVolume(podSpec.Volumes, "qemu-bin") {
		t.Fatal("did not expect a qemu-bin volume when buildkit.binfmt.enabled is false")
	}
}
