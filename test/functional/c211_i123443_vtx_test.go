//go:build functional && gke

// C211 i123443 — convert the three Domino sibling projects that produced
// NO_DEVICE_LINE in the x123052 explicit-command Vertex T4 probe into either a
// READY T4 proof in an alternative region or one exact per-project GATE.
//
// Saved-spec diagnosis (i123363 C211R_MATRIX / fact 123406): all four
// projects' customJobs carried machineSpec {machineType: n1-standard-4,
// acceleratorType: NVIDIA_TESLA_T4, acceleratorCount: 1} verbatim in the stored
// jobSpec, and all four measured the same on-demand Vertex T4 quota
// (us-central1=12, europe-west4=2, us-east1=2, europe-west2=6,
// asia-northeast1=6, asia-south1=6).  The prior accelerator therefore WAS
// attached in the spec, so us-central1 is NOT eligible under the mandate's
// fallback clause.  This probe reads each project's own
// custom_model_training_nvidia_t4_gpus quota and acceleratorTypes offering and
// creates exactly ONE on-demand n1-standard-4 + 1x NVIDIA_TESLA_T4 customJob in
// the first alternative region where BOTH limit >= 1 AND nvidia-tesla-t4 is
// offered, with an explicit nvidia-smi command and two independent evidence
// sinks (Cloud Logging stdout plus a GCS object written from inside the
// container) so a logging-side gap cannot masquerade as a missing GPU.
//
// Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const c211tNonce = "i123443"
const c211tPrefix = "c211-i123443"

type c211tProj struct {
	Name string
	Num  string
}

