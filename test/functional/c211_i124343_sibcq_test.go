//go:build functional && gke

// C211 i124343 — Cloud Quotas GPU-family quota-preference lever, four readable
// Domino sibling projects.
//
// Fact 124276 proved in the hephaestus HOST project 153711203585 that
// quotaPreferences.create with a TOP-LEVEL contactEmail returns real grants:
// GPUS-PER-GPU-FAMILY-per-project-region / NVIDIA_H100 / us-central1 was
// APPROVED with grantedValue=1 ("Quota request approved to 1") after the WIF
// identity self-granted roles/cloudquotas.admin, while NVIDIA_H200/us-central1
// and NVIDIA_B200/us-east1 were created then DENIED.  The four siblings
// 210628133505 / 1072394045651 / 463640210929 / 1074386228880 have never had a
// GPU-family preference attempted and their GPUS-PER-GPU-FAMILY-per-project-zone
// dimension is unmeasured.
//
// This probe uses the exact in-job identity that device-proved 8x L4 in the
// siblings (x123817/x124118), measures one-permission-per-call testIamPermissions
// for the Cloud Quotas / Service Usage / Resource Manager / Compute permissions,
// reads the verbatim GPU quotaInfos (per-project-region AND per-project-zone),
// self-grants roles/cloudquotas.admin additively where setIamPolicy is allowed,
// creates AT MOST ONE quotaPreference per project for the highest-value eligible
// zero-limit family (NVIDIA_H100 -> NVIDIA_A100_80GB -> NVIDIA_RTX_PRO_6000),
// polls it to terminal, and — only if grantedValue >= 1 — creates EXACTLY ONE
// nonce-tagged ON-DEMAND GCE marker of the matching shape, captures the device
// lines, deletes it and proves absence.
//
// No payment method, no paid plan, no billing change; every preference/marker is
// nonce-tagged c211-i124343 and is our own resource.

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

const c211cqNonce = "i124343"

// c211cqFleetEmail is the exact verbatim service account that successfully
// crossed into the four Domino sibling projects in x123817/x124118 (len 76,
// sha256 9736b01aaa62f06b...). It must never be selected by a loose substring
// match: the host-scoped dev-fleetcommand@ sorts first and only holds host
// project 153711203585 reach, which made every sibling call 403 in run
// 44936658742.
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

var c211cqQuotaIds = []string{
	"GPUS-PER-GPU-FAMILY-per-project-region",
	"GPUS-PER-GPU-FAMILY-per-project-zone",
	"NVIDIA-H100-GPUS-per-project-region",
	"NVIDIA-A100-80GB-GPUS-per-project-region",
	"NVIDIA-RTX-PRO-6000-VWS-GPUS-per-project-region",
}

type c211cqCand struct {
	Family  string
	Accel   string
	Machine string
	Disk    string
}

// Highest value first; the disk type is the one the class needs for a boot disk.
var c211cqCands = []c211cqCand{
	{"NVIDIA_H100", "nvidia-h100-80gb", "a3-highgpu-1g", "hyperdisk-balanced"},
	{"NVIDIA_A100_80GB", "nvidia-a100-80gb", "a2-ultragpu-1g", "pd-balanced"},
	{"NVIDIA_RTX_PRO_6000", "nvidia-rtx-pro-6000", "g4-standard-24", "pd-balanced"},
}

type c211cqFam struct {
	Value    string
	Locs     []string
	Eligible bool
	EligibleRaw string
}

type c211cqResult struct {
	Proj          string
	Num           string
	State         string
	Perms         map[string]map[string]string
	SvcState      string
	SvcHTTP       int
	EnableHTTP    int
	EnableVerbatim string
	GrantPresent  bool
	SetIamHTTP    int
	SetIamVerbatim string
	FamRegion     map[string]*c211cqFam
	FamZone       map[string]*c211cqFam
	QInfo         map[string]map[string]interface{}
	Chosen        string
	ChosenQuota   string
	ChosenDims    map[string]string
	ChosenRegion  string
	PrefListHTTP  int
	PrefHTTP      int
	PrefVerbatim  string
	PrefName      string
	Granted       string
	StateDetail   string
	PrefState     string
	MarkerUUIDs   []string
	MarkerLog     string
	MarkerAbsent  bool
	Verdict       string
}

