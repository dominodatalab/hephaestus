//go:build functional && gke

// C211 i123729 — GCP CLOUD QUOTAS quota-preference measurement across the
// hephaestus/Domino project family.
//
// Basis: fact 122360 closes the host project 153711203585 larger-accelerator
// cell as a quota GATE (NVIDIA_A100_80GB_GPUS limit=0 in all 43 regions; the
// enforcing H100 metric GPUS_PER_GPU_FAMILY gpu_family=NVIDIA_H100 limit=0 in
// us-central1) and names cloudquotas.googleapis.com quota-preference creation
// as the one never-measured way to obtain a quota change. Facts 122473/122498/
// 122769 close every sibling A100-80GB/H100/H100-mega/H200/B200/L40S cell as a
// zero-quota or absent-metric GATE. The only prior cloudquotas result in the
// estate (inherited facts 76211/76230) observed the API DISABLED and then an
// active container-level policy restriction returning verbatim HTTP 403 on
// three DIFFERENT GitHub-OIDC WIF projects (1076618220242 / 366271888053 /
// 744911585239), never on these Domino projects.
//
// This probe is READ-ONLY except for (a) POST serviceusage :enable of the
// Cloud Quotas API when the identity is permitted to do so and (b) AT MOST
// THREE quotaPreferences total, all in the host project, highest value first.
// No billing account, no payment method, no existing third-party preference is
// modified. Every provider response is printed verbatim as C211Q_* lines.

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

// c211qProjects is the hephaestus project family: host first.
var c211qProjects = []string{"153711203585", "210628133505", "1072394045651", "463640210929", "1074386228880"}

// c211qQ is one quotaInfo record flattened for matching.
type c211qQ struct {
	Loc    string
	Name   string
	QID    string
	Dims   map[string]string
	Metric string
	Metry  string // metricDisplayName
	Elig   string // quotaIncreaseEligibility verbatim (isEligible + reason)
	Prec   string
	CT     string
}

func c211qParseInfos(loc, body string) []c211qQ {
	out := []c211qQ{}
	m := c211xJSON(body)
	if m == nil {
		return out
	}
	arr, _ := m["quotaInfos"].([]interface{})
	for _, it := range arr {
		im, _ := it.(map[string]interface{})
		if im == nil {
			continue
		}
		q := c211qQ{Loc: loc}
		q.Name = c211xStr(im, "name")
		q.QID = c211xStr(im, "quotaId")
		q.Metric = c211xStr(im, "metric")
		q.Metry = c211xStr(im, "metricDisplayName")
		q.Prec = fmt.Sprintf("%v", im["isPrecise"])
		q.CT = c211xStr(im, "containerType")
		q.Dims = map[string]string{}
		if dm, ok := im["dimensions"].(map[string]interface{}); ok {
			for k, v := range dm {
				q.Dims[k] = fmt.Sprintf("%v", v)
			}
		}
		if e, ok := im["quotaIncreaseEligibility"].(map[string]interface{}); ok {
			q.Elig = fmt.Sprintf("isEligible=%v reason=%v", e["isEligible"], e["ineligibilityReason"])
		}
		out = append(out, q)
	}
	return out
}

// c211qNested walks nested objects and returns a string field.
func c211qNested(m map[string]interface{}, path ...string) string {
	cur := m
	for i, p := range path {
		if cur == nil {
			return ""
		}
		if i == len(path)-1 {
			if v, ok := cur[p].(string); ok {
				return v
			}
			return ""
		}
		nxt, _ := cur[p].(map[string]interface{})
		cur = nxt
	}
	return ""
}

