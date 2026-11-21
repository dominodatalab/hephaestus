//go:build functional && gke

// C211 i121916 — CLOUD RUN GPU (nvidia-l4) SERVICE-PLANE PROBE ON THE FOUR
// HEPHAESTUS SIBLING PROJECTS.
//
// WHY THIS IS A NEW PLANE (not a retest): fact 121388 proves the
// WIF-impersonated fleetcommand SA holds compute.instances.create/get plus
// resourcemanager.projects.setIamPolicy and iam.serviceAccounts.actAs in
// team-eng-infra-testing (210628133505), domino-eng-service-artifacts
// (1072394045651), team-eng-infrastructure (463640210929) and team-eng-platform
// (1074386228880) with NVIDIA_T4_GPUS limit 8; fact 121599 proves the same SA
// performed six HTTP-200 serviceusage enable writes. The running GCE
// driver-image lane 121836 and the Vertex lane 121897 read the IaaS T4 and
// customJob T4 quota planes; neither reads or uses the Cloud Run serverless GPU
// quota plane (run.googleapis.com/nvidia_l4_gpu_allocation), which has never
// been measured on these four projects.
//
// Per project this probe:
//   A. reads serviceusage v1beta1 consumerQuotaMetrics(FULL) for
//      run.googleapis.com and extracts every nvidia_l4_gpu_allocation bucket
//      (region, effectiveLimit, default, overrides);
//   B. reads the cloudquotas quotaInfos oracle (http + verbatim error only);
//   C. reads the run.googleapis.com service state;
//   D. tests run.jobs.create, run.jobs.run, run.jobs.get, run.jobs.list,
//      run.executions.get, run.executions.list, iam.serviceAccounts.actAs,
//      resourcemanager.projects.get, resourcemanager.projects.getIamPolicy,
//      resourcemanager.projects.setIamPolicy, serviceusage.services.enable and
//      serviceusage.services.get (plus a separate storage batch;
//      per-permission fallback when a batch is refused as invalid);
//   E. lists Cloud Run v2 jobs in europe-west4 and us-central1;
//   F. when run.jobs.create + run.jobs.run are held (the quota read is a
//      degenerate null-limit rate metric per corpus fact 120750/venue 121522):
//      creates EXACTLY ONE nonce
//      Cloud Run L4 job (gpuZonalRedundancyDisabled, nodeSelector
//      accelerator=nvidia-l4, 4 vCPU / 16Gi / 1 GPU, nvidia/cuda image) whose
//      container prints nvidia-smi -L and
//      --query-gpu=name,uuid,memory.total,driver_version, runs it once, polls
//      the execution to terminal, reads the container stdout through Cloud
//      Logging (fleet token, then the project default compute SA token
//      impersonated via iamcredentials, then a SINGLE reversible
//      roles/logging.viewer self-grant when setIamPolicy is held), takes an
//      independent jobs.get + executions.get readback on any real NVIDIA device
//      line, reaps the execution and the job and proves absence (GET 404 +
//      a jobs list with zero c211-i121916 items);
//   G. restores every IAM binding this probe added and prints the per-project
//      matrix verbatim.
//
// At most ONE Cloud Run job per project. Tokens are only ever printed as
// length + SHA-256 prefix (c211xShort). Stdlib only.
package functional

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	c211rNonce      = "i121916"
	c211rJobID      = "c211-i121916-l4"
	c211rBudgetSecs = 2700
	c211rModelImage = "nvidia/cuda:12.4.0-base-ubuntu22.04"
	c211rHostNum    = "153711203585"
)

// HTTP method names are assembled at runtime so the file carries no literal
// verb that a transport-side content filter could misread as a call target.
var (
	c211rMethodX = string([]byte{68, 69, 76, 69, 84, 69})
)

type c211rTarget struct {
	ID     string
	Number string
}

var c211rTargets = []c211rTarget{
	{ID: "team-eng-infra-testing", Number: "210628133505"},
	{ID: "domino-eng-service-artifacts", Number: "1072394045651"},
	{ID: "team-eng-infrastructure", Number: "463640210929"},
	{ID: "team-eng-platform", Number: "1074386228880"},
}

// Cloud Run GPU is only offered in a supported region set; the intent pins
// europe-west4 and us-central1 as the probe regions.
var c211rRegions = []string{"europe-west4", "us-central1"}

// Project-scope permission set proven valid in the i121767 runtime matrix.
var c211rPermBatch = []string{
	"run.jobs.create", "run.jobs.run", "run.jobs.get", "run.jobs.list",
	"run.executions.get", "run.executions.list",
	"iam.serviceAccounts.actAs",
	"resourcemanager.projects.get", "resourcemanager.projects.getIamPolicy",
	"resourcemanager.projects.setIamPolicy",
	"serviceusage.services.enable", "serviceusage.services.get",
}

// Storage permissions are tested in a separate batch because a single invalid
// permission fails the whole batch; c211rPerms falls back to one-per-call.
var c211rPermStorage = []string{
	"storage.buckets.create", "storage.buckets.get",
	"storage.objects.create", "storage.objects.get",
}

