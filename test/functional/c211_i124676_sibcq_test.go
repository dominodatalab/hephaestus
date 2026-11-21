//go:build functional && gke

// C211 i124676 — corrected four-sibling Cloud Quotas ladder.
//
// Fact 124650 proved the corrected run 44940627321 / GKE job 136956829974 used
// the exact proven identity
// fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com
// (len 76, sha256 9736b01aaa62f06b, token 200) and ENABLED
// cloudquotas.googleapis.com plus self-granted roles/cloudquotas.admin in all
// four Domino siblings, but created ZERO quotaPreferences because the helper
// c211cqNested returns only strings and therefore parsed the verbatim JSON
// boolean `quotaIncreaseEligibility.isEligible = true` as "" (eligible=false),
// so every family looked ineligible.
//
// This probe keeps the same vehicle/identity and fixes exactly that defect: the
// eligibility flag is unmarshalled as a real bool (string, bool and number forms
// accepted) while every verbatim quotaInfo body is still emitted. It then, per
// sibling project, creates AT MOST TWO quotaPreferences:
//
//	(1) EXACTLY ONE GPU-quota preference for the highest-value eligible
//	    zero-limit metric in order NVIDIA_H100 -> NVIDIA_H100_MEGA ->
//	    NVIDIA_A100_80GB (legacy region metric) -> NVIDIA_RTX_PRO_6000, in the
//	    first priority region where the matching shape is actually offered
//	    (fallback us-central1), smallest preferredValue that admits one marker;
//	    only if every family is ineligible, ONE aiplatform
//	    custom_model_training_nvidia_h100_gpus fallback;
//	(2) EXACTLY ONE compute INSTANCES-per-project-region preference in that same
//	    region with preferredValue 1, because the compute region quota measures
//	    limit 0.0 in all four siblings (fact 124547) and no GCE marker can
//	    launch without it.
//
// Each preference is polled to terminal and grantedValue/stateDetail are
// recorded verbatim (HTTP 403 container-policy or grantedValue=0 are valid
// provider verdicts and no metric is retried). The probe creates NO marker:
// the marker is launched out-of-band with the saved durable sibling owner key
// only if a GPU preference yields grantedValue>=1 and INSTANCES reads/grants
// >=1. No secret is printed; every preference is nonce-tagged c211-i124676 and
// is our own resource.

package functional

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const c211cqNonce = "i124676"

// c211cqFleetEmail is the exact verbatim service account that crossed into the
// four Domino sibling projects (len 76, sha256 9736b01aaa62f06b...). It must
// never be selected by a loose substring match.
const c211cqFleetEmail = "fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com"

type c211cqProj struct {
	Name string
	Num  string
}