// The three projects that returned NO_DEVICE_LINE in run 44912176107.
var c211tProjects = []c211tProj{
	{"team-eng-infra-testing", "210628133505"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

// Candidate order mandated by the intent (us-central1 excluded: the saved spec
// proves the accelerator was attached there).  Only regions where the project's
// own T4 quota is >= 1 AND a zone offers nvidia-tesla-t4 are eligible.
var c211tRegionPref = []string{
	"us-east1", "us-west1", "europe-west4", "asia-east1",
	"europe-west2", "asia-northeast1", "asia-south1",
}

var c211tZonesByRegion = map[string][]string{
	"us-east1":        {"us-east1-b", "us-east1-c", "us-east1-d", "us-east1-a"},
	"us-west1":        {"us-west1-a", "us-west1-b", "us-west1-c"},
	"europe-west4":    {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
	"asia-east1":      {"asia-east1-a", "asia-east1-b", "asia-east1-c"},
	"europe-west2":    {"europe-west2-a", "europe-west2-b", "europe-west2-c"},
	"asia-northeast1": {"asia-northeast1-a", "asia-northeast1-b", "asia-northeast1-c"},
	"asia-south1":     {"asia-south1-a", "asia-south1-b", "asia-south1-c"},
}

var c211tPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"serviceusage.services.enable", "storage.buckets.create",
	"storage.objects.create",
}

// ---------------------------------------------------------------------------
// small local helpers (names c211t* to avoid collisions)
// ---------------------------------------------------------------------------

func c211tList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211tMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211tFloat(v interface{}) float64 {
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

func c211tShortProj(p string) string {
	p = strings.ToLower(p)
	out := []rune{}
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	s := string(out)
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

func c211tUUIDs(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range c211xUUIDRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func c211tJoin(m map[string]string) string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// c211tQuota reads all GPU consumer quota metrics of the project.
func c211tQuota(tok, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, body
	}
	m := c211xJSON(body)
	for _, mi := range c211tList(m["metrics"]) {
		mm := c211tMap(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "gpu") {
			continue
		}
		for _, li := range c211tList(mm["consumerQuotaLimits"]) {
			lm := c211tMap(li)
			for _, bi := range c211tList(lm["quotaBuckets"]) {
				bm := c211tMap(bi)
				dims := c211tMap(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211tFloat(bm["effectiveLimit"])
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

// ---------------------------------------------------------------------------
// GCS proof sink (best effort: no new API is enabled)
// ---------------------------------------------------------------------------

func c211tBucketName(num string) string {
	return "c211-" + c211tNonce + "-" + num
}

func c211tBucketCreate(tok, num, region string) (string, int, string) {
	name := c211tBucketName(num)
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(num)
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(body))
	return name, st, rb
}

func c211tBucketIam(tok, name, num, fleetEmail string) (int, string) {
	bindings := []interface{}{map[string]interface{}{
		"role": "roles/storage.objectAdmin",
		"members": []interface{}{
			"serviceAccount:" + num + "-compute@developer.gserviceaccount.com",
			"serviceAccount:" + fleetEmail,
		},
	}}
	body, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/iam"
	return c211xHTTP("PUT", u, tok, "application/json", string(body))
}

func c211tBucketListObjects(tok, name, prefix string) ([]string, int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211xHTTP("GET", u, tok, "", "")
	names := []string{}
	m := c211xJSON(body)
	for _, it := range c211tList(m["items"]) {
		if n := c211xStr(c211tMap(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st, body
}

func c211tObjectRead(tok, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211xHTTP("GET", u, tok, "", "")
}

func c211tBucketCleanup(tok, name string) string {
	names, lst, _ := c211tBucketListObjects(tok, name, "")
	deleted := []string{}
	for _, n := range names {
		du := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/o/" + url.PathEscape(n)
		dst, _ := c211xHTTP("DELETE", du, tok, "", "")
		deleted = append(deleted, fmt.Sprintf("%s=%d", n, dst))
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), tok, "", "")
	getAfter := -1
	for i := 0; i < 8; i++ {
		gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), tok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("list=%d deleted=%v delete_http=%d get_after=%d verbatim=%s",
		lst, deleted, dst, getAfter, c211xTrunc(db, 200))
}

// ---------------------------------------------------------------------------
// container script
// ---------------------------------------------------------------------------

func c211tScript(proj string) string {
	s := `set -u
PROOF=/tmp/c211t-proof.txt
{
echo "C211T_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i123443 proj=__PROJ__"
echo "C211T_UNAME $(uname -a)"
echo "C211T_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211T_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211T_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211T_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
echo "C211T_MOUNTS $(mount 2>/dev/null | grep -i -E 'gcs|gcsfuse' | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211T_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211T_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211T_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211T_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211T_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211T_NOSMI"
fi
echo "C211T_END"
} > $PROOF 2>&1
cat $PROOF
DEST=""
for c in /gcs /gcs/__NONCE__ /gcs/output; do
  if [ -d "$c" ]; then DEST="$c"; break; fi
done
echo "C211T_GCS_DEST ${DEST:-none}"
if [ -n "$DEST" ]; then
  if cp $PROOF "$DEST/c211t-proof-__PROJ__.txt"; then
    echo "C211T_GCS_WRITE ok path=$DEST/c211t-proof-__PROJ__.txt"
  else
    echo "C211T_GCS_WRITE copy_failed"
  fi
else
  echo "C211T_GCS_WRITE no_mount"
fi
sleep 25
echo "C211T_DONE at=$(date -u +%FT%TZ)"
`
	s = strings.ReplaceAll(s, "__PROJ__", proj)
	s = strings.ReplaceAll(s, "__NONCE__", c211tNonce)
	return s
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func c211tLogList(body string) (string, int) {
	m := c211xJSON(body)
	entries, ok := m["entries"].([]interface{})
	if !ok {
		return "", 0
	}
	var b strings.Builder
	for _, e := range entries {
		em := c211tMap(e)
		if ts := c211xStr(em, "timestamp"); ts != "" {
			b.WriteString(ts + " ")
		}
		if p, ok := em["textPayload"].(string); ok {
			b.WriteString(p)
		}
		if jp := c211tMap(em["jsonPayload"]); jp != nil {
			b.WriteString(c211xJSONString(jp))
		}
		if pb := c211tMap(em["protoPayload"]); pb != nil {
			b.WriteString(c211xJSONString(pb))
		}
		b.WriteString("\n")
	}
	return b.String(), len(entries)
}

func c211tLogsOnce(tok, num, filter string) (int, string, int) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + num},
		"filter":        filter,
		"pageSize":      300,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	txt, n := c211tLogList(rb)
	return st, txt, n
}

func c211tLogs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`resource.type="ml_job" AND labels.job_id="` + jobID + `"`,
		`"C211T_" AND resource.labels.job_id="` + jobID + `"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		st, txt, n := c211tLogsOnce(tok, num, f)
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 80), st, n, len(txt)))
		if txt != "" {
			all.WriteString(txt + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// ---------------------------------------------------------------------------
// job helpers
// ---------------------------------------------------------------------------

func c211tJobState(m map[string]interface{}) string {
	errText := ""
	if e := c211tMap(m["error"]); e != nil {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 300))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

func c211tMachineSpec(m map[string]interface{}) string {
	spec := c211tMap(m["jobSpec"])
	wps := c211tList(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	ms := c211tMap(c211tMap(wps[0])["machineSpec"])
	out := fmt.Sprintf("machineType=%q acceleratorType=%q acceleratorCount=%v",
		c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"])
	cs := c211tMap(c211tMap(wps[0])["containerSpec"])
	out += fmt.Sprintf(" imageUri=%q", c211xStr(cs, "imageUri"))
	if bo := c211tMap(spec["baseOutputDirectory"]); bo != nil && c211xStr(bo, "outputUriPrefix") != "" {
		out += fmt.Sprintf(" outputUriPrefix=%q", c211xStr(bo, "outputUriPrefix"))
	}
	return out
}

func c211tRetire(tok, reg, name string) (int, int, string) {
	jurl := "https://" + reg + "-aiplatform.googleapis.com/v1/" + name
	dst := -1
	note := ""
	for attempt := 0; attempt < 10; attempt++ {
		gst, gb := c211xHTTP("GET", jurl, tok, "", "")
		if gst == 404 {
			return dst, 404, "already-absent"
		}
		state := c211xStr(c211xJSON(gb), "state")
		if state == "JOB_STATE_PENDING" {
			cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
			fmt.Printf("C211T_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211T_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(db, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211xHTTP("GET", jurl, tok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211tResult struct {
	Proj        string            `json:"proj"`
	Num         string            `json:"num"`
	State       string            `json:"state"`
	PermHTTP    int               `json:"perm_http"`
	Allowed     []string          `json:"allowed"`
	Svc         string            `json:"svc"`
	QuotaT4     map[string]string `json:"quota_t4"`
	Offering    map[string]string `json:"offering"`
	Chosen      string            `json:"chosen_region"`
	ChosenZone  string            `json:"chosen_zone"`
	Bucket      string            `json:"bucket"`
	BucketMake  string            `json:"bucket_create"`
	Create      string            `json:"create"`
	Job         string            `json:"job"`
	States      []string          `json:"states"`
	Readback    string            `json:"readback"`
	Spec        string            `json:"stored_machine_spec"`
	LogQuery    string            `json:"log_query"`
	LogBytes    int               `json:"log_bytes"`
	GCSRead     string            `json:"gcs_read"`
	GCSBytes    int               `json:"gcs_bytes"`
	BucketClean string            `json:"bucket_cleanup"`
	Device      string            `json:"device_lines"`
	UUIDs       []string          `json:"uuids"`
	Absence     string            `json:"absence"`
	Verdict     string            `json:"verdict"`
}

func TestC211T3T4AltRegion(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211T_START nonce=%s at=%s\n", c211tNonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211T_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
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
	fmt.Printf("C211T_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211T_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211T_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("C211T_TOKENINFO status=%d verbatim=%s\n", tiSt, c211xTrunc(tiBody, 400))

	const t4Key = "aiplatform.googleapis.com/custom_model_training_nvidia_t4_gpus"
	results := map[string]*c211tResult{}
	proven := []string{}
	deadline := started.Add(30 * time.Minute)

	for _, p := range c211tProjects {
		r := &c211tResult{Proj: p.Name, Num: p.Num, QuotaT4: map[string]string{}, Offering: map[string]string{}}
		results[p.Name] = r
		if time.Until(deadline) < 7*time.Minute {
			r.Verdict = "BUDGET_EXHAUSTED_BEFORE_PROJECT"
			fmt.Printf("C211T_SKIP proj=%s reason=budget\n", p.Name)
			continue
		}

		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p.Name), ftok, "", "")
		r.State = c211xStr(c211xJSON(pmBody), "lifecycleState")
		fmt.Printf("C211T_PROJECT proj=%s http=%d state=%s\n", p.Name, pmSt, r.State)

		polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Name)
		pol, polSt, polBody := c211xGetPolicy(ftok, polURL)
		roles := []string{}
		if pol != nil {
			member := "serviceAccount:" + fleetEmail
			for _, bv := range c211tList(pol["bindings"]) {
				bm := c211tMap(bv)
				for _, mv := range c211tList(bm["members"]) {
					if s, ok := mv.(string); ok && s == member {
						roles = append(roles, c211xStr(bm, "role"))
					}
				}
			}
		}
		sort.Strings(roles)
		fmt.Printf("C211T_FLEET_ROLES proj=%s http=%d n=%d roles=%v verbatim=%s\n", p.Name, polSt, len(roles), roles, c211xTrunc(polBody, 160))

		al, ast, _ := c211xTestIamBatch(ftok, polURL, c211tPerms)
		r.PermHTTP = ast
		r.Allowed = al
		fmt.Printf("C211T_PERM proj=%s http=%d allowed=%v\n", p.Name, ast, al)
		if !c211xContains(al, "aiplatform.customJobs.create") {
			r.Verdict = "PERM_MISSING_CREATE"
			fmt.Printf("C211T_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+p.Num+"/services/aiplatform.googleapis.com", ftok, "", "")
		r.Svc = c211xStr(c211xJSON(svBody), "state")
		fmt.Printf("C211T_SVC proj=%s http=%d state=%s\n", p.Name, svSt, r.Svc)

		quotaMap, qst, qbody := c211tQuota(ftok, p.Num)
		t4 := map[string]float64{}
		if quotaMap != nil && quotaMap[t4Key] != nil {
			t4 = quotaMap[t4Key]
		}
		for _, reg := range c211tRegionPref {
			if v, ok := t4[reg]; ok {
				r.QuotaT4[reg] = fmt.Sprintf("%.0f", v)
			} else {
				r.QuotaT4[reg] = "absent"
			}
		}
		if v, ok := t4["us-central1"]; ok {
			r.QuotaT4["us-central1"] = fmt.Sprintf("%.0f", v)
		}
		fmt.Printf("C211T_QUOTA proj=%s http=%d t4=%s verbatim=%s\n", p.Name, qst, c211tJoin(r.QuotaT4), c211xTrunc(qbody, 160))

		// acceleratorTypes offering matrix + region choice
		chosen, chosenZone := "", ""
		for _, reg := range c211tRegionPref {
			zones := c211tZonesByRegion[reg]
			hits := []string{}
			for _, z := range zones {
				types, zst, _ := c211xAccelTypes(ftok, p.Num, z)
				t4off := c211xContains(types, "nvidia-tesla-t4")
				if t4off {
					hits = append(hits, z)
				}
				fmt.Printf("C211T_OFFER proj=%s region=%s zone=%s http=%d n=%d t4=%v\n", p.Name, reg, z, zst, len(types), t4off)
				time.Sleep(200 * time.Millisecond)
			}
			if len(hits) > 0 {
				r.Offering[reg] = strings.Join(hits, "+")
			} else {
				r.Offering[reg] = "none"
			}
			if chosen == "" && t4[reg] >= 1 && len(hits) > 0 {
				chosen = reg
				chosenZone = hits[0]
			}
		}
		r.Chosen = chosen
		r.ChosenZone = chosenZone
		fmt.Printf("C211T_CHOICE proj=%s region=%s zone=%s quota=%v offering=%s\n", p.Name, chosen, chosenZone, r.QuotaT4[chosen], c211tJoin(r.Offering))
		if chosen == "" {
			r.Verdict = "NO_ALT_REGION_WITH_QUOTA_AND_T4_OFFERING"
			fmt.Printf("C211T_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		// Best-effort GCS proof sink (no API is enabled; a refusal is recorded verbatim).
		usedBucket := ""
		bucketCreated := ""
		bname, bst, bbody := c211tBucketCreate(ftok, p.Num, chosen)
		r.BucketMake = fmt.Sprintf("http=%d verbatim=%s", bst, c211xTrunc(bbody, 200))
		fmt.Printf("C211T_BUCKET_CREATE proj=%s name=%s http=%d verbatim=%s\n", p.Name, bname, bst, c211xTrunc(bbody, 240))
		if bst == 200 {
			bucketCreated = bname
			ist, ibody := c211tBucketIam(ftok, bname, p.Num, fleetEmail)
			fmt.Printf("C211T_BUCKET_IAM proj=%s http=%d verbatim=%s\n", p.Name, ist, c211xTrunc(ibody, 240))
			if ist == 200 {
				usedBucket = bname
			}
		}
		r.Bucket = usedBucket

		display := strings.ToLower(fmt.Sprintf("%s-vtx-%s", c211tPrefix, c211tShortProj(p.Name)))
		if len(display) > 128 {
			display = display[:128]
		}
		script := c211tScript(p.Name)
		jobSpec := map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType": "n1-standard-4", "acceleratorType": "NVIDIA_TESLA_T4", "acceleratorCount": 1,
				},
				"replicaCount": 1,
				"containerSpec": map[string]interface{}{
					"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
					"command":  []interface{}{"/bin/bash", "-c"},
					"args":     []interface{}{script},
				},
			}},
		}
		if usedBucket != "" {
			jobSpec["baseOutputDirectory"] = map[string]interface{}{
				"outputUriPrefix": "gs://" + usedBucket + "/" + c211tNonce,
			}
		}
		jobBody := map[string]interface{}{
			"displayName": display,
			"labels":      map[string]interface{}{"c211": c211tNonce},
			"jobSpec":     jobSpec,
		}
		body, _ := json.Marshal(jobBody)
		createURL := "https://" + chosen + "-aiplatform.googleapis.com/v1/projects/" + p.Num + "/locations/" + chosen + "/customJobs"
		fmt.Printf("C211T_REQUEST proj=%s region=%s bytes=%d script_bytes=%d bucket=%s\n", p.Name, chosen, len(body), len(script), usedBucket)
		cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
		fmt.Printf("C211T_CREATE proj=%s region=%s http=%d verbatim=%s\n", p.Name, chosen, cst, c211xTrunc(cb, 700))
		if cst != 200 && usedBucket != "" && strings.Contains(strings.ToLower(cb), "output") {
			// baseOutputDirectory rejected: retry the same create once without it.
			delete(jobBody["jobSpec"].(map[string]interface{}), "baseOutputDirectory")
			body2, _ := json.Marshal(jobBody)
			cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(body2))
			r.BucketMake += fmt.Sprintf(" | retry_without_output http=%d", cst)
			fmt.Printf("C211T_CREATE_RETRY proj=%s http=%d verbatim=%s\n", p.Name, cst, c211xTrunc(cb, 500))
		}
		r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 600))
		if cst != 200 {
			r.Verdict = "CREATE_REFUSED"
			if bucketCreated != "" {
				r.BucketClean = c211tBucketCleanup(ftok, bucketCreated)
				fmt.Printf("C211T_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketClean)
			}
			fmt.Printf("C211T_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}
		cm := c211xJSON(cb)
		r.Job = c211xStr(cm, "name")
		r.Spec = c211tMachineSpec(cm)
		fmt.Printf("C211T_JOB proj=%s name=%s\n", p.Name, r.Job)
		fmt.Printf("C211T_CREATE_SPEC proj=%s stored={%s} %s\n", p.Name, r.Spec, c211tJobState(cm))

		jobID := r.Job
		if i := strings.LastIndex(jobID, "/"); i >= 0 {
			jobID = jobID[i+1:]
		}
		jurl := "https://" + chosen + "-aiplatform.googleapis.com/v1/" + r.Job

		terminal := false
		logText := ""
		lastLogRead := time.Time{}
		pollEnd := time.Now().Add(13 * time.Minute)
		for i := 0; time.Now().Before(pollEnd) && time.Now().Before(deadline) && !terminal; i++ {
			gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
			gm := c211xJSON(gb)
			state := c211xStr(gm, "state")
			if state != "" && (len(r.States) == 0 || r.States[len(r.States)-1] != state) {
				r.States = append(r.States, state)
			}
			fmt.Printf("C211T_POLL proj=%s i=%d http=%d state=%s\n", p.Name, i, gst, state)
			if state != "" && state != "JOB_STATE_PENDING" && time.Since(lastLogRead) > 20*time.Second {
				txt, qv := c211tLogs(ftok, p.Num, jobID)
				lastLogRead = time.Now()
				if txt != "" {
					logText += txt
				}
				r.LogQuery = qv
				fmt.Printf("C211T_LOG_READ proj=%s bytes=%d q=%s\n", p.Name, len(txt), qv)
			}
			switch state {
			case "JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED", "JOB_STATE_PARTIALLY_SUCCEEDED":
				terminal = true
			}
			if terminal {
				break
			}
			time.Sleep(15 * time.Second)
		}

		// Post-terminal evidence collection: log retries (ingestion delay is real)
		// plus the independent GCS sink.
		gcsText := ""
		for k := 0; k < 8; k++ {
			txt, qv := c211tLogs(ftok, p.Num, jobID)
			r.LogQuery = qv
			if txt != "" {
				logText += txt
			}
			if usedBucket != "" && gcsText == "" {
				objs, olst, _ := c211tBucketListObjects(ftok, usedBucket, c211tNonce)
				if len(objs) > 0 {
					parts := []string{}
					for _, o := range objs {
						ost, obody := c211tObjectRead(ftok, usedBucket, o)
						parts = append(parts, fmt.Sprintf("%s http=%d bytes=%d", o, ost, len(obody)))
						if ost == 200 {
							gcsText += obody + "\n"
						}
					}
					r.GCSRead = strings.Join(parts, " ; ") + fmt.Sprintf(" list_http=%d", olst)
				} else {
					r.GCSRead = fmt.Sprintf("no_objects list_http=%d prefix=%s", olst, c211tNonce)
				}
			}
			if len(c211tUUIDs(logText+gcsText)) > 0 {
				break
			}
			if k < 7 {
				time.Sleep(20 * time.Second)
			}
		}
		r.LogBytes = len(logText)
		r.GCSBytes = len(gcsText)
		allText := logText + "\n" + gcsText
		r.UUIDs = c211tUUIDs(allText)
		lines := []string{}
		for _, ln := range strings.Split(allText, "\n") {
			if strings.Contains(ln, "C211T_") || strings.Contains(ln, "GPU-") {
				lines = append(lines, c211xTrunc(ln, 400))
			}
		}
		r.Device = c211xTrunc(strings.Join(lines, " || "), 4000)
		fmt.Printf("C211T_LOG proj=%s log_bytes=%d gcs_bytes=%d uuids=%d q=%s gcs=%s\n",
			p.Name, r.LogBytes, r.GCSBytes, len(r.UUIDs), r.LogQuery, c211xTrunc(r.GCSRead, 300))
		for _, ln := range lines {
			fmt.Printf("C211T_DEVTEXT proj=%s %s\n", p.Name, ln)
		}

		rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
		rbm := c211xJSON(rbBody)
		r.Readback = fmt.Sprintf("http=%d %s %s", rbSt, c211tJobState(rbm), c211tMachineSpec(rbm))
		fmt.Printf("C211T_READBACK proj=%s %s\n", p.Name, r.Readback)

		dst, getAfter, note := c211tRetire(ftok, chosen, r.Job)
		lcSt, lcBody := c211xHTTP("GET", "https://"+chosen+"-aiplatform.googleapis.com/v1/projects/"+p.Num+
			"/locations/"+chosen+"/customJobs?pageSize=100", ftok, "", "")
		leftItems := []string{}
		for _, it := range c211tList(c211xJSON(lcBody)["customJobs"]) {
			dn := c211xStr(c211tMap(it), "displayName")
			if strings.HasPrefix(dn, c211tPrefix) {
				leftItems = append(leftItems, dn+"="+c211xStr(c211tMap(it), "state"))
			}
		}
		r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d c211_items=%d %v %s",
			dst, getAfter, lcSt, len(leftItems), leftItems, note)
		fmt.Printf("C211T_ABSENCE proj=%s %s\n", p.Name, r.Absence)

		if bucketCreated != "" {
			r.BucketClean = c211tBucketCleanup(ftok, bucketCreated)
			fmt.Printf("C211T_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketClean)
		}

		if len(r.UUIDs) > 0 {
			r.Verdict = "VERTEX_T4_DEVICE_PROVEN"
			proven = append(proven, p.Name+":"+strings.Join(r.UUIDs, ","))
		} else {
			r.Verdict = "NO_DEVICE_LINE"
		}
		fmt.Printf("C211T_VERDICT proj=%s %s uuids=%d\n", p.Name, r.Verdict, len(r.UUIDs))
	}

	fmt.Printf("C211T_MATRIX %s\n", c211xJSONString(results))
	for _, pv := range proven {
		fmt.Printf("C211T_PROVEN %s\n", pv)
	}
	fmt.Printf("C211T_VERDICT_FINAL elapsed=%ds proven=%d\n", int(time.Since(started).Seconds()), len(proven))
}