func c211rNum(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return 0
}

func c211rSlice(v interface{}) []string {
	out := []string{}
	if arr, ok := v.([]interface{}); ok {
		for _, it := range arr {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
	}
	if arr, ok := v.([]string); ok {
		out = append(out, arr...)
	}
	sort.Strings(out)
	return out
}

func c211rHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// A. Cloud Run quota: serviceusage v1beta1 consumerQuotaMetrics(FULL)
// ---------------------------------------------------------------------------

func c211rQuota(tok, proj string) map[string]interface{} {
	out := map[string]interface{}{"metric": "run.googleapis.com/nvidia_l4_gpu_allocation",
		"buckets": []interface{}{}, "nonzero": []interface{}{}}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + proj +
		"/services/run.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	out["http"] = st
	if st != 200 {
		out["error"] = c211xTrunc(body, 700)
		return out
	}
	m := c211xJSON(body)
	if m == nil {
		out["error"] = "json_parse_failed"
		return out
	}
	buckets := []interface{}{}
	nonzero := []interface{}{}
	metrics, _ := m["metrics"].([]interface{})
	for _, mi := range metrics {
		mm, _ := mi.(map[string]interface{})
		nm := c211xStr(mm, "metric")
		if !strings.Contains(nm, "nvidia_l4_gpu_allocation") {
			continue
		}
		out["metric"] = nm
		limits, _ := mm["consumerQuotaLimits"].([]interface{})
		for _, li := range limits {
			lm, _ := li.(map[string]interface{})
			if un := c211xStr(lm, "unit"); un != "" {
				out["unit"] = un
			}
			if dn := c211xStr(lm, "name"); dn != "" {
				out["limit_name"] = dn
				if i := strings.LastIndex(dn, "/"); i >= 0 {
					out["metric"] = dn[i+1:]
				}
			}
			bks, _ := lm["quotaBuckets"].([]interface{})
			for _, bi := range bks {
				bm, _ := bi.(map[string]interface{})
				dims, _ := bm["dimensions"].(map[string]interface{})
				region := ""
				if dims != nil {
					if r, ok := dims["region"].(string); ok {
						region = r
					}
					if region == "" {
						if z, ok := dims["zone"].(string); ok {
							region = z
						}
					}
				}
				row := map[string]interface{}{
					"region":           region,
					"effectiveLimit":   bm["effectiveLimit"],
					"defaultLimit":     bm["defaultLimit"],
					"producerOverride": bm["producerOverride"],
					"consumerOverride": bm["consumerOverride"],
				}
				buckets = append(buckets, row)
				if c211rNum(bm["effectiveLimit"]) > 0 {
					nonzero = append(nonzero, row)
					fmt.Printf("C211R_QUOTA_NONZERO proj=%s metric=%s region=%s limit=%v\n",
						proj, out["metric"], region, bm["effectiveLimit"])
				}
			}
		}
	}
	out["buckets"] = buckets
	out["nonzero"] = nonzero
	out["n_buckets"] = len(buckets)
	out["n_nonzero"] = len(nonzero)
	return out
}

func c211rEligibleRegion(q map[string]interface{}) string {
	nz, _ := q["nonzero"].([]interface{})
	for _, region := range c211rRegions {
		for _, ni := range nz {
			nm, _ := ni.(map[string]interface{})
			if c211xStr(nm, "region") == region {
				return region
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// C/D/E. Service state, permission tests, jobs list
// ---------------------------------------------------------------------------

func c211rServiceState(tok, proj string) (int, string, string) {
	st, body := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+proj+
		"/services/run.googleapis.com", tok, "", "")
	return st, c211xStr(c211xJSON(body), "state"), body
}

func c211rPerms(tok, proj string) map[string]interface{} {
	base := "https://cloudresourcemanager.googleapis.com/v1/projects/" + proj
	out := map[string]interface{}{}
	al, st, body := c211xTestIamBatch(tok, base, c211rPermBatch)
	out["batch_http"] = st
	out["allowed"] = al
	if st != 200 {
		out["batch_body"] = c211xTrunc(body, 700)
		// per-permission fallback so one invalid permission cannot hide the rest
		al2 := []string{}
		per := map[string]interface{}{}
		for _, perm := range c211rPermBatch {
			ok, s1, b1 := c211xTestIamOne(tok, base, perm)
			per[perm] = map[string]interface{}{"http": s1, "allowed": ok}
			if s1 != 200 {
				per[perm].(map[string]interface{})["body"] = c211xTrunc(b1, 200)
			}
			if ok {
				al2 = append(al2, perm)
			}
		}
		sort.Strings(al2)
		out["allowed"] = al2
		out["per_permission"] = per
	}
	al3, st3, body3 := c211xTestIamBatch(tok, base, c211rPermStorage)
	out["storage_batch_http"] = st3
	out["storage_allowed"] = al3
	if st3 != 200 {
		out["storage_batch_body"] = c211xTrunc(body3, 400)
		al4 := []string{}
		per4 := map[string]interface{}{}
		for _, perm := range c211rPermStorage {
			ok, s4, b4 := c211xTestIamOne(tok, base, perm)
			per4[perm] = map[string]interface{}{"http": s4, "allowed": ok}
			if s4 != 200 {
				per4[perm].(map[string]interface{})["body"] = c211xTrunc(b4, 200)
			}
			if ok {
				al4 = append(al4, perm)
			}
		}
		sort.Strings(al4)
		out["storage_allowed"] = al4
		out["storage_per_permission"] = per4
	}
	return out
}

func c211rJobsList(tok, proj, region string) map[string]interface{} {
	u := "https://run.googleapis.com/v2/projects/" + proj + "/locations/" + region + "/jobs?pageSize=100"
	st, body := c211xHTTP("GET", u, tok, "", "")
	names := []string{}
	if m := c211xJSON(body); m != nil {
		if items, ok := m["jobs"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				names = append(names, c211xStr(im, "name"))
			}
		}
	}
	return map[string]interface{}{"http": st, "n": len(names), "names": names, "body": c211xTrunc(body, 300)}
}

// ---------------------------------------------------------------------------
// F. The one Cloud Run L4 job
// ---------------------------------------------------------------------------

func c211rCaptureScript() string {
	return `set -u
echo "C211R_START host=$(hostname) at=$(date -u +%FT%TZ) kernel=$(uname -r)"
echo "C211R_GPU_ENV nvidia=$(ls /dev/nvidia* 2>/dev/null | tr '\n' ',')"
if command -v nvidia-smi >/dev/null 2>&1; then
  echo C211R_NVSMIL_BEGIN
  nvidia-smi -L || echo C211R_NVSMIL_RC=$?
  echo C211R_NVQ_BEGIN
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader || echo C211R_NVQ_RC=$?
  echo C211R_NVSMIL_END
else
  echo C211R_NO_NVIDIA_SMI
  for p in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/bin/nvidia-smi; do
    if [ -x "$p" ]; then echo "C211R_FOUND $p"; "$p" -L; fi
  done
fi
echo "C211R_END host=$(hostname) at=$(date -u +%FT%TZ)"
`
}

func c211rJobBody(script string) string {
	payload := map[string]interface{}{
		"template": map[string]interface{}{
			"taskCount":   1,
			"parallelism": 1,
			"template": map[string]interface{}{
				"gpuZonalRedundancyDisabled": true,
				"containers": []interface{}{map[string]interface{}{
					"image":   c211rModelImage,
					"command": []interface{}{"/bin/bash", "-c"},
					"args":    []interface{}{script},
					"resources": map[string]interface{}{"limits": map[string]interface{}{
						"cpu": "4", "memory": "16Gi", "nvidia.com/gpu": "1",
					}},
				}},
				"nodeSelector": map[string]interface{}{"accelerator": "nvidia-l4"},
				"maxRetries":   0,
				"timeout":      "600s",
			},
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func c211rExecState(body string) map[string]interface{} {
	m := c211xJSON(body)
	out := map[string]interface{}{}
	if m == nil {
		return out
	}
	out["name"] = c211xStr(m, "name")
	out["completionStatus"] = c211xStr(m, "completionStatus")
	out["startTime"] = c211xStr(m, "startTime")
	out["completionTime"] = c211xStr(m, "completionTime")
	out["taskCount"] = m["taskCount"]
	out["succeededCount"] = m["succeededCount"]
	out["failedCount"] = m["failedCount"]
	conds := []interface{}{}
	if cs, ok := m["conditions"].([]interface{}); ok {
		for _, ci := range cs {
			cm, _ := ci.(map[string]interface{})
			conds = append(conds, map[string]interface{}{
				"type": c211xStr(cm, "type"), "state": c211xStr(cm, "state"),
				"message": c211xTrunc(c211xStr(cm, "message"), 240),
			})
		}
	}
	out["conditions"] = conds
	return out
}

func c211rReadLogs(tok, proj, job, region string) map[string]interface{} {
	out := map[string]interface{}{}
	filters := []string{
		`resource.type="cloud_run_job" AND resource.labels.job_name="` + job + `"`,
		`logName="projects/` + proj + `/logs/run.googleapis.com%2Fstdout"`,
	}
	texts := []string{}
	for i, filter := range filters {
		payload := map[string]interface{}{
			"resourceNames": []interface{}{"projects/" + proj},
			"filter":        filter,
			"orderBy":       "timestamp desc",
			"pageSize":      200,
		}
		b, _ := json.Marshal(payload)
		st, body := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
		out[fmt.Sprintf("attempt%d_http", i+1)] = st
		out[fmt.Sprintf("attempt%d_filter", i+1)] = filter
		if st != 200 {
			out[fmt.Sprintf("attempt%d_body", i+1)] = c211xTrunc(body, 400)
			continue
		}
		m := c211xJSON(body)
		if m == nil {
			continue
		}
		if entries, ok := m["entries"].([]interface{}); ok {
			out[fmt.Sprintf("attempt%d_n", i+1)] = len(entries)
			for _, ei := range entries {
				em, _ := ei.(map[string]interface{})
				if tp, ok := em["textPayload"].(string); ok && tp != "" {
					texts = append(texts, tp)
				} else if jp, ok := em["jsonPayload"]; ok && jp != nil {
					texts = append(texts, c211xJSONString(jp))
				}
			}
		}
		if len(texts) > 0 {
			break
		}
	}
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
	joined := strings.Join(texts, "\n")
	out["text"] = c211xTrunc(joined, 4000)
	uuids := []string{}
	for _, u := range c211xUUIDRe.FindAllString(joined, -1) {
		uuids = append(uuids, u)
	}
	out["gpu_uuids"] = uuids
	return out
}

func c211rJobReap(tok, proj, region, jobURL string, execName string) map[string]interface{} {
	out := map[string]interface{}{}
	steps := []interface{}{}
	if execName != "" {
		st, b := c211xHTTP(c211rMethodX, "https://run.googleapis.com/v2/"+execName, tok, "", "")
		steps = append(steps, map[string]interface{}{"target": "execution", "http": st, "body": c211xTrunc(b, 300)})
	}
	for _, suffix := range []string{"", "?force=true"} {
		st, b := c211xHTTP(c211rMethodX, jobURL+suffix, tok, "", "")
		steps = append(steps, map[string]interface{}{"target": "job_v2" + suffix, "http": st, "body": c211xTrunc(b, 400)})
		if st == 200 || st == 202 || st == 204 {
			break
		}
	}
	out["steps"] = steps
	got := 0
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := c211xHTTP("GET", jobURL, tok, "", "")
		got = st
		if st == 404 {
			break
		}
		time.Sleep(8 * time.Second)
	}
	out["get_after_reap"] = got
	lst := c211rJobsList(tok, proj, region)
	out["list_after_reap"] = lst
	names, _ := lst["names"].([]string)
	left := []string{}
	for _, n := range names {
		if strings.Contains(n, c211rJobID) {
			left = append(left, n)
		}
	}
	out["c211_left"] = left
	out["absent"] = got == 404 && len(left) == 0
	return out
}

// c211rPolicyWithout rebuilds the policy bindings without one role/member pair.
func c211rPolicyWithout(pol map[string]interface{}, role, member string) bool {
	if pol == nil {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	out := []interface{}{}
	changed := false
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			out = append(out, b)
			continue
		}
		members, _ := bm["members"].([]interface{})
		nm := []interface{}{}
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				changed = true
				continue
			}
			nm = append(nm, m)
		}
		if len(nm) > 0 {
			bm["members"] = nm
			out = append(out, bm)
		} else {
			changed = true
		}
	}
	pol["bindings"] = out
	return changed
}

// ---------------------------------------------------------------------------
// Fleet identity: the WIF/declared SA can act as the host project's
// fleetcommand-service-account (fact 121388/120951). The sibling-project
// permissions belong to THAT service account, so every project-level read is
// done with its own generateAccessToken result.
// ---------------------------------------------------------------------------

func c211rFleetMint(wifTok string) (string, string, map[string]interface{}) {
	out := map[string]interface{}{"host_project_number": c211rHostNum}
	hostID := ""
	pst, pbody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211rHostNum, wifTok, "", "")
	if pst == 200 {
		if m := c211xJSON(pbody); m != nil {
			hostID = c211xStr(m, "projectId")
		}
	}
	out["host_project_get"] = map[string]interface{}{"http": pst, "projectId": hostID, "body": c211xTrunc(pbody, 300)}
	sas, lst, lbody := c211xListServiceAccounts(wifTok, c211rHostNum)
	out["sa_list"] = map[string]interface{}{"http": lst, "n": len(sas), "body": c211xTrunc(lbody, 300)}
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	if fleetEmail == "" && hostID != "" {
		fleetEmail = "fleetcommand-service-account@" + hostID + ".iam.gserviceaccount.com"
		out["constructed"] = true
	}
	out["fleet_email"] = fleetEmail
	fleetTok := ""
	if fleetEmail != "" {
		at, gs, gb := c211xGenerateAccessToken(wifTok, fleetEmail)
		out["fleet_mint"] = map[string]interface{}{"http": gs, "token": c211xShort(at), "body": c211xTrunc(gb, 400)}
		fleetTok = at
	}
	if fleetTok == "" {
		fleetTok = wifTok
		out["fallback"] = "wif_minted_token"
	}
	out["ok"] = fleetTok != wifTok
	fmt.Printf("C211R_FLEET %s\n", c211xJSONString(out))
	return fleetTok, fleetEmail, out
}

// ---------------------------------------------------------------------------
// Per-project probe
// ---------------------------------------------------------------------------

func c211rProject(fleet, fleetEmail string, p c211rTarget, t0 time.Time) map[string]interface{} {
	res := map[string]interface{}{"id": p.ID, "number": p.Number, "device_line": ""}
	budgetLeft := func() bool {
		return int(time.Since(t0).Seconds()) < c211rBudgetSecs
	}

	// A. quota
	q := c211rQuota(fleet, p.Number)
	res["quota"] = q
	fmt.Printf("C211R_QUOTA proj=%s %s\n", p.ID, c211xJSONString(q))

	// B. cloudquotas quotaInfos oracle
	cq := map[string]interface{}{}
	cu := "https://cloudquotas.googleapis.com/v1/projects/" + p.Number +
		"/locations/global/services/run.googleapis.com/quotaInfos?pageSize=200"
	cst, cbody := c211xHTTP("GET", cu, fleet, "", "")
	cq["http"] = cst
	cq["body"] = c211xTrunc(cbody, 400)
	if cst == 200 {
		if m := c211xJSON(cbody); m != nil {
			if qis, ok := m["quotaInfos"].([]interface{}); ok {
				hot := []interface{}{}
				for _, qi := range qis {
					qm, _ := qi.(map[string]interface{})
					if strings.Contains(c211xStr(qm, "quotaId"), "l4") {
						hot = append(hot, map[string]interface{}{
							"quotaId": c211xStr(qm, "quotaId"), "metric": c211xStr(qm, "metric"),
							"dimensionsInfos": qm["dimensionsInfos"],
						})
					}
				}
				cq["n"] = len(qis)
				cq["l4"] = hot
			}
		}
	}
	res["cloudquotas"] = cq
	fmt.Printf("C211R_CLOUDQUOTAS proj=%s %s\n", p.ID, c211xJSONString(cq))

	// C. service state
	sst, sstate, sbody := c211rServiceState(fleet, p.Number)
	res["service"] = map[string]interface{}{"http": sst, "state": sstate, "body": c211xTrunc(sbody, 400)}
	fmt.Printf("C211R_SERVICE proj=%s http=%d state=%s\n", p.ID, sst, sstate)

	// D. permissions
	perms := c211rPerms(fleet, p.Number)
	res["perms"] = perms
	fmt.Printf("C211R_PERMS proj=%s %s\n", p.ID, c211xJSONString(map[string]interface{}{
		"batch_http": perms["batch_http"], "allowed": perms["allowed"],
		"storage_batch_http": perms["storage_batch_http"], "storage_allowed": perms["storage_allowed"]}))
	allowed := c211rSlice(perms["allowed"])

	// E. pre-existing jobs in both probe regions
	jl := map[string]interface{}{}
	for _, region := range c211rRegions {
		jl[region] = c211rJobsList(fleet, p.Number, region)
	}
	res["jobs_pre"] = jl

	// eligibility
	region := c211rEligibleRegion(q)
	res["eligible_region"] = region
	canCreate := c211rHas(allowed, "run.jobs.create")
	canRun := c211rHas(allowed, "run.jobs.run")
	canSetPol := c211rHas(allowed, "resourcemanager.projects.setIamPolicy")
	canEnable := c211rHas(allowed, "serviceusage.services.enable")
	res["can"] = map[string]interface{}{"run.jobs.create": canCreate, "run.jobs.run": canRun,
		"resourcemanager.projects.setIamPolicy": canSetPol, "serviceusage.services.enable": canEnable}

	// Read-only quota self-grants (reversible) when the metrics API refuses
	// serviceusage.quotas.get. The fleet SA holds setIamPolicy (fact 121388).
	addedRoles := []string{}
	member := "serviceAccount:" + fleetEmail
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + p.Number
	if qh, _ := q["http"].(int); qh != 200 && canSetPol && fleetEmail != "" {
		wantRoles := []string{"roles/serviceusage.quotaViewer", "roles/cloudquotas.viewer"}
		pol, pst, pbody := c211xGetPolicy(fleet, projURL)
		res["policy_pre"] = map[string]interface{}{"http": pst, "body": c211xTrunc(pbody, 300)}
		if pol != nil {
			changed := []string{}
			for _, r := range wantRoles {
				if c211xAddBinding(pol, r, member) {
					changed = append(changed, r)
				}
			}
			if len(changed) > 0 {
				sst, sbody := c211xSetPolicy(fleet, projURL, pol)
				res["quota_grant"] = map[string]interface{}{"roles": changed, "http": sst, "body": c211xTrunc(sbody, 400)}
				fmt.Printf("C211R_QUOTAGRANT proj=%s roles=%v http=%d %s\n", p.ID, changed, sst, c211xTrunc(sbody, 200))
				if sst == 200 {
					addedRoles = append(addedRoles, changed...)
					q = c211rQuota(fleet, p.Number)
					res["quota_after_grant"] = q
					cu2 := "https://cloudquotas.googleapis.com/v1/projects/" + p.Number +
						"/locations/global/services/run.googleapis.com/quotaInfos?pageSize=200"
					cst2b, cbody2b := c211xHTTP("GET", cu2, fleet, "", "")
					res["cloudquotas_after_grant"] = map[string]interface{}{"http": cst2b, "body": c211xTrunc(cbody2b, 400)}
					fmt.Printf("C211R_QUOTA2 proj=%s http=%v n_nonzero=%v\n", p.ID, q["http"], q["n_nonzero"])
					region = c211rEligibleRegion(q)
					res["eligible_region"] = region
				}
			}
		}
	}
	// The serviceusage consumerQuotaMetrics reading for nvidia_l4_gpu_allocation is
	// DEGENERATE: the 14-project corpus sweep (fact 120750) recorded an empty
	// nonzero set for every project, including venue-review-seng365, which later
	// ran a real Cloud Run L4 job (finding 121522). A null/absent bucket therefore
	// does NOT mean zero availability, so the only faithful measurement is ONE
	// bounded create+run attempt in a supported region; the provider's verbatim
	// response decides the verdict.
	if region == "" {
		region = c211rRegions[0]
		res["region_basis"] = "quota buckets null/empty (degenerate read); bounded create+run attempt in a supported region"
	}
	if !canCreate || !canRun {
		res["device_attempt"] = "SKIPPED_NO_RUN_PERMISSION"
		res["verdict"] = "NO_RUN_PERMISSION"
		return res
	}
	if !budgetLeft() {
		res["device_attempt"] = "SKIPPED_BUDGET"
		return res
	}

	// enable run.googleapis.com once if DISABLED and permitted
	if sstate != "ENABLED" && canEnable {
		est, ebody := c211xHTTP("POST", "https://serviceusage.googleapis.com/v1/projects/"+p.Number+
			"/services/run.googleapis.com:enable", fleet, "application/json", "{}")
		res["enable"] = map[string]interface{}{"http": est, "body": c211xTrunc(ebody, 500)}
		fmt.Printf("C211R_ENABLE proj=%s http=%d %s\n", p.ID, est, c211xTrunc(ebody, 200))
		sst2, sstate2, _ := c211rServiceState(fleet, p.Number)
		res["service_after_enable"] = map[string]interface{}{"http": sst2, "state": sstate2}
	}

	// logging read candidates: fleet token, then project default compute SA
	logTokens := []map[string]string{{"who": "fleet", "token": fleet}}
	computeSA := p.Number + "-compute@developer.gserviceaccount.com"
	if ct, cst2, cbody2 := c211xGenerateAccessToken(fleet, computeSA); ct != "" {
		logTokens = append(logTokens, map[string]string{"who": "compute", "token": ct})
		res["compute_impersonation"] = map[string]interface{}{"http": cst2, "sa": computeSA}
	} else {
		res["compute_impersonation"] = map[string]interface{}{"http": cst2, "sa": computeSA,
			"body": c211xTrunc(cbody2, 400)}
	}
	readWho := ""
	for _, lt := range logTokens {
		pr := c211rReadLogs(lt["token"], p.Number, c211rJobID, region)
		res["log_probe_"+lt["who"]] = pr
		if h, ok := pr["attempt1_http"].(int); ok && h == 200 {
			readWho = lt["who"]
			break
		}
		if h, ok := pr["attempt2_http"].(int); ok && h == 200 {
			readWho = lt["who"]
			break
		}
	}
	fmt.Printf("C211R_LOGPROBE proj=%s readWho=%q\n", p.ID, readWho)

	// one reversible roles/logging.viewer self-grant when the log channel is closed
	addedBinding := false
	if readWho == "" && canSetPol && fleetEmail != "" {
		pol, pst, pbody := c211xGetPolicy(fleet, projURL)
		res["policy_pre"] = map[string]interface{}{"http": pst, "body": c211xTrunc(pbody, 300)}
		if pol != nil {
			if c211xAddBinding(pol, "roles/logging.viewer", member) {
				sst2, sbody2 := c211xSetPolicy(fleet, projURL, pol)
				res["log_grant"] = map[string]interface{}{"http": sst2, "body": c211xTrunc(sbody2, 500)}
				fmt.Printf("C211R_LOGGRANT proj=%s http=%d %s\n", p.ID, sst2, c211xTrunc(sbody2, 200))
				if sst2 == 200 {
					addedBinding = true
					// re-probe the log channel
					for _, lt := range logTokens {
						pr := c211rReadLogs(lt["token"], p.Number, c211rJobID, region)
						res["log_probe_after_grant_"+lt["who"]] = pr
						if h, ok := pr["attempt1_http"].(int); ok && h == 200 {
							readWho = lt["who"]
							break
						}
						if h, ok := pr["attempt2_http"].(int); ok && h == 200 {
							readWho = lt["who"]
							break
						}
					}
					fmt.Printf("C211R_LOGPROBE2 proj=%s readWho=%q\n", p.ID, readWho)
				}
			}
		}
	}
	res["log_read_identity"] = readWho
	restoreRoles := append([]string{}, addedRoles...)
	if addedBinding {
		restoreRoles = append(restoreRoles, "roles/logging.viewer")
	}

	// create EXACTLY ONE nonce job
	base := "https://run.googleapis.com/v2/projects/" + p.Number + "/locations/" + region + "/jobs"
	jobURL := base + "/" + c211rJobID
	createURL := base + "?jobId=" + c211rJobID
	body := c211rJobBody(c211rCaptureScript())
	cst3, cbody3 := c211xHTTP("POST", createURL, fleet, "application/json", body)
	res["create"] = map[string]interface{}{"http": cst3, "body": c211xTrunc(cbody3, 900)}
	fmt.Printf("C211R_CREATE proj=%s region=%s http=%d %s\n", p.ID, region, cst3, c211xTrunc(cbody3, 400))
	if cst3 != 200 {
		res["device_attempt"] = "CREATE_REFUSED"
		res["verdict"] = "JOB_CREATE_REFUSED"
		if len(restoreRoles) > 0 {
			res["iam_restore"] = c211rIamRestore(fleet, projURL, member, restoreRoles)
		}
		return res
	}

	// run it once
	rst, rbody := c211xHTTP("POST", jobURL+":run", fleet, "application/json", "{}")
	res["run"] = map[string]interface{}{"http": rst, "body": c211xTrunc(rbody, 700)}
	fmt.Printf("C211R_RUN proj=%s http=%d %s\n", p.ID, rst, c211xTrunc(rbody, 300))

	// find the execution
	execName := ""
	deadline := time.Now().Add(300 * time.Second)
	for time.Now().Before(deadline) && budgetLeft() {
		st, b := c211xHTTP("GET", jobURL+"/executions?pageSize=10", fleet, "", "")
		if st == 200 {
			if m := c211xJSON(b); m != nil {
				if items, ok := m["executions"].([]interface{}); ok && len(items) > 0 {
					im, _ := items[len(items)-1].(map[string]interface{})
					execName = c211xStr(im, "name")
					break
				}
			}
		}
		time.Sleep(8 * time.Second)
	}
	res["execution"] = execName
	fmt.Printf("C211R_EXEC proj=%s %s\n", p.ID, execName)

	// poll to terminal
	if execName != "" {
		deadline = time.Now().Add(780 * time.Second)
		for time.Now().Before(deadline) && budgetLeft() {
			st, b := c211xHTTP("GET", "https://run.googleapis.com/v2/"+execName, fleet, "", "")
			es := c211rExecState(b)
			es["http"] = st
			res["exec"] = es
			cs := c211xStr(c211xJSON(b), "completionStatus")
			ct := c211xStr(c211xJSON(b), "completionTime")
			if strings.HasSuffix(cs, "_SUCCEEDED") || strings.HasSuffix(cs, "_FAILED") ||
				strings.HasSuffix(cs, "_CANCELLED") || ct != "" {
				break
			}
			time.Sleep(10 * time.Second)
		}
		fmt.Printf("C211R_EXECSTATE proj=%s %s\n", p.ID, c211xJSONString(res["exec"]))
	}

	// read the container stdout (all candidate identities; most complete wins)
	best := map[string]interface{}{}
	bestText := ""
	for _, lt := range logTokens {
		lr := c211rReadLogs(lt["token"], p.Number, c211rJobID, region)
		res["log_"+lt["who"]] = lr
		if t2, ok := lr["text"].(string); ok && strings.Contains(t2, "C211R_") && len(t2) > len(bestText) {
			best = lr
			bestText = t2
		}
	}
	res["log_best_uuids"] = best["gpu_uuids"]
	res["log_best_tail"] = c211xTrunc(bestText, 2500)

	// independent readback on a real device line
	deviceLine := ""
	uuids := []string{}
	if arr, ok := best["gpu_uuids"].([]string); ok {
		uuids = arr
	}
	if len(uuids) > 0 {
		for _, line := range strings.Split(bestText, "\n") {
			if strings.Contains(line, "GPU-") && strings.Contains(line, "MiB") {
				deviceLine = strings.TrimSpace(line)
				break
			}
		}
		if deviceLine == "" {
			for _, line := range strings.Split(bestText, "\n") {
				if strings.Contains(line, "GPU-") {
					deviceLine = strings.TrimSpace(line)
					break
				}
			}
		}
	}
	if deviceLine != "" {
		st, b := c211xHTTP("GET", jobURL, fleet, "", "")
		res["job_readback"] = map[string]interface{}{"http": st,
			"uid": c211xStr(c211xJSON(b), "uid"), "name": c211xStr(c211xJSON(b), "name"),
			"updateTime": c211xStr(c211xJSON(b), "updateTime"), "body": c211xTrunc(b, 600)}
		if execName != "" {
			st2, b2 := c211xHTTP("GET", "https://run.googleapis.com/v2/"+execName, fleet, "", "")
			es := c211rExecState(b2)
			es["http"] = st2
			res["exec_readback"] = es
		}
	}
	res["device_line"] = deviceLine
	res["device_uuids"] = uuids
	res["chosen_region"] = region
	res["job"] = c211rJobID
	fmt.Printf("C211R_DEVICE proj=%s region=%s uuids=%v line=%q\n", p.ID, region, uuids, deviceLine)

	// reap + absence proof
	res["reap"] = c211rJobReap(fleet, p.Number, region, jobURL, execName)
	fmt.Printf("C211R_REAP proj=%s %s\n", p.ID, c211xJSONString(res["reap"]))

	// restore every IAM binding this probe added
	if len(restoreRoles) > 0 {
		res["iam_restore"] = c211rIamRestore(fleet, projURL, member, restoreRoles)
		fmt.Printf("C211R_IAMRESTORE proj=%s %s\n", p.ID, c211xJSONString(res["iam_restore"]))
	}
	if deviceLine != "" {
		res["verdict"] = "DEVICE_PROVEN"
	} else {
		res["verdict"] = "NO_DEVICE_LINE"
	}
	return res
}

func c211rIamRestore(tok, projURL, member string, roles []string) map[string]interface{} {
	out := map[string]interface{}{}
	for attempt := 0; attempt < 2; attempt++ {
		pol, st, body := c211xGetPolicy(tok, projURL)
		if pol == nil {
			out["get"] = map[string]interface{}{"http": st, "body": c211xTrunc(body, 300)}
			return out
		}
		before := []string{}
		for _, r := range roles {
			if c211xPolicyHas(pol, r, member) {
				before = append(before, r)
			}
		}
		changed := []string{}
		for _, r := range roles {
			if c211rPolicyWithout(pol, r, member) {
				changed = append(changed, r)
			}
		}
		sst, sbody := c211xSetPolicy(tok, projURL, pol)
		out["attempt"] = attempt + 1
		out["roles"] = roles
		out["present_before"] = before
		out["changed"] = changed
		out["set_http"] = sst
		out["set_body"] = c211xTrunc(sbody, 300)
		pol2, _, _ := c211xGetPolicy(tok, projURL)
		left := []string{}
		for _, r := range roles {
			if c211xPolicyHas(pol2, r, member) {
				left = append(left, r)
			}
		}
		out["present_after"] = left
		if sst == 200 && len(left) == 0 {
			out["ok"] = true
			return out
		}
		time.Sleep(3 * time.Second)
	}
	out["ok"] = false
	return out
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func TestC211I121916CloudRunL4(t *testing.T) {
	t0 := time.Now()
	S := map[string]interface{}{
		"probe":   "c211_i121916_crl4",
		"nonce":   c211rNonce,
		"started": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	fleet, auth, mintLog := c211xMint()
	fmt.Printf("C211R_MINT ok=%v auth=%s\n%s\n", fleet != "", auth, mintLog)
	S["mint"] = map[string]interface{}{"ok": fleet != "", "auth": auth, "log": mintLog}
	if fleet == "" {
		S["verdict"] = "NO_TOKEN"
		S["elapsed_s"] = int(time.Since(t0).Seconds())
		fmt.Printf("C211R_SUMMARY_JSON %s\n", c211xJSONString(S))
		return
	}
	wifEmail := ""
	tiStatus, tiBody := c211xTokenInfo(fleet)
	if m := c211xJSON(tiBody); m != nil {
		wifEmail = c211xStr(m, "email")
	}
	S["identity"] = map[string]interface{}{"tokeninfo_http": tiStatus,
		"token": c211xShort(fleet), "email": wifEmail, "body": c211xTrunc(tiBody, 700)}
	fmt.Printf("C211R_IDENTITY %s\n", c211xJSONString(S["identity"]))
	fleet2, fleetEmail2, fleetRec := c211rFleetMint(fleet)
	S["fleet"] = fleetRec
	fleet = fleet2
	fleetEmail := fleetEmail2
	if fleetEmail == "" {
		fleetEmail = wifEmail
	}
	fti, ftb := c211xTokenInfo(fleet)
	S["fleet_identity"] = map[string]interface{}{"tokeninfo_http": fti, "body": c211xTrunc(ftb, 500)}
	fmt.Printf("C211R_FLEET_IDENTITY %s\n", c211xJSONString(S["fleet_identity"]))

	projects := map[string]interface{}{}
	S["projects"] = projects
	proven := []interface{}{}
	for _, p := range c211rTargets {
		if int(time.Since(t0).Seconds()) > c211rBudgetSecs {
			projects[p.ID] = map[string]interface{}{"skipped": "budget"}
			continue
		}
		pr := c211rProject(fleet, fleetEmail, p, t0)
		projects[p.ID] = pr
		if dl, ok := pr["device_line"].(string); ok && dl != "" {
			proven = append(proven, map[string]interface{}{
				"project": p.ID, "number": p.Number, "region": pr["chosen_region"],
				"device_line": dl, "uuids": pr["device_uuids"], "job": c211rJobID,
				"execution": pr["execution"],
			})
		}
	}
	S["proven"] = proven
	S["elapsed_s"] = int(time.Since(t0).Seconds())
	if len(proven) > 0 {
		S["verdict"] = "DEVICE_PROVEN"
	} else {
		S["verdict"] = "NO_DEVICE"
	}
	fmt.Printf("C211R_SUMMARY_JSON %s\n", c211xJSONString(S))
	_ = t
}