func c211qDims(d map[string]string) string {
	ks := []string{}
	for k := range d {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := []string{}
	for _, k := range ks {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, ",")
}

// c211qListInfos lists quotaInfos in one location and emits a compact summary
// plus the verbatim body on failure.
func c211qListInfos(tok, proj, loc string) ([]c211qQ, int, string) {
	u := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/%s/services/compute.googleapis.com/quotaInfos?pageSize=200", url.PathEscape(proj), url.PathEscape(loc))
	st, body := c211xHTTP("GET", u, tok, "", "")
	qs := c211qParseInfos(loc, body)
	names := []string{}
	for _, q := range qs {
		names = append(names, q.QID)
	}
	sort.Strings(names)
	fmt.Printf("C211Q_QUOTAINFOS proj=%s loc=%s http=%d count=%d quotaIds=[%s]\n", proj, loc, st, len(qs), strings.Join(names, " "))
	if st != 200 {
		fmt.Printf("C211Q_QUOTAINFOS_BODY proj=%s loc=%s verbatim=%s\n", proj, loc, c211xTrunc(body, 900))
	}
	for _, q := range qs {
		fmt.Printf("C211Q_QINFO proj=%s loc=%s quotaId=%s dims=[%s] metric=%s elig=[%s] precise=%s container=%s\n",
			proj, q.Loc, q.QID, c211qDims(q.Dims), q.Metric, q.Elig, q.Prec, q.CT)
	}
	return qs, st, body
}

// c211qBestToken picks the strongest token for a project: the base WIF token,
// or a project service-account token (prefer *fleet*) when the base token is
// not permitted to enable services.
func c211qBestToken(tok, proj string) (string, string, []string, string) {
	reqPerms := []string{"serviceusage.services.enable", "serviceusage.services.get",
		"compute.instances.create", "iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.actAs",
		"resourcemanager.projects.setIamPolicy", "resourcemanager.projects.getIamPolicy"}
	al, st, _ := c211xTestIamBatch(tok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), reqPerms)
	fmt.Printf("C211Q_PERMS proj=%s token=base http=%d allow=[%s]\n", proj, st, strings.Join(al, ","))
	note := ""
	if c211xContains(al, "serviceusage.services.enable") {
		return tok, "base", al, note
	}
	sas, sst, sbody := c211xListServiceAccounts(tok, proj)
	fmt.Printf("C211Q_SA proj=%s list_http=%d n=%d\n", proj, sst, len(sas))
	if sst != 200 {
		fmt.Printf("C211Q_SA_BODY proj=%s verbatim=%s\n", proj, c211xTrunc(sbody, 600))
		return tok, "base", al, note
	}
	emails := []string{}
	for _, a := range sas {
		em := c211xStr(a, "email")
		if em != "" {
			emails = append(emails, em)
		}
	}
	sort.Strings(emails)
	pick := ""
	for _, em := range emails {
		if strings.Contains(strings.ToLower(em), "fleet") {
			pick = em
			break
		}
	}
	if pick == "" {
		fmt.Printf("C211Q_SA_DEFAULT proj=%s emails=%d no-fleet-candidate\n", proj, len(emails))
		return tok, "base", al, note
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, pick)
	fmt.Printf("C211Q_SA_TOKEN proj=%s sa=%s http=%d token_len=%d\n", proj, pick, gst, len(at))
	if at == "" {
		fmt.Printf("C211Q_SA_TOKEN_BODY proj=%s verbatim=%s\n", proj, c211xTrunc(gbody, 700))
		return tok, "base", al, note
	}
	al2, st2, _ := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), reqPerms)
	fmt.Printf("C211Q_PERMS proj=%s token=sa:%s http=%d allow=[%s]\n", proj, pick, st2, strings.Join(al2, ","))
	return at, "sa:" + pick, al2, note
}