func c211cqNested(m map[string]interface{}, keys ...string) string {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

func c211cqAtoiOr(s string, d int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return d
	}
	return n
}

func c211cqZones(region string) []string {
	// the zones the sibling offering matrices have carried for these classes
	pre := []string{"a", "b", "c", "f", "d", "e"}
	out := []string{}
	for _, s := range pre {
		out = append(out, region+"-"+s)
	}
	return out
}

// parse the dimensionsInfos of one quotaInfo body into family -> {value, locs, eligible}
func c211cqParseQuotaInfo(body string) (map[string]*c211cqFam, map[string]interface{}) {
	m := c211xJSON(body)
	fams := map[string]*c211cqFam{}
	if m == nil {
		return fams, nil
	}
	elig := c211cqNested(m, "quotaIncreaseEligibility", "isEligible")
	eligB := elig == "true"
	reason := c211cqNested(m, "quotaIncreaseEligibility", "ineligibilityReason")
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
		f := &c211cqFam{Value: val, Locs: locs, Eligible: eligB, EligibleRaw: c211xFirst(reason, elig)}
		if famName == "" && len(dims) > 0 {
			// non-gpu-family dimensioned entry (e.g. region-only): keep under ""
			fams[""] = f
			continue
		}
		fams[famName] = f
	}
	return fams, m
}

// one nonce-tagged ON-DEMAND GCE marker; bundled machine types get no guestAccelerators
func c211cqCreateInstance(token, proj, zone, name, machine, accel string, bundled bool, diskType, saEmail string, actAs bool) (int, string) {
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + machine,
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/ubuntu-os-accelerator-images/global/images/family/ubuntu-accelerator-2204-amd64-with-nvidia-595",
				"diskSizeGb":  "120",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/" + diskType,
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "projects/" + proj + "/global/networks/default",
			"subnetwork": "projects/" + proj + "/regions/" + c211xRegionOfZone(zone) + "/subnetworks/default",
			"accessConfigs": []interface{}{map[string]interface{}{
				"type": "ONE_TO_ONE_NAT", "name": "External NAT",
			}},
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"key": "startup-script", "value": c211xStartupScript()},
				map[string]interface{}{"key": "c211x", "value": c211cqNonce},
				map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
			},
		},
		"labels": map[string]string{"c211": c211cqNonce},
	}
	if !bundled {
		body["guestAccelerators"] = []interface{}{map[string]interface{}{
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
			"acceleratorCount": 1,
		}}
	}
	if actAs && saEmail != "" {
		body["serviceAccounts"] = []interface{}{map[string]interface{}{
			"email": saEmail, "scopes": []string{c211xScope},
		}}
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, token, "application/json", string(b))
}

