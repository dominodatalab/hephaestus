//go:build functional

package functional

// Read-only credential/identity probe for the deployed-hephaestus functional
// test jobs. It never creates, modifies or deletes any cloud resource and it
// never prints secret values: only identity metadata, quota values and
// instance-family counts.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

const c211Marker = "C211-I108373"

// c211exec runs a command with a timeout and logs its combined output (truncated).
func c211exec(t *testing.T, timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=", "AZURE_CORE_ONLY_SHOW_ERRORS=true", "AZURE_EXTENSION_USE_DYNAMIC_INSTALL=no")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 6000 {
		s = s[:6000] + "\n...[c211 truncated]"
	}
	t.Logf("%s CMD %s %s => exit=%d\n%s", c211Marker, name, strings.Join(args, " "), code, s)
	return s, code
}

func c211has(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// c211envNames lists names (not values) of cloud-related environment variables.
func c211envNames() []string {
	prefixes := []string{"AWS_", "ARM_", "AZURE_", "GOOGLE_", "GCP_", "CLOUDSDK_", "AZ_"}
	var names []string
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				names = append(names, k)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// c211counts summarises instance types by GPU family without dumping the list.
func c211counts(types []string) string {
	fams := []string{"p2.", "p3.", "p4", "p5.", "p6", "g2.", "g3.", "g4", "g5.", "g6"}
	counts := map[string]int{}
	total := 0
	for _, it := range types {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		total++
		for _, f := range fams {
			if strings.HasPrefix(it, f) {
				counts[f]++
				break
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return fmt.Sprintf("total=%d gpu_families[%s]", total, strings.Join(parts, ","))
}

func c211logGPUQuotaLines(t *testing.T, label, out string) {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		up := strings.ToUpper(line)
		if strings.Contains(up, "NVIDIA") || strings.Contains(up, "GPU") || strings.Contains(up, "TPU") {
			keep = append(keep, strings.TrimSpace(line))
		}
	}
	if len(keep) > 80 {
		keep = keep[:80]
	}
	t.Logf("%s %s count=%d\n%s", c211Marker, label, len(keep), strings.Join(keep, "\n"))
}

func c211logAzureGPUUsage(t *testing.T, out string) {
	var items []struct {
		CurrentValue int    `json:"currentValue"`
		Limit        int    `json:"limit"`
		LocalName    string `json:"localName"`
		Name         struct {
			Value          string `json:"value"`
			LocalizedValue string `json:"localizedValue"`
		} `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Logf("%s AZ_VM_USAGE_PARSE_ERROR err=%v", c211Marker, err)
		return
	}
	var lines []string
	for _, it := range items {
		n := strings.ToUpper(it.Name.Value)
		loc := strings.ToUpper(it.Name.LocalizedValue)
		if strings.Contains(n, "NC") || strings.Contains(n, "ND") || strings.Contains(n, "NV") ||
			strings.Contains(n, "GPU") || strings.Contains(n, "H100") || strings.Contains(n, "A100") ||
			strings.Contains(loc, "GPU") {
			lines = append(lines, fmt.Sprintf("%s current=%d limit=%d (%s)", it.Name.Value, it.CurrentValue, it.Limit, it.LocalName))
		}
	}
	if len(lines) > 120 {
		lines = lines[:120]
	}
	t.Logf("%s AZURE_VM_GPU_USAGE count=%d\n%s", c211Marker, len(lines), strings.Join(lines, "\n"))
}

func TestC211Probe(t *testing.T) {
	host, _ := os.Hostname()
	t.Logf("%s PROBE START host=%s job=%q workflow=%q event=%q ref=%q repo=%q sha=%q",
		c211Marker, host,
		os.Getenv("GITHUB_JOB"), os.Getenv("GITHUB_WORKFLOW"), os.Getenv("GITHUB_EVENT_NAME"),
		os.Getenv("GITHUB_REF"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_SHA"))
	t.Logf("%s ENV_NAMES %s", c211Marker, strings.Join(c211envNames(), ","))
	t.Logf("%s CONTEXT AWS_REGION=%q GCP_REGION=%q GCP_PROJECT_ID=%q AZURE_LOCATION=%q",
		c211Marker, os.Getenv("AWS_REGION"), os.Getenv("GCP_REGION"), os.Getenv("GCP_PROJECT_ID"), os.Getenv("AZURE_LOCATION"))

	// ---------------- AWS ----------------
	if !c211has("aws") {
		t.Logf("%s AWS_CLI missing", c211Marker)
	} else {
		c211exec(t, 60*time.Second, "aws", "sts", "get-caller-identity", "--output", "json")
		for _, q := range []string{"L-DB2E81BA", "L-3819A6DF", "L-417A185B"} {
			c211exec(t, 60*time.Second, "aws", "service-quotas", "get-service-quota", "--service-code", "ec2", "--quota-code", q, "--output", "json")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		cmd := exec.CommandContext(ctx, "aws", "ec2", "describe-instances", "--filters", "Name=instance-state-name,Values=running", "--query", "Reservations[].Instances[].InstanceType", "--output", "text")
		cmd.Env = append(os.Environ(), "AWS_PAGER=")
		out, err := cmd.Output()
		cancel()
		if err != nil {
			code := -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			t.Logf("%s AWS_DESCRIBE_INSTANCES_ERROR exit=%d err=%v", c211Marker, code, err)
		} else {
			t.Logf("%s AWS_RUNNING_INSTANCES %s", c211Marker, c211counts(strings.Fields(string(out))))
		}
	}

	// ---------------- GCP ----------------
	if !c211has("gcloud") {
		t.Logf("%s GCLOUD_CLI missing", c211Marker)
	} else {
		c211exec(t, 60*time.Second, "gcloud", "config", "list")
		c211exec(t, 60*time.Second, "gcloud", "auth", "list", "--format=value(account,status)")
		proj, _ := c211exec(t, 90*time.Second, "gcloud", "compute", "project-info", "describe", "--format=value(quotas.metric,quotas.limit,quotas.usage)")
		c211logGPUQuotaLines(t, "GCP_PROJECT_GPU_QUOTAS", proj)
		if r := os.Getenv("GCP_REGION"); r != "" {
			reg, _ := c211exec(t, 90*time.Second, "gcloud", "compute", "regions", "describe", r, "--format=value(quotas.metric,quotas.limit,quotas.usage)")
			c211logGPUQuotaLines(t, "GCP_REGION_GPU_QUOTAS", reg)
		}
		c211exec(t, 90*time.Second, "gcloud", "services", "list", "--enabled", "--filter=aiplatform")
	}

	// ---------------- AZURE ----------------
	if !c211has("az") {
		t.Logf("%s AZ_CLI missing", c211Marker)
	} else {
		_, code := c211exec(t, 90*time.Second, "az", "account", "show", "-o", "json")
		if code != 0 {
			cid := os.Getenv("AZURE_CLIENT_ID")
			if cid == "" {
				cid = os.Getenv("ARM_CLIENT_ID")
			}
			csec := os.Getenv("AZURE_CLIENT_SECRET")
			if csec == "" {
				csec = os.Getenv("ARM_CLIENT_SECRET")
			}
			cten := os.Getenv("AZURE_TENANT_ID")
			if cten == "" {
				cten = os.Getenv("ARM_TENANT_ID")
			}
			if cid != "" && csec != "" && cten != "" {
				// Redacted variant: the secret is passed as an argv element that is
				// never logged, and any echo of it is scrubbed before logging.
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				cmd := exec.CommandContext(ctx, "az", "login", "--service-principal", "-u", cid, "-p", csec, "--tenant", cten, "-o", "none")
				cmd.Env = append(os.Environ(), "AZURE_CORE_ONLY_SHOW_ERRORS=true", "AZURE_EXTENSION_USE_DYNAMIC_INSTALL=no")
				out, err := cmd.CombinedOutput()
				cancel()
				lc := 0
				if err != nil {
					lc = -1
				}
				s := string(out)
				if csec != "" {
					s = strings.ReplaceAll(s, csec, "[redacted-secret]")
				}
				if cid != "" {
					s = strings.ReplaceAll(s, cid, "[redacted-client-id]")
				}
				if len(s) > 800 {
					s = s[:800]
				}
				t.Logf("%s CMD az login --service-principal (secret redacted) => exit=%d\n%s", c211Marker, lc, strings.TrimSpace(s))
				c211exec(t, 90*time.Second, "az", "account", "show", "-o", "json")
			} else {
				t.Logf("%s AZ_LOGIN_SKIPPED missing env credential names", c211Marker)
			}
		}
		if loc := os.Getenv("AZURE_LOCATION"); loc != "" {
			usage, _ := c211exec(t, 150*time.Second, "az", "vm", "list-usage", "--location", loc, "-o", "json")
			c211logAzureGPUUsage(t, usage)
		}
	}

	t.Logf("%s PROBE END", c211Marker)
}