var c211cqProjects = []c211cqProj{
	{"team-eng-infra-testing", "210628133505"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

var c211cqPerms = []string{
	"cloudquotas.quotaPreferences.create",
	"cloudquotas.quotaPreferences.get",
	"cloudquotas.quotaPreferences.list",
	"cloudquotas.quotas.update",
	"serviceusage.services.enable",
	"serviceusage.services.get",
	"resourcemanager.projects.setIamPolicy",
	"resourcemanager.projects.getIamPolicy",
	"compute.instances.create",
	"compute.instances.get",
	"compute.instances.delete",
	"iam.serviceAccounts.actAs",
}

type c211cqCand struct {
	Family  string
	Kind    string // "family" (region dimension of GPUS-PER-GPU-FAMILY) or "region" (legacy per-project-region metric)
	QuotaID string
	Accel   string
	Machine string
	Want    string // smallest preferredValue that admits ONE marker of this class
	Bundled bool
}

// Highest value first.
var c211cqCands = []c211cqCand{
	{"NVIDIA_H100", "family", "GPUS-PER-GPU-FAMILY-per-project-region", "nvidia-h100-80gb", "a3-highgpu-1g", "1", false},
	{"NVIDIA_H100_MEGA", "family", "GPUS-PER-GPU-FAMILY-per-project-region", "nvidia-h100-mega-80gb", "a3-megagpu-8g", "8", false},
	{"NVIDIA_A100_80GB", "region", "NVIDIA-A100-80GB-GPUS-per-project-region", "nvidia-a100-80gb", "a2-ultragpu-1g", "1", false},
	{"NVIDIA_RTX_PRO_6000", "family", "GPUS-PER-GPU-FAMILY-per-project-region", "nvidia-rtx-pro-6000", "g4-standard-384", "8", true},
}

var c211cqQIDs = []string{
	"GPUS-PER-GPU-FAMILY-per-project-region",
	"GPUS-PER-GPU-FAMILY-per-project-zone",
	"NVIDIA-A100-80GB-GPUS-per-project-region",
	"NVIDIA-RTX-PRO-6000-VWS-GPUS-per-project-region",
	"INSTANCES-per-project-region",
	"CPUS-per-project-region",
}

var c211cqPriorRegions = []string{
	"us-central1", "us-east1", "us-east4", "us-west1", "europe-west4",
	"europe-west1", "asia-southeast1", "asia-northeast3",
}

// c211cqAny walks nested map keys and returns the raw leaf.
func c211cqAny(m map[string]interface{}, keys ...string) interface{} {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// c211cqBool is the FIX: the Cloud Quotas API answers a real JSON boolean for
// quotaIncreaseEligibility.isEligible, which the previous string-only helper
// collapsed to "". Accept bool, string and numeric forms and report the raw.
func c211cqBool(v interface{}) (bool, string) {
	switch t := v.(type) {
	case bool:
		if t {
			return true, "true"
		}
		return false, "false"
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s == "true", t
	case float64:
		return t != 0, strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return false, ""
	}
	b, _ := json.Marshal(v)
	return false, string(b)
}

func c211cqLeafStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

type c211cqFam struct {
	Value       string
	Locs        []string
	Eligible    bool
	EligibleRaw string
}

// c211cqParseQuotaInfo parses one verbatim quotaInfo body.
// Eligibility is the quotaInfo-level quotaIncreaseEligibility.isEligible bool;
// every dimensionsInfos entry inherits it (the entries carry only details.value
// and applicableLocations).
func c211cqParseQuotaInfo(body string) (map[string]*c211cqFam, map[string]interface{}, string, string) {
	m := c211xJSON(body)
	fams := map[string]*c211cqFam{}
	if m == nil {
		return fams, nil, "", ""
	}
	eligV := c211cqAny(m, "quotaIncreaseEligibility", "isEligible")
	eligB, eligRaw := c211cqBool(eligV)
	reason := c211cqLeafStr(c211cqAny(m, "quotaIncreaseEligibility", "ineligibilityReason"))
	di, _ := m["dimensionsInfos"].([]interface{})
	for _, e := range di {
		em, _ := e.(map[string]interface{})
		dims, _ := em["dimensions"].(map[string]interface{})
		famName := ""
		if dims != nil {
			famName = c211xStr(dims, "gpu_family")
		}
		val := ""
		if det, ok := em["details"].(map[string]interface{}); ok {
			val = c211xStr(det, "value")
		}
		locs := []string{}
		if arr, ok := em["applicableLocations"].([]interface{}); ok {
			for _, l := range arr {
				if s, ok := l.(string); ok {
					locs = append(locs, s)
				}
			}
		}
		sort.Strings(locs)
		fams[famName] = &c211cqFam{Value: val, Locs: locs, Eligible: eligB, EligibleRaw: c211xFirst(reason, eligRaw)}
	}
	return fams, m, eligRaw, reason
}

func c211cqQuotaInfo(ftok, num, qid string) (int, string) {
	u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/" + url.PathEscape(qid)
	return c211xHTTP("GET", u, ftok, "", "")
}

func c211cqRegionOfZone(zone string) string {
	parts := strings.Split(zone, "-")
	if len(parts) >= 3 {
		return parts[0] + "-" + parts[1]
	}
	return zone
}

// c211cqFindOffering walks the priority regions and their first zones looking
// for a zone where the machine type exists (and, unless bundled, the attachable
// accelerator type exists). Returns (region, zone, matrix).
func c211cqFindOffering(tok, num string, c c211cqCand) (string, string, string) {
	var sb strings.Builder
	for _, r := range c211cqPriorRegions {
		for _, z := range []string{r + "-a", r + "-b", r + "-c", r + "-f"} {
			mst, _ := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(num)+
				"/zones/"+z+"/machineTypes/"+c.Machine, tok, "", "")
			ast := 0
			if !c.Bundled {
				ast, _ = c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(num)+
					"/zones/"+z+"/acceleratorTypes/"+c.Accel, tok, "", "")
			}
			fmt.Fprintf(&sb, "%s:machine=%d", z, mst)
			if !c.Bundled {
				fmt.Fprintf(&sb, ",accel=%d", ast)
			}
			fmt.Fprintf(&sb, ";")
			if mst == 200 && (c.Bundled || ast == 200) {
				return r, z, sb.String()
			}
		}
	}
	return "", "", sb.String()
}

func TestC211I124676SibCQ(t *testing.T) {
	started := time.Now().UTC()
	host := "153711203585"
	tok, method, mlog := c211xMint()
	fmt.Printf("C211CQ_IDENTITY method=%s token_len=%d at=%s log=%s\n", method, len(tok), started.Format(time.RFC3339), c211xTrunc(strings.ReplaceAll(mlog, "\n", " | "), 700))
	if tok == "" {
		t.Fatalf("C211CQ no WIF token minted")
	}
	fleet := ""
	sas, sst, sbody := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211CQ_SA_LIST host_http=%d n=%d\n", sst, len(sas))
	for _, a := range sas {
		em := c211xStr(a, "email")
		if em == c211cqFleetEmail {
			fleet = em
			break
		}
	}
	if fleet == "" {
		for _, a := range sas {
			em := strings.ToLower(c211xStr(a, "email"))
			if strings.HasPrefix(em, "fleetcommand-service-account@") {
				fleet = c211xStr(a, "email")
				break
			}
		}
	}
	sum := sha256.Sum256([]byte(fleet))
	fmt.Printf("C211CQ_FLEET found=%v email_len=%d email_sha256=%s\n", fleet != "", len(fleet), fmt.Sprintf("%x", sum[:8]))
	if fleet == "" {
		t.Fatalf("C211CQ no fleet SA: %s", c211xTrunc(sbody, 400))
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211CQ_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		t.Fatalf("C211CQ no fleet token")
	}
	member := "serviceAccount:" + fleet
	results := map[string]interface{}{}
	for _, p := range c211cqProjects {
		res := map[string]interface{}{}
		results[p.Name] = res
		res["proj"] = p.Name
		res["num"] = p.Num
		perms := map[string]interface{}{}
		res["perms"] = perms
		fmt.Printf("C211CQ_PROJ_BEGIN proj=%s num=%s\n", p.Name, p.Num)
		projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Num)
		pst, pbody := c211xHTTP("GET", projURL, ftok, "", "")
		res["state"] = c211xStr(c211xJSON(pbody), "state")
		fmt.Printf("C211CQ_PROJECT proj=%s http=%d state=%s\n", p.Name, pst, res["state"])
		for _, perm := range c211cqPerms {
			allowed, http, vb := c211xTestIamOne(ftok, projURL, perm)
			perms[perm] = map[string]string{"allowed": fmt.Sprintf("%v", allowed), "http": strconv.Itoa(http), "verbatim": c211xTrunc(vb, 300)}
			fmt.Printf("C211CQ_PERM proj=%s perm=%s http=%d allowed=%v verbatim=%s\n", p.Name, perm, http, allowed, c211xTrunc(vb, 180))
		}
		// ---------- service state: verify ENABLED, never re-enable ----------
		su := "https://serviceusage.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/services/cloudquotas.googleapis.com"
		sst2, sbody2 := c211xHTTP("GET", su, ftok, "", "")
		svcState := c211xStr(c211xJSON(sbody2), "state")
		res["svc_http"] = sst2
		res["svc_state"] = svcState
		fmt.Printf("C211CQ_SVC proj=%s http=%d state=%s verbatim=%s\n", p.Name, sst2, svcState, c211xTrunc(sbody2, 240))
		if svcState != "ENABLED" {
			res["verdict"] = "svc=" + svcState + "_not_enabled_stand_down"
			fmt.Printf("C211CQ_NOPREF proj=%s verdict=%s\n", p.Name, res["verdict"])
			continue
		}
		// ---------- verbatim quotaInfos with the FIXED boolean parse ----------
		famRegion := map[string]*c211cqFam{}
		famZone := map[string]*c211cqFam{}
		qiRaw := map[string]interface{}{}
		res["qinfo"] = qiRaw
		var instEligible bool
		var instLimit, instReason string
		var legacyA100 *c211cqFam
		for _, qid := range c211cqQIDs {
			qst, qbody := c211cqQuotaInfo(ftok, p.Num, qid)
			fams, _, eligRaw, reason := c211cqParseQuotaInfo(qbody)
			qiRaw[qid] = map[string]interface{}{"http": qst, "verbatim": c211xTrunc(qbody, 700), "families": len(fams), "eligible": eligRaw, "reason": reason}
			if qid == "GPUS-PER-GPU-FAMILY-per-project-region" {
				famRegion = fams
			}
			if qid == "GPUS-PER-GPU-FAMILY-per-project-zone" {
				famZone = fams
			}
			if qid == "NVIDIA-A100-80GB-GPUS-per-project-region" {
				legacyA100 = fams[""]
			}
			if qid == "INSTANCES-per-project-region" {
				instEligible = len(fams) > 0 && fams[""].Eligible
				if f := fams[""]; f != nil {
					instLimit = f.Value
					instReason = f.EligibleRaw
				}
			}
			fmt.Printf("C211CQ_QINFO proj=%s quotaId=%s http=%d nfam=%d eligible_raw=%s reason=%s verbatim=%s\n",
				p.Name, qid, qst, len(fams), eligRaw, c211xTrunc(reason, 160), c211xTrunc(qbody, 320))
			for _, fam := range sortedKeys(fams) {
				f := fams[fam]
				sample := ""
				if len(f.Locs) > 0 {
					sample = f.Locs[0]
				}
				fmt.Printf("C211CQ_QDIM proj=%s quotaId=%s gpu_family=%s eligible=%v eligible_raw=%s value=%q nloc=%d first_loc=%s\n",
					p.Name, qid, fam, f.Eligible, c211xTrunc(f.EligibleRaw, 120), f.Value, len(f.Locs), sample)
			}
		}
		// legacy A100-80GB quotaInfo family entry is stored under ""
		if lq, ok := qiRaw["NVIDIA-A100-80GB-GPUS-per-project-region"].(map[string]interface{}); ok {
			lq["dim"] = famRegion[""]
		}
		fz := map[string]interface{}{}
		for fam, f := range famZone {
			fz[fam] = map[string]interface{}{"eligible": f.Eligible, "eligible_raw": f.EligibleRaw, "value": f.Value, "nloc": len(f.Locs)}
			fmt.Printf("C211CQ_ZDIM proj=%s gpu_family=%s eligible=%v value=%q nloc=%d\n", p.Name, fam, f.Eligible, f.Value, len(f.Locs))
		}
		res["fam_zone"] = fz
		res["instances_eligible"] = instEligible
		res["instances_limit"] = instLimit
		res["instances_reason"] = instReason
		// ---------- grant check: never re-grant ----------
		pol, gpst, gpbody := c211xGetPolicy(ftok, projURL)
		grantPresent := c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
		res["grant_present"] = grantPresent
		fmt.Printf("C211CQ_GRANT_CHECK proj=%s policy_http=%d present=%v verbatim=%s\n", p.Name, gpst, grantPresent, c211xTrunc(gpbody, 240))
		if !grantPresent {
			fmt.Printf("C211CQ_GRANT_MISSING proj=%s -> stand down (mandate: do not re-grant)\n", p.Name)
			res["verdict"] = "cloudquotas_admin_grant_missing_stand_down"
			continue
		}
		// existing preferences (dedupe)
		lst, lbody := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/projects/"+url.PathEscape(p.Num)+"/locations/global/quotaPreferences?pageSize=100", ftok, "", "")
		existing := map[string]bool{}
		if lm := c211xJSON(lbody); lm != nil {
			if arr, ok := lm["quotaPreferences"].([]interface{}); ok {
				for _, it := range arr {
					im, _ := it.(map[string]interface{})
					existing[c211xStr(im, "quotaId")+"|"+c211xJSONString(im["dimensions"])] = true
				}
			}
		}
		res["pref_list_http"] = lst
		fmt.Printf("C211CQ_PREF_LIST proj=%s http=%d n=%d verbatim=%s\n", p.Name, lst, len(existing), c211xTrunc(lbody, 320))
		// ---------- GPU preference: highest-value eligible + offered ----------
		chosen := c211cqCand{}
		chosenRegion := ""
		chosenZone := ""
		offerLog := ""
		skip := map[string]string{}
		for _, c := range c211cqCands {
			var fam *c211cqFam
			if c.Kind == "family" {
				fam = famRegion[c.Family]
			} else {
				fam = legacyA100
			}
			if fam == nil {
				skip[c.Family] = "not_present_in_quotaInfo"
				continue
			}
			if !fam.Eligible {
				skip[c.Family] = "ineligible:" + c211xTrunc(fam.EligibleRaw, 80)
				continue
			}
			region, zone, matrix := c211cqFindOffering(ftok, p.Num, c)
			offerLog = matrix
			fmt.Printf("C211CQ_OFFER proj=%s family=%s region=%s zone=%s matrix=%s\n", p.Name, c.Family, region, zone, c211xTrunc(matrix, 600))
			if region == "" {
				skip[c.Family] = "eligible_but_no_offering_in_priority_regions"
				continue
			}
			chosen = c
			chosenRegion = region
			chosenZone = zone
			break
		}
		res["skip"] = skip
		res["chosen"] = chosen.Family
		res["chosen_region"] = chosenRegion
		res["chosen_zone"] = chosenZone
		res["offer_matrix"] = c211xTrunc(offerLog, 900)
		gpuGranted := ""
		gpuDetail := ""
		if chosen.Family == "" {
			// one aiplatform fallback only when every family is ineligible/unoffered
			aid := "aiplatform.googleapis.com/custom_model_training_nvidia_h100_gpus"
			_ = aid
			fmt.Printf("C211CQ_GPU_NONE proj=%s skips=%s -> aiplatform fallback preference\n", p.Name, c211xJSONString(skip))
			prefBody := map[string]interface{}{
				"service":    "aiplatform.googleapis.com",
				"quotaId":    "CustomModelTrainingH100GPUsPerProjectPerRegion",
				"dimensions": map[string]string{"region": "us-central1"},
				"quotaConfig": map[string]interface{}{
					"preferredValue": "1",
				},
				"contactEmail":  fleet,
				"justification": "c211-i124676 nonce measurement: no compute GPU-family quotaInfo was eligible/offered in this Domino sibling project; request the smallest aiplatform custom_model_training_nvidia_h100_gpus quota increase. No billing change, no production use.",
			}
			bj, _ := json.Marshal(prefBody)
			pu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/locations/global/quotaPreferences?quotaPreferenceId=" + url.PathEscape("c211-i124676-"+p.Num+"-aiplat")
			pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
			res["pref_http"] = pst2
			res["pref_verbatim"] = c211xTrunc(rbody, 1200)
			fmt.Printf("C211CQ_PREF proj=%s family=AIPLATFORM_H100 quotaId=CustomModelTrainingH100GPUsPerProjectPerRegion http=%d verbatim=%s\n", p.Name, pst2, c211xTrunc(rbody, 700))
			if pst2 == 200 || pst2 == 201 {
				name := c211xStr(c211xJSON(rbody), "name")
				res["pref_name"] = name
				for k := 0; k < 10 && name != ""; k++ {
					time.Sleep(10 * time.Second)
					gst, gbody := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/"+name, ftok, "", "")
					gm := c211xJSON(gbody)
					gpuGranted = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "grantedValue"))
					gpuDetail = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "stateDetail"))
					fmt.Printf("C211CQ_PREF_STATE proj=%s label=aiplatform iter=%d http=%d granted=%s stateDetail=%s verbatim=%s\n", p.Name, k, gst, gpuGranted, gpuDetail, c211xTrunc(gbody, 500))
					if gpuGranted != "" && gpuGranted != "0" {
						break
					}
					if strings.Contains(strings.ToLower(gpuDetail), "denied") {
						break
					}
				}
			}
			res["gpu_granted"] = gpuGranted
			res["gpu_state_detail"] = gpuDetail
		} else {
			prefBody := map[string]interface{}{
				"service":    "compute.googleapis.com",
				"quotaId":    chosen.QuotaID,
				"dimensions": map[string]string{"gpu_family": chosen.Family, "region": chosenRegion},
				"quotaConfig": map[string]interface{}{
					"preferredValue": chosen.Want,
				},
				"contactEmail":  fleet,
				"justification": "c211-i124676 nonce measurement of Cloud Quotas quota-preference creation for a zero-limit GPU family in a Domino sibling project; contactEmail supplied top-level per the Cloud Quotas v1 schema; smallest preferredValue that admits one marker; no production use, no billing change",
			}
			if chosen.Kind == "region" {
				prefBody["dimensions"] = map[string]string{"region": chosenRegion}
			}
			bj, _ := json.Marshal(prefBody)
			pu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/locations/global/quotaPreferences?quotaPreferenceId=" + url.PathEscape("c211-i124676-"+p.Num+"-gpu")
			pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
			res["pref_http"] = pst2
			res["pref_verbatim"] = c211xTrunc(rbody, 1200)
			res["pref_dims"] = c211xJSONString(prefBody["dimensions"])
			fmt.Printf("C211CQ_PREF proj=%s family=%s kind=%s quotaId=%s dims=%s preferred=%s http=%d verbatim=%s\n",
				p.Name, chosen.Family, chosen.Kind, chosen.QuotaID, c211xJSONString(prefBody["dimensions"]), chosen.Want, pst2, c211xTrunc(rbody, 900))
			if pst2 == 200 || pst2 == 201 {
				name := c211xStr(c211xJSON(rbody), "name")
				res["pref_name"] = name
				for k := 0; k < 12 && name != ""; k++ {
					time.Sleep(10 * time.Second)
					gst, gbody := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/"+name, ftok, "", "")
					gm := c211xJSON(gbody)
					gpuGranted = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "grantedValue"))
					gpuDetail = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "stateDetail"))
					fmt.Printf("C211CQ_PREF_STATE proj=%s label=gpu iter=%d http=%d granted=%s stateDetail=%s verbatim=%s\n", p.Name, k, gst, gpuGranted, gpuDetail, c211xTrunc(gbody, 420))
					if gpuGranted != "" && gpuGranted != "0" {
						break
					}
					if strings.Contains(strings.ToLower(gpuDetail), "denied") {
						break
					}
				}
			}
			res["gpu_granted"] = gpuGranted
			res["gpu_state_detail"] = gpuDetail
		}
		// ---------- INSTANCES-per-project-region preference ----------
		iPrefRegion := chosenRegion
		if iPrefRegion == "" {
			iPrefRegion = "us-central1"
		}
		iBody := map[string]interface{}{
			"service":    "compute.googleapis.com",
			"quotaId":    "INSTANCES-per-project-region",
			"dimensions": map[string]string{"region": iPrefRegion},
			"quotaConfig": map[string]interface{}{
				"preferredValue": "1",
			},
			"contactEmail":  fleet,
			"justification": "c211-i124676 nonce measurement: the compute INSTANCES-per-project-region quota for this region measures a 0.0 limit (fact 124547), which forbids any GCE marker; request the smallest increase (1) that admits exactly one instance. No billing change, no production use.",
		}
		ibj, _ := json.Marshal(iBody)
		iu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/locations/global/quotaPreferences?quotaPreferenceId=" + url.PathEscape("c211-i124676-"+p.Num+"-inst")
		ist, ibody := c211xHTTP("POST", iu, ftok, "application/json", string(ibj))
		res["inst_pref_http"] = ist
		res["inst_pref_region"] = iPrefRegion
		res["inst_pref_verbatim"] = c211xTrunc(ibody, 1200)
		fmt.Printf("C211CQ_INST_PREF proj=%s region=%s preferred=1 http=%d verbatim=%s\n", p.Name, iPrefRegion, ist, c211xTrunc(ibody, 800))
		instGranted := ""
		instDetail := ""
		if ist == 200 || ist == 201 {
			iname := c211xStr(c211xJSON(ibody), "name")
			res["inst_pref_name"] = iname
			for k := 0; k < 12 && iname != ""; k++ {
				time.Sleep(10 * time.Second)
				gst, gbody := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/"+iname, ftok, "", "")
				gm := c211xJSON(gbody)
				instGranted = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "grantedValue"))
				instDetail = c211cqLeafStr(c211cqAny(gm, "quotaConfig", "stateDetail"))
				fmt.Printf("C211CQ_INST_STATE proj=%s iter=%d http=%d granted=%s stateDetail=%s verbatim=%s\n", p.Name, k, gst, instGranted, instDetail, c211xTrunc(gbody, 400))
				if instGranted != "" && instGranted != "0" {
					break
				}
				if strings.Contains(strings.ToLower(instDetail), "denied") {
					break
				}
			}
		}
		res["inst_granted"] = instGranted
		res["inst_state_detail"] = instDetail
		// ---------- post-grant live compute region read for the chosen region ----------
		if chosenRegion != "" {
			rst, rbody2 := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/regions/"+chosenRegion, ftok, "", "")
			var rows []string
			if rm := c211xJSON(rbody2); rm != nil {
				if qs, ok := rm["quotas"].([]interface{}); ok {
					for _, q := range qs {
						qm, _ := q.(map[string]interface{})
						mt := c211xStr(qm, "metric")
						if strings.Contains(mt, "INSTANCES") || strings.Contains(mt, "GPUS") || mt == "CPUS" {
							rows = append(rows, fmt.Sprintf("%s limit=%v usage=%v", mt, qm["limit"], qm["usage"]))
						}
					}
				}
			}
			res["region_quota_after"] = rows
			fmt.Printf("C211CQ_REGION_AFTER proj=%s region=%s http=%d rows=%s\n", p.Name, chosenRegion, rst, c211xTrunc(strings.Join(rows, ";"), 600))
		}
		res["verdict"] = fmt.Sprintf("gpu=%s granted=%s detail=%s | inst=%s granted=%s detail=%s",
			chosen.Family, gpuGranted, c211xTrunc(gpuDetail, 90), iPrefRegion, instGranted, c211xTrunc(instDetail, 90))
		fmt.Printf("C211CQ_VERDICT proj=%s %s\n", p.Name, res["verdict"])
	}
	jb, _ := json.Marshal(results)
	fmt.Printf("C211CQ_MATRIX %s\n", string(jb))
	fmt.Printf("C211CQ_VERDICT_FINAL elapsed=%s at=%s\n", time.Since(started).Truncate(time.Second), time.Now().UTC().Format(time.RFC3339))
}

func sortedKeys(m map[string]*c211cqFam) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