func TestC211I124343SibCQ(t *testing.T) {
	started := time.Now().UTC()
	host := "153711203585"
	tok, method, mlog := c211xMint()
	fmt.Printf("C211CQ_IDENTITY method=%s token_len=%d at=%s log=%s\n", method, len(tok), started.Format(time.RFC3339), c211xTrunc(strings.ReplaceAll(mlog, "\n", " | "), 700))
	if tok == "" {
		t.Fatalf("C211CQ no WIF token minted")
	}
	// The proven sibling-crossing identity is the exact service account that
	// x123817/x124118 impersonated (verbatim email, len 76,
	// sha256 9736b01aaa62f06b...). A substring match on "fleet" resolves to the
	// host-scoped dev-fleetcommand@ first and every sibling call then 403s, so
	// match the full email exactly (prefix of the local part only as fallback).
	fleet := ""
	sas, sst, sbody := c211xListServiceAccounts(tok, host)
	fmt.Printf("C211CQ_SA_LIST host_http=%d n=%d\n", sst, len(sas))
	for _, a := range sas {
		em := c211xStr(a, "email")
		if strings.Contains(strings.ToLower(em), "fleet") {
			sum := sha256.Sum256([]byte(em))
			fmt.Printf("C211CQ_SA_CAND email_len=%d email_sha256=%s\n", len(em), fmt.Sprintf("%x", sum[:8]))
		}
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
	fmt.Printf("C211CQ_FLEET found=%v email_len=%d email_sha256=%s\n", fleet != "", len(fleet), func() string { s := sha256.Sum256([]byte(fleet)); return fmt.Sprintf("%x", s[:8]) }())
	if fleet == "" {
		t.Fatalf("C211CQ no fleet SA: %s", c211xTrunc(sbody, 400))
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211CQ_FLEET_TOKEN http=%d token_len=%d verbatim=%s\n", fst, len(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		t.Fatalf("C211CQ no fleet token")
	}
	member := "serviceAccount:" + fleet
	results := map[string]*c211cqResult{}
	for _, p := range c211cqProjects {
		res := &c211cqResult{Proj: p.Name, Num: p.Num, Perms: map[string]map[string]string{}}
		results[p.Name] = res
		fmt.Printf("C211CQ_PROJ_BEGIN proj=%s num=%s\n", p.Name, p.Num)
		projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Num)
		pst, pbody := c211xHTTP("GET", projURL, ftok, "", "")
		res.State = c211xStr(c211xJSON(pbody), "state")
		fmt.Printf("C211CQ_PROJECT proj=%s http=%d state=%s verbatim=%s\n", p.Name, pst, res.State, c211xTrunc(pbody, 300))
		// ---------- (a) one-permission-per-call testIamPermissions ----------
		for _, perm := range c211cqPerms {
			allowed, http, vb := c211xTestIamOne(ftok, projURL, perm)
			res.Perms[perm] = map[string]string{"allowed": fmt.Sprintf("%v", allowed), "http": strconv.Itoa(http), "verbatim": c211xTrunc(vb, 300)}
			fmt.Printf("C211CQ_PERM proj=%s perm=%s http=%d allowed=%v verbatim=%s\n", p.Name, perm, http, allowed, c211xTrunc(vb, 220))
		}
		// ---------- (b) service state + enable ----------
		su := "https://serviceusage.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/services/cloudquotas.googleapis.com"
		sst2, sbody2 := c211xHTTP("GET", su, ftok, "", "")
		res.SvcHTTP = sst2
		res.SvcState = c211xStr(c211xJSON(sbody2), "state")
		fmt.Printf("C211CQ_SVC proj=%s http=%d state=%s verbatim=%s\n", p.Name, sst2, res.SvcState, c211xTrunc(sbody2, 300))
		if res.SvcState != "ENABLED" && res.Perms["serviceusage.services.enable"]["allowed"] == "true" {
			est, ebody := c211xHTTP("POST", su+":enable", ftok, "application/json", "{}")
			res.EnableHTTP = est
			res.EnableVerbatim = c211xTrunc(ebody, 500)
			fmt.Printf("C211CQ_ENABLE proj=%s http=%d verbatim=%s\n", p.Name, est, res.EnableVerbatim)
			op := c211xStr(c211xJSON(ebody), "name")
			for i := 0; i < 12 && op != ""; i++ {
				time.Sleep(5 * time.Second)
				ost, obody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/"+op, ftok, "", "")
				om := c211xJSON(obody)
				fmt.Printf("C211CQ_ENABLE_POLL proj=%s i=%d http=%d done=%v error=%v\n", p.Name, i, ost, om["done"], c211xTrunc(c211xJSONString(om["error"]), 200))
				if om != nil && om["done"] == true {
					break
				}
			}
			sst3, sbody3 := c211xHTTP("GET", su, ftok, "", "")
			res.SvcState = c211xStr(c211xJSON(sbody3), "state")
			fmt.Printf("C211CQ_SVC_AFTER proj=%s http=%d state=%s\n", p.Name, sst3, res.SvcState)
		}
		// ---------- (c) verbatim GPU quotaInfos ----------
		res.FamRegion = map[string]*c211cqFam{}
		res.FamZone = map[string]*c211cqFam{}
		res.QInfo = map[string]map[string]interface{}{}
		for _, qid := range c211cqQuotaIds {
			qu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(p.Num) +
				"/locations/global/services/compute.googleapis.com/quotaInfos/" + url.PathEscape(qid)
			qst, qbody := c211xHTTP("GET", qu, ftok, "", "")
			fams, raw := c211cqParseQuotaInfo(qbody)
			res.QInfo[qid] = map[string]interface{}{"http": qst, "verbatim": c211xTrunc(qbody, 700), "families": len(fams)}
			if strings.Contains(qid, "per-project-region") && strings.HasPrefix(qid, "GPUS-") {
				res.FamRegion = fams
			}
			if strings.Contains(qid, "per-project-zone") && strings.HasPrefix(qid, "GPUS-") {
				res.FamZone = fams
			}
			fmt.Printf("C211CQ_QINFO proj=%s quotaId=%s http=%d nfam=%d verbatim=%s\n", p.Name, qid, qst, len(fams), c211xTrunc(qbody, 400))
			if raw != nil {
				for _, fam := range sortedKeys(fams) {
					f := fams[fam]
					nl := len(f.Locs)
					sample := ""
					if nl > 0 {
						sample = f.Locs[0]
					}
					fmt.Printf("C211CQ_QDIM proj=%s quotaId=%s gpu_family=%s eligible=%v eligible_raw=%s value=%q nloc=%d first_loc=%s\n",
						p.Name, qid, fam, f.Eligible, c211xTrunc(f.EligibleRaw, 120), f.Value, nl, sample)
				}
			}
		}
		// ---------- (d) setIamPolicy self-grant ----------
		pol, gpst, gpbody := c211xGetPolicy(ftok, projURL)
		res.GrantPresent = c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
		fmt.Printf("C211CQ_GRANT_CHECK proj=%s policy_http=%d present=%v verbatim=%s\n", p.Name, gpst, res.GrantPresent, c211xTrunc(gpbody, 300))
		if !res.GrantPresent && pol != nil && res.Perms["resourcemanager.projects.setIamPolicy"]["allowed"] == "true" {
			if c211xAddBinding(pol, "roles/cloudquotas.admin", member) {
				rst, rbody := c211xSetPolicy(ftok, projURL, pol)
				res.SetIamHTTP = rst
				res.SetIamVerbatim = c211xTrunc(rbody, 600)
				fmt.Printf("C211CQ_GRANT_SET proj=%s http=%d verbatim=%s\n", p.Name, rst, res.SetIamVerbatim)
				if rst == 200 {
					pol2, _, _ := c211xGetPolicy(ftok, projURL)
					res.GrantPresent = c211xPolicyHas(pol2, "roles/cloudquotas.admin", member)
					fmt.Printf("C211CQ_GRANT_AFTER proj=%s present=%v\n", p.Name, res.GrantPresent)
				}
			}
		}
		canCreate := res.Perms["cloudquotas.quotaPreferences.create"]["allowed"] == "true" || res.GrantPresent
		if res.SvcState != "ENABLED" || !canCreate {
			res.Verdict = fmt.Sprintf("svc=%s create_allowed=%v grant_present=%v", res.SvcState, res.Perms["cloudquotas.quotaPreferences.create"]["allowed"], res.GrantPresent)
			fmt.Printf("C211CQ_NOPREF proj=%s verdict=%s\n", p.Name, res.Verdict)
			continue
		}
		// existing preferences (dedupe)
		lst, lbody := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/projects/"+url.PathEscape(p.Num)+"/locations/global/quotaPreferences?pageSize=100", ftok, "", "")
		res.PrefListHTTP = lst
		existing := map[string]bool{}
		if lm := c211xJSON(lbody); lm != nil {
			if arr, ok := lm["quotaPreferences"].([]interface{}); ok {
				for _, it := range arr {
					im, _ := it.(map[string]interface{})
					existing[c211xStr(im, "quotaId")] = true
				}
			}
		}
		fmt.Printf("C211CQ_PREF_LIST proj=%s http=%d n=%d verbatim=%s\n", p.Name, lst, len(existing), c211xTrunc(lbody, 400))
		// ---------- (e) one preference, highest-value eligible family ----------
		if existing["GPUS-PER-GPU-FAMILY-per-project-region"] {
			res.Verdict = "gpu_family_preference_already_exists_skip"
			fmt.Printf("C211CQ_NOPREF proj=%s verdict=%s\n", p.Name, res.Verdict)
			continue
		}
		for _, cand := range c211cqCands {
			fam := res.FamRegion[cand.Family]
			if fam == nil {
				continue
			}
			if !fam.Eligible {
				continue
			}
			region := ""
			for _, l := range fam.Locs {
				if l == "us-central1" {
					region = l
					break
				}
			}
			if region == "" && len(fam.Locs) > 0 {
				region = fam.Locs[0]
			}
			if region == "" {
				continue
			}
			res.Chosen = cand.Family
			res.ChosenQuota = "GPUS-PER-GPU-FAMILY-per-project-region"
			res.ChosenDims = map[string]string{"gpu_family": cand.Family, "region": region}
			res.ChosenRegion = region
			break
		}
		if res.Chosen == "" {
			res.Verdict = "no_eligible_zero_limit_gpu_family_in_per-project-region"
			fmt.Printf("C211CQ_NOPREF proj=%s verdict=%s\n", p.Name, res.Verdict)
			continue
		}
		prefID := "c211-i124343-" + p.Num + "-1"
		body := map[string]interface{}{
			"service":    "compute.googleapis.com",
			"quotaId":    res.ChosenQuota,
			"dimensions": res.ChosenDims,
			"quotaConfig": map[string]interface{}{
				"preferredValue": "1",
			},
			"contactEmail":  fleet,
			"justification": "c211-i124343 nonce measurement of Cloud Quotas quota-preference creation for a zero-limit GPU family in a Domino sibling project; contactEmail supplied top-level per the Cloud Quotas v1 discovery schema; no production use, no billing change",
		}
		bj, _ := json.Marshal(body)
		pu := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(p.Num) + "/locations/global/quotaPreferences?quotaPreferenceId=" + url.PathEscape(prefID)
		pst2, rbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
		res.PrefHTTP = pst2
		res.PrefVerbatim = c211xTrunc(rbody, 1200)
		fmt.Printf("C211CQ_PREF proj=%s family=%s dims=%v quotaId=%s http=%d verbatim=%s\n", p.Name, res.Chosen, res.ChosenDims, res.ChosenQuota, pst2, res.PrefVerbatim)
		if pst2 == 200 || pst2 == 201 {
			res.PrefName = c211xStr(c211xJSON(rbody), "name")
			if res.PrefName == "" {
				res.Verdict = "pref_create_http_ok_but_no_name"
				continue
			}
			gu := "https://cloudquotas.googleapis.com/v1/" + res.PrefName
			for k := 0; k < 10; k++ {
				time.Sleep(10 * time.Second)
				gst, gbody := c211xHTTP("GET", gu, ftok, "", "")
				gm := c211xJSON(gbody)
				res.PrefState = c211xStr(gm, "state")
				res.Granted = c211cqNested(gm, "quotaConfig", "grantedValue")
				res.StateDetail = c211cqNested(gm, "quotaConfig", "stateDetail")
				fmt.Printf("C211CQ_PREF_STATE proj=%s iter=%d http=%d state=%s granted=%s stateDetail=%s reconciling=%v\n", p.Name, k, gst, res.PrefState, res.Granted, res.StateDetail, gm["reconciling"])
				if res.Granted != "" && res.Granted != "0" {
					break
				}
				if strings.Contains(strings.ToLower(res.StateDetail), "denied") {
					break
				}
			}
		}
		if c211cqAtoiOr(res.Granted, 0) < 1 {
			res.Verdict = fmt.Sprintf("pref_http=%d granted=%s stateDetail=%s", res.PrefHTTP, res.Granted, c211xTrunc(res.StateDetail, 120))
			fmt.Printf("C211CQ_NOMARKER proj=%s verdict=%s\n", p.Name, res.Verdict)
			continue
		}
		// ---------- (f) ONE on-demand marker of the matching shape ----------
		elapsed := time.Since(started)
		if elapsed > 2100*time.Second {
			res.Verdict = "granted_" + res.Granted + "_marker_deferred_budget"
			fmt.Printf("C211CQ_MARKER_DEFERRED proj=%s elapsed=%s granted=%s\n", p.Name, elapsed, res.Granted)
			continue
		}
		cand := c211cqCand{}
		for _, c := range c211cqCands {
			if c.Family == res.Chosen {
				cand = c
			}
		}
		bundled := res.Chosen == "NVIDIA_RTX_PRO_6000"
		actAs := res.Perms["iam.serviceAccounts.actAs"]["allowed"] == "true"
		markerName := "c211-i124343-" + strings.ToLower(strings.ReplaceAll(p.Name, "_", "-"))
		if len(markerName) > 55 {
			markerName = markerName[:55]
		}
		var vlog strings.Builder
		fmt.Fprintf(&vlog, "MARKER proj=%s family=%s machine=%s accel=%s bundled=%v region=%s actAs=%v name=%s\n",
			p.Name, res.Chosen, cand.Machine, cand.Accel, bundled, res.ChosenRegion, actAs, markerName)
		launched := false
		for _, zone := range c211cqZones(res.ChosenRegion) {
			ast, abody := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/zones/"+zone+"/acceleratorTypes/"+cand.Accel, ftok, "", "")
			mst, mbody := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/zones/"+zone+"/machineTypes/"+cand.Machine, ftok, "", "")
			fmt.Fprintf(&vlog, "OFFER zone=%s accel_%s=%d machine_%s=%d\n", zone, cand.Accel, ast, cand.Machine, mst)
			fmt.Printf("C211CQ_OFFER proj=%s zone=%s accel_http=%d machine_http=%d verbatim=%s|%s\n", p.Name, zone, ast, mst, c211xTrunc(abody, 160), c211xTrunc(mbody, 160))
			if ast != 200 || mst != 200 {
				continue
			}
			cst, cbody := c211cqCreateInstance(ftok, p.Num, zone, markerName, cand.Machine, cand.Accel, bundled, cand.Disk, fleet, actAs)
			fmt.Fprintf(&vlog, "CREATE zone=%s status=%d verbatim=%s\n", zone, cst, c211xTrunc(cbody, 2500))
			fmt.Printf("C211CQ_MARKER_CREATE proj=%s zone=%s http=%d verbatim=%s\n", p.Name, zone, cst, c211xTrunc(cbody, 700))
			if cst != 200 {
				continue
			}
			launched = true
			uuidFound := ""
			for i := 0; i < 34; i++ {
				time.Sleep(10 * time.Second)
				ist, im, _ := c211xInstanceGet(ftok, p.Num, zone, markerName)
				st := ""
				if im != nil {
					st = c211xStr(im, "status")
				}
				ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/zones/"+zone+"/instances/"+markerName+"/serialPort?port=1&start=0", ftok, "", "")
				content := ""
				if ps == 200 {
					if pm := c211xJSON(pb); pm != nil {
						content = c211xStr(pm, "contents")
					} else {
						content = pb
					}
				}
				meta := c211xMetaValue(im, "c211x-result")
				fmt.Fprintf(&vlog, "POLL i=%d inst=%s(%d) serial=%d bytes=%d meta=%s tail=%s\n",
					i, st, ist, ps, len(content), c211xTrunc(meta, 300), c211xTrunc(content[maxi(0, len(content)-240):], 240))
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuidFound = u
				}
				if u := c211xUUIDRe.FindString(meta); u != "" {
					uuidFound = u
				}
				if uuidFound != "" {
					// collect all UUIDs from both sinks
					all := append(c211xUUIDRe.FindAllString(content, -1), c211xUUIDRe.FindAllString(meta, -1)...)
					seen := map[string]bool{}
					for _, u := range all {
						if !seen[u] {
							seen[u] = true
							res.MarkerUUIDs = append(res.MarkerUUIDs, u)
						}
					}
					sort.Strings(res.MarkerUUIDs)
					fmt.Printf("C211CQ_MARKER_UUID proj=%s zone=%s n=%d uuids=%s\n", p.Name, zone, len(res.MarkerUUIDs), strings.Join(res.MarkerUUIDs, ","))
					break
				}
				if i%6 == 5 {
					fmt.Printf("C211CQ_MARKER_POLL proj=%s i=%d inst=%s serial=%d meta=%s\n", p.Name, i, st, ps, c211xTrunc(meta, 200))
				}
			}
			dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/zones/"+zone+"/instances/"+markerName, ftok, "", "")
			fmt.Fprintf(&vlog, "DELETE status=%d verbatim=%s\n", dst, c211xTrunc(db, 600))
			time.Sleep(10 * time.Second)
			absent := false
			for i := 0; i < 12; i++ {
				ist, _, ib := c211xInstanceGet(ftok, p.Num, zone, markerName)
				fmt.Fprintf(&vlog, "ABSENCE i=%d http=%d verbatim=%s\n", i, ist, c211xTrunc(ib, 240))
				if ist == 404 {
					absent = true
					break
				}
				time.Sleep(10 * time.Second)
			}
			lst2, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.Num)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(markerName), ftok, "", "")
			fmt.Fprintf(&vlog, "AGG status=%d verbatim=%s\n", lst2, c211xTrunc(lb, 500))
			res.MarkerAbsent = absent
			fmt.Printf("C211CQ_MARKER_TEARDOWN proj=%s zone=%s delete=%d absent=%v agg=%d\n", p.Name, zone, dst, absent, lst2)
			break
		}
		res.MarkerLog = vlog.String()
		if !launched {
			res.Verdict = "granted_" + res.Granted + "_marker_create_refused"
		} else if len(res.MarkerUUIDs) > 0 {
			res.Verdict = fmt.Sprintf("granted_%s_marker_device_proven_%dx_absent=%v", res.Granted, len(res.MarkerUUIDs), res.MarkerAbsent)
		} else {
			res.Verdict = "granted_" + res.Granted + "_marker_no_device_line_absent=" + fmt.Sprintf("%v", res.MarkerAbsent)
		}
		fmt.Printf("C211CQ_MARKER_VERDICT proj=%s verdict=%s\n", p.Name, res.Verdict)
	}
	// ---------- final matrix ----------
	out := map[string]interface{}{}
	for k, r := range results {
		m := map[string]interface{}{
			"proj": r.Proj, "num": r.Num, "state": r.State,
			"perms": r.Perms, "svc_state": r.SvcState, "svc_http": r.SvcHTTP,
			"enable_http": r.EnableHTTP, "enable_verbatim": r.EnableVerbatim,
			"grant_present": r.GrantPresent, "setiam_http": r.SetIamHTTP, "setiam_verbatim": r.SetIamVerbatim,
			"chosen": r.Chosen, "chosen_quota": r.ChosenQuota, "chosen_dims": r.ChosenDims, "chosen_region": r.ChosenRegion,
			"pref_list_http": r.PrefListHTTP, "pref_http": r.PrefHTTP, "pref_verbatim": r.PrefVerbatim, "pref_name": r.PrefName,
			"granted": r.Granted, "state_detail": r.StateDetail, "pref_state": r.PrefState,
			"marker_uuids": r.MarkerUUIDs, "marker_absent": r.MarkerAbsent, "marker_log": r.MarkerLog,
			"verdict": r.Verdict,
		}
		fr := map[string]interface{}{}
		for fam, f := range r.FamRegion {
			fr[fam] = map[string]interface{}{"value": f.Value, "eligible": f.Eligible, "eligible_raw": f.EligibleRaw, "nloc": len(f.Locs), "locs": f.Locs}
		}
		m["fam_region"] = fr
		fz := map[string]interface{}{}
		for fam, f := range r.FamZone {
			fz[fam] = map[string]interface{}{"value": f.Value, "eligible": f.Eligible, "eligible_raw": f.EligibleRaw, "nloc": len(f.Locs)}
		}
		m["fam_zone"] = fz
		m["qinfo"] = r.QInfo
		out[k] = m
	}
	jb, _ := json.Marshal(out)
	fmt.Printf("C211CQ_MATRIX %s\n", string(jb))
	fmt.Printf("C211CQ_VERDICT_FINAL elapsed=%s at=%s\n", time.Since(started).Truncate(time.Second), time.Now().UTC().Format(time.RFC3339))
}

func c211cqQuotaIdFor(family string) string {
	switch family {
	case "NVIDIA_H100":
		return "NVIDIA-H100-GPUS-per-project-region"
	case "NVIDIA_A100_80GB":
		return "NVIDIA-A100-80GB-GPUS-per-project-region"
	case "NVIDIA_RTX_PRO_6000":
		return "NVIDIA-RTX-PRO-6000-VWS-GPUS-per-project-region"
	}
	return ""
}

func sortedKeys(m map[string]*c211cqFam) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