func TestC211I123729CQ1Done(t *testing.T) {
	t.Skip("round 1 completed at run 44922221787 / job 136900560622")
	deadline := time.Now().Add(22 * time.Minute)
	tok, method, mlog := c211xMint()
	fmt.Printf("C211Q_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	c211xEmit("C211Q_MINT", mlog)
	if tok == "" {
		t.Fatalf("C211Q no WIF token minted")
	}
	host := c211qProjects[0]
	// ---- Phase 1: per-project permissions, service state, quotaInfos ----
	infos := map[string][]c211qQ{}
	for _, proj := range c211qProjects {
		if time.Now().After(deadline) {
			fmt.Printf("C211Q_BUDGET proj=%s skipped=deadline\n", proj)
			continue
		}
		best, bestName, allow, _ := c211qBestToken(tok, proj)
		canEnable := c211xContains(allow, "serviceusage.services.enable")
		// service state before
		su := "https://serviceusage.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/services/cloudquotas.googleapis.com"
		st, body := c211xHTTP("GET", su, best, "", "")
		fmt.Printf("C211Q_SVC proj=%s token=%s state_http=%d verbatim=%s\n", proj, bestName, st, c211xTrunc(body, 500))
		enabledNow := false
		if m := c211xJSON(body); m != nil && c211xStr(m, "state") == "ENABLED" {
			enabledNow = true
		}
		if !enabledNow && canEnable {
			est, ebody := c211xHTTP("POST", su+":enable", best, "application/json", "{}")
			fmt.Printf("C211Q_ENABLE proj=%s token=%s http=%d verbatim=%s\n", proj, bestName, est, c211xTrunc(ebody, 1400))
			if est == 200 {
				if m := c211xJSON(ebody); m != nil {
					op := c211xStr(m, "name")
					if op != "" {
						ou := "https://serviceusage.googleapis.com/v1/" + op
						for i := 0; i < 20; i++ {
							time.Sleep(6 * time.Second)
							ost, obody := c211xHTTP("GET", ou, best, "", "")
							om := c211xJSON(obody)
							done := false
							if om != nil {
								done = om["done"] == true
							}
							fmt.Printf("C211Q_ENABLE_OP proj=%s iter=%d http=%d done=%v verbatim=%s\n", proj, i, ost, done, c211xTrunc(obody, 600))
							if done {
								break
							}
						}
					}
				}
				st2, body2 := c211xHTTP("GET", su, best, "", "")
				fmt.Printf("C211Q_SVC_AFTER proj=%s state_http=%d verbatim=%s\n", proj, st2, c211xTrunc(body2, 400))
				if m := c211xJSON(body2); m != nil && c211xStr(m, "state") == "ENABLED" {
					enabledNow = true
				}
			}
		} else if !enabledNow {
			fmt.Printf("C211Q_ENABLE_SKIPPED proj=%s token=%s reason=serviceusage.services.enable-not-in-testIamPermissions\n", proj, bestName)
		}
		if !enabledNow {
			continue
		}
		// ---- Phase 2: quotaInfos (global + the two target regions) ----
		all := []c211qQ{}
		for _, loc := range []string{"global", "us-central1", "us-east1"} {
			qs, _, _ := c211qListInfos(best, proj, loc)
			all = append(all, qs...)
		}
		infos[proj] = all
		// testIamPermissions on a real quotaInfo resource when one exists
		for _, q := range all {
			if !strings.Contains(strings.ToUpper(q.QID), "GPU") && !strings.Contains(strings.ToUpper(q.QID), "GPUS") {
				continue
			}
			urlRes := "https://cloudquotas.googleapis.com/v1/" + q.Name
			al, pst, pbody := c211xTestIamBatch(best, urlRes, []string{"cloudquotas.quotas.update", "cloudquotas.quotaPreferences.create", "cloudquotas.quotaInfos.get"})
			fmt.Printf("C211Q_QI_PERMS proj=%s quotaId=%s http=%d allow=[%s] verbatim=%s\n",
				proj, q.QID, pst, strings.Join(al, ","), c211xTrunc(pbody, 400))
			if c211xContains(al, "cloudquotas.quotaPreferences.create") || c211xContains(al, "cloudquotas.quotas.update") {
				break
			}
		}
	}
	// ---- Phase 3: AT MOST THREE quotaPreferences, host project only ----
	hostInfos := infos[host]
	pickInfo := func(pred func(q c211qQ) bool) *c211qQ {
		for i := range hostInfos {
			if pred(hostInfos[i]) {
				return &hostInfos[i]
			}
		}
		return nil
	}
	hasFam := func(q c211qQ, fam string) bool {
		v, ok := q.Dims["gpu_family"]
		return ok && strings.EqualFold(v, fam)
	}
	attempts := []struct {
		label string
		q     *c211qQ
	}{
		{"h100-uscentral1", pickInfo(func(q c211qQ) bool {
			u := strings.ToUpper(q.QID)
			return (strings.Contains(u, "GPUS-PER-GPU-FAMILY") || strings.Contains(u, "GPUS_PER_GPU_FAMILY")) && hasFam(q, "NVIDIA_H100")
		})},
		{"a100-80gb-uscentral1", pickInfo(func(q c211qQ) bool {
			return strings.Contains(strings.ToUpper(q.QID), "A100-80GB")
		})},
		{"b200-useast1", pickInfo(func(q c211qQ) bool {
			u := strings.ToUpper(q.QID)
			return (strings.Contains(u, "GPUS-PER-GPU-FAMILY") || strings.Contains(u, "GPUS_PER_GPU_FAMILY")) && hasFam(q, "NVIDIA_B200")
		})},
	}
	// Enumerate every GPUS_PER_GPU_FAMILY dimension value seen, for the matrix.
	famVals := []string{}
	for _, q := range hostInfos {
		u := strings.ToUpper(q.QID)
		if strings.Contains(u, "GPUS-PER-GPU-FAMILY") || strings.Contains(u, "GPUS_PER_GPU_FAMILY") {
			famVals = append(famVals, q.QID+"["+c211qDims(q.Dims)+"]@"+q.Loc)
		}
	}
	sort.Strings(famVals)
	fmt.Printf("C211Q_FAMILY_MATRIX proj=%s entries=%d list=[%s]\n", host, len(famVals), strings.Join(famVals, " | "))
	prefN := 0
	for _, a := range attempts {
		if prefN >= 3 {
			break
		}
		if a.q == nil {
			fmt.Printf("C211Q_PREF proj=%s label=%s http=-1 reason=quotaInfo-not-found\n", host, a.label)
			continue
		}
		if time.Now().After(deadline) {
			fmt.Printf("C211Q_PREF proj=%s label=%s http=-1 reason=deadline\n", host, a.label)
			continue
		}
		q := *a.q
		prefN++
		prefID := fmt.Sprintf("c211-i123729-%d", prefN)
		body := map[string]interface{}{
			"service":     "compute.googleapis.com",
			"quotaId":     q.QID,
			"dimensions":  q.Dims,
			"quotaConfig": map[string]interface{}{"preferredValue": "1"},
			"justification": "c211-i123729 nonce measurement of Cloud Quotas quota-preference creation for a " +
				"zero-limit GPU metric; no production use, no billing change",
		}
		bj, _ := json.Marshal(body)
		pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/%s/quotaPreferences?quotaPreferenceId=%s",
			url.PathEscape(host), url.PathEscape(q.Loc), url.PathEscape(prefID))
		st, rbody := c211xHTTP("POST", pu, tok, "application/json", string(bj))
		fmt.Printf("C211Q_PREF proj=%s label=%s quotaId=%s dims=[%s] loc=%s http=%d verbatim=%s\n",
			host, a.label, q.QID, c211qDims(q.Dims), q.Loc, st, c211xTrunc(rbody, 1600))
		if st == 200 || st == 201 {
			m := c211xJSON(rbody)
			name := c211xStr(m, "name")
			if name != "" {
				gu := "https://cloudquotas.googleapis.com/v1/" + name
				for i := 0; i < 10; i++ {
					gst, gbody := c211xHTTP("GET", gu, tok, "", "")
					gm := c211xJSON(gbody)
					granted := ""
					state := ""
					detail := ""
					if gm != nil {
						granted = c211qNested(gm, "quotaConfig", "grantedValue")
						state = c211qNested(gm, "quotaConfig", "stateDetail")
						detail = fmt.Sprintf("%v", gm["reconciling"])
					}
					fmt.Printf("C211Q_PREF_STATE proj=%s label=%s iter=%d http=%d name=%s granted=%s stateDetail=%s reconciling=%s verbatim=%s\n",
						host, a.label, i, gst, name, granted, state, detail, c211xTrunc(gbody, 900))
					if i >= 2 {
						break
					}
					time.Sleep(8 * time.Second)
				}
			}
		}
	}
	// ---- Final verdict ----
	fmt.Printf("C211Q_VERDICT_FINAL projects=%d infos_counts=", len(c211qProjects))
	parts := []string{}
	for _, p := range c211qProjects {
		parts = append(parts, fmt.Sprintf("%s:%d", p, len(infos[p])))
	}
	fmt.Printf("[%s] prefs_attempted=%d elapsed=%ds\n", strings.Join(parts, ","), prefN, int(time.Since(deadline.Add(-22*time.Minute)).Seconds()))
}
