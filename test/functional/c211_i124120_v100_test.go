//go:build functional && gke

// C211 i124120 — Vertex AI V100 on-demand custom-training cell.
//
// The host project 153711203585 measures custom_model_training_nvidia_v100_gpus
// = 6.0 on-demand (preemptible 10.0) in asia-east1 / europe-west4 / us-central1
// / us-west1 (host_quota_153711203585.json, HTTP 200, 398 metrics) and no
// gcp-wif-hephaestus-vertex V100 training row exists in either handoff tree.
// This probe runs inside the dominodatalab/hephaestus '/functional-test' GKE
// WIF identity, mints the owner-level fleetcommand service-account token via
// iamcredentials.generateAccessToken (the durable host owner key lacks
// aiplatform.customJobs per fact 123804), reads each project's verbatim
// custom_model_training_nvidia_v100_gpus buckets plus the nvidia-tesla-v100
// accelerator and n1-standard-8/16/32 offerings, and creates EXACTLY ONE
// nonce-tagged ON-DEMAND customJob per project (largest offered card count
// covered by the measured per-region limit, cap 4) whose command prints
// nvidia-smi -L and nvidia-smi --query-gpu. Only after a verbatim on-demand
// refusal is ONE preemptible/SPOT variant of the same shape attempted.
// Evidence is collected from Cloud Logging stdout plus an independent GCS
// sink (in-container REST upload with the metadata token), the job is read
// back with projects.locations.customJobs.get and retired to 404/list-empty.
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

const c211v100Nonce = "i124120"
const c211v100Prefix = "c211-i124120"
const c211v100QuotaKey = "aiplatform.googleapis.com/custom_model_training_nvidia_v100_gpus"
const c211v100PreemptKey = "aiplatform.googleapis.com/custom_model_training_preemptible_nvidia_v100_gpus"

type c211v100Proj struct {
	Name string
	Num  string
}

// Host first (the cell the mandate names), then the four siblings.
var c211v100Projects = []c211v100Proj{
	{"domino-eng-platform-dev", "153711203585"},
	{"team-eng-infra-testing", "210628133505"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

// Only regions where the host project measured custom_model_training_nvidia_v100_gpus > 0.
var c211v100RegionPref = []string{"us-central1", "europe-west4", "us-west1", "asia-east1"}

var c211v100ZonesByRegion = map[string][]string{
	"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
	"europe-west4": {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
	"us-west1":     {"us-west1-a", "us-west1-b", "us-west1-c"},
	"asia-east1":   {"asia-east1-a", "asia-east1-b", "asia-east1-c"},
}

var c211v100Perms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"storage.buckets.create", "storage.objects.create",
}

// machine-type ladder: highest card density first
type c211v100Shape struct {
	MachineType string
	AccelCount  int
}

var c211v100Shapes = []c211v100Shape{
	{"n1-standard-32", 4},
	{"n1-standard-16", 2},
	{"n1-standard-8", 1},
}

// ---------------------------------------------------------------------------
// local helpers (c211v100* to avoid collisions with the shared helpers)
// ---------------------------------------------------------------------------

func c211v100List(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211v100Map(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211v100Float(v interface{}) float64 {
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

func c211v100ShortProj(p string) string {
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

func c211v100UUIDs(s string) []string {
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

func c211v100Join(m map[string]string) string {
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

// c211v100Quota reads every GPU consumer quota metric of the project, per region.
func c211v100Quota(tok, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, body
	}
	m := c211xJSON(body)
	for _, mi := range c211v100List(m["metrics"]) {
		mm := c211v100Map(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "gpu") {
			continue
		}
		for _, li := range c211v100List(mm["consumerQuotaLimits"]) {
			lm := c211v100Map(li)
			for _, bi := range c211v100List(lm["quotaBuckets"]) {
				bm := c211v100Map(bi)
				dims := c211v100Map(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211v100Float(bm["effectiveLimit"])
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
// GCS proof sink (best effort; a refusal is recorded verbatim)
// ---------------------------------------------------------------------------

func c211v100BucketName(num string) string {
	return "c211-" + c211v100Nonce + "-" + num
}

func c211v100BucketCreate(tok, num, region string) (string, int, string) {
	name := c211v100BucketName(num)
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(num)
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(body))
	return name, st, rb
}

func c211v100BucketIam(tok, name, num, fleetEmail string) (int, string) {
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

func c211v100BucketListObjects(tok, name, prefix string) ([]string, int) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211xHTTP("GET", u, tok, "", "")
	names := []string{}
	m := c211xJSON(body)
	for _, it := range c211v100List(m["items"]) {
		if n := c211xStr(c211v100Map(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st
}

func c211v100ObjectRead(tok, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211xHTTP("GET", u, tok, "", "")
}

func c211v100BucketCleanup(tok, name string) string {
	names, lst := c211v100BucketListObjects(tok, name, "")
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

func c211v100Script(proj string, shape c211v100Shape, bucket string) string {
	s := `set -u
PROOF=/tmp/c211v100-proof.txt
{
echo "C211V100_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124120 proj=__PROJ__ shape=__SHAPE__"
echo "C211V100_UNAME $(uname -a)"
echo "C211V100_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211V100_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211V100_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211V100_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211V100_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211V100_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211V100_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211V100_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211V100_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>&1 | wc -l)"
else
  echo "C211V100_NOSMI"
fi
echo "C211V100_END"
} > $PROOF 2>&1
cat $PROOF
GBUCKET="__BUCKET__"
DEST=""
for c in /gcs /gcs/__NONCE__ /gcs/output; do
  if [ -d "$c" ]; then DEST="$c"; break; fi
done
echo "C211V100_GCS_DEST ${DEST:-none}"
if [ -n "$DEST" ]; then
  if cp $PROOF "$DEST/c211v100-proof-__PROJ__.txt"; then
    echo "C211V100_GCS_MOUNT_WRITE ok path=$DEST/c211v100-proof-__PROJ__.txt"
  else
    echo "C211V100_GCS_MOUNT_WRITE copy_failed"
  fi
fi
if [ -n "$GBUCKET" ]; then
  MTOK=$(curl -s -m 10 -H "Metadata-Flavor: Google" http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token | sed -n 's/.*"access_token"[ ]*:[ ]*"\([^"]*\)".*/\1/p')
  if [ -n "$MTOK" ]; then
    US=$(curl -s -m 30 -o /tmp/c211v100-upload.out -w '%{http_code}' -X POST --data-binary @$PROOF -H "Authorization: Bearer $MTOK" -H "Content-Type: text/plain" "https://storage.googleapis.com/upload/storage/v1/b/$GBUCKET/o?uploadType=media&name=__NONCE__/c211v100-proof-__PROJ__.txt")
    echo "C211V100_GCS_UPLOAD http=$US bucket=$GBUCKET"
  else
    echo "C211V100_GCS_UPLOAD token_failed"
  fi
fi
sleep 25
echo "C211V100_DONE at=$(date -u +%FT%TZ)"
`
	s = strings.ReplaceAll(s, "__PROJ__", proj)
	s = strings.ReplaceAll(s, "__SHAPE__", fmt.Sprintf("%s-x%d", shape.MachineType, shape.AccelCount))
	s = strings.ReplaceAll(s, "__NONCE__", c211v100Nonce)
	s = strings.ReplaceAll(s, "__BUCKET__", bucket)
	return s
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func c211v100LogList(body string) (string, int) {
	m := c211xJSON(body)
	entries, ok := m["entries"].([]interface{})
	if !ok {
		return "", 0
	}
	var b strings.Builder
	for _, e := range entries {
		em := c211v100Map(e)
		if ts := c211xStr(em, "timestamp"); ts != "" {
			b.WriteString(ts + " ")
		}
		if p, ok := em["textPayload"].(string); ok {
			b.WriteString(p)
		}
		if jp := c211v100Map(em["jsonPayload"]); jp != nil {
			b.WriteString(c211xJSONString(jp))
		}
		if pb := c211v100Map(em["protoPayload"]); pb != nil {
			b.WriteString(c211xJSONString(pb))
		}
		b.WriteString("\n")
	}
	return b.String(), len(entries)
}

func c211v100LogsOnce(tok, num, filter string) (int, string, int) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + num},
		"filter":        filter,
		"pageSize":      300,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	txt, n := c211v100LogList(rb)
	return st, txt, n
}

func c211v100Logs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`resource.type="ml_job" AND labels.job_id="` + jobID + `"`,
		`"C211V100_" AND resource.labels.job_id="` + jobID + `"`,
		`textPayload:"C211V100_"`,
		`"C211V100_"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		st, txt, n := c211v100LogsOnce(tok, num, f)
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 90), st, n, len(txt)))
		if txt != "" {
			all.WriteString(txt + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// ---------------------------------------------------------------------------
// job helpers
// ---------------------------------------------------------------------------

func c211v100JobState(m map[string]interface{}) string {
	errText := ""
	if e := c211v100Map(m["error"]); e != nil {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 300))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

func c211v100MachineSpec(m map[string]interface{}) string {
	spec := c211v100Map(m["jobSpec"])
	wps := c211v100List(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	ms := c211v100Map(c211v100Map(wps[0])["machineSpec"])
	out := fmt.Sprintf("machineType=%q acceleratorType=%q acceleratorCount=%v",
		c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"])
	cs := c211v100Map(c211v100Map(wps[0])["containerSpec"])
	out += fmt.Sprintf(" imageUri=%q", c211xStr(cs, "imageUri"))
	if sch := c211v100Map(spec["scheduling"]); sch != nil {
		out += fmt.Sprintf(" strategy=%q timeout=%q", c211xStr(sch, "strategy"), c211xStr(sch, "timeout"))
	}
	return out
}

func c211v100Retire(tok, reg, name string) (int, int, string) {
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
			fmt.Printf("C211V100_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211V100_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(db, 260))
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

func c211v100MTypeOffered(tok, num, zone, mt string) (bool, int) {
	u := "https://www.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/machineTypes/" + mt
	st, _ := c211xHTTP("GET", u, tok, "", "")
	return st == 200, st
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211v100Result struct {
	Proj       string            `json:"proj"`
	Num        string            `json:"num"`
	State      string            `json:"state"`
	PermHTTP   int               `json:"perm_http"`
	Allowed    []string          `json:"allowed"`
	Svc        string            `json:"svc"`
	QuotaV100  map[string]string `json:"quota_v100"`
	QuotaPre   map[string]string `json:"quota_preemptible_v100"`
	Offering   map[string]string `json:"offering"`
	MT         map[string]string `json:"machine_types"`
	Chosen     string            `json:"chosen_region"`
	ChosenZone string            `json:"chosen_zone"`
	Shape      string            `json:"shape"`
	Bucket     string            `json:"bucket"`
	BucketMake string            `json:"bucket_create"`
	Create     string            `json:"create"`
	CreateFB   string            `json:"create_fallback"`
	Spot       string            `json:"spot_attempt"`
	Job        string            `json:"job"`
	States     []string          `json:"states"`
	Readback   string            `json:"readback"`
	Spec       string            `json:"stored_machine_spec"`
	LogQuery   string            `json:"log_query"`
	LogBytes   int               `json:"log_bytes"`
	GCSRead    string            `json:"gcs_read"`
	GCSBytes   int               `json:"gcs_bytes"`
	BucketCle  string            `json:"bucket_cleanup"`
	Device     string            `json:"device_lines"`
	UUIDs      []string          `json:"uuids"`
	Absence    string            `json:"absence"`
	Verdict    string            `json:"verdict"`
}

func TestC211V100VertexOnDemand(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211V100_START nonce=%s at=%s\n", c211v100Nonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211V100_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
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
	fmt.Printf("C211V100_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211V100_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211V100_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}

	results := map[string]*c211v100Result{}
	proven := []string{}
	deadline := started.Add(22 * time.Minute)

	for _, p := range c211v100Projects {
		r := &c211v100Result{Proj: p.Name, Num: p.Num,
			QuotaV100: map[string]string{}, QuotaPre: map[string]string{},
			Offering: map[string]string{}, MT: map[string]string{}}
		results[p.Name] = r
		if time.Until(deadline) < 6*time.Minute {
			r.Verdict = "BUDGET_EXHAUSTED_BEFORE_PROJECT"
			fmt.Printf("C211V100_SKIP proj=%s reason=budget\n", p.Name)
			continue
		}

		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p.Num), ftok, "", "")
		r.State = c211xStr(c211xJSON(pmBody), "lifecycleState")
		fmt.Printf("C211V100_PROJECT proj=%s num=%s http=%d state=%s\n", p.Name, p.Num, pmSt, r.State)

		polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Num)
		al, ast, _ := c211xTestIamBatch(ftok, polURL, c211v100Perms)
		r.PermHTTP = ast
		r.Allowed = al
		fmt.Printf("C211V100_PERM proj=%s http=%d allowed=%v\n", p.Name, ast, al)
		if !c211xContains(al, "aiplatform.customJobs.create") {
			r.Verdict = "PERM_MISSING_CREATE"
			fmt.Printf("C211V100_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+p.Num+"/services/aiplatform.googleapis.com", ftok, "", "")
		r.Svc = c211xStr(c211xJSON(svBody), "state")
		fmt.Printf("C211V100_SVC proj=%s http=%d state=%s\n", p.Name, svSt, r.Svc)
		if r.Svc != "" && r.Svc != "ENABLED" {
			r.Verdict = "AIPLATFORM_" + r.Svc
			fmt.Printf("C211V100_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		quotaMap, qst, qbody := c211v100Quota(ftok, p.Num)
		v100 := map[string]float64{}
		pre := map[string]float64{}
		if quotaMap != nil {
			if quotaMap[c211v100QuotaKey] != nil {
				v100 = quotaMap[c211v100QuotaKey]
			}
			if quotaMap[c211v100PreemptKey] != nil {
				pre = quotaMap[c211v100PreemptKey]
			}
		}
		for _, reg := range c211v100RegionPref {
			if v, ok := v100[reg]; ok {
				r.QuotaV100[reg] = fmt.Sprintf("%.0f", v)
			} else {
				r.QuotaV100[reg] = "absent"
			}
			if v, ok := pre[reg]; ok {
				r.QuotaPre[reg] = fmt.Sprintf("%.0f", v)
			} else {
				r.QuotaPre[reg] = "absent"
			}
		}
		fmt.Printf("C211V100_QUOTA proj=%s http=%d v100=%s preemptible_v100=%s verbatim=%s\n",
			p.Name, qst, c211v100Join(r.QuotaV100), c211v100Join(r.QuotaPre), c211xTrunc(qbody, 160))

		// pick the largest shape offered in a zone of a region whose measured limit covers it
		chosen, chosenZone, chosenShape := "", "", c211v100Shape{}
		for _, shape := range c211v100Shapes {
			for _, reg := range c211v100RegionPref {
				if v100[reg] < float64(shape.AccelCount) {
					continue
				}
				zones := c211v100ZonesByRegion[reg]
				hits := []string{}
				for _, z := range zones {
					types, zst, _ := c211xAccelTypes(ftok, p.Num, z)
					off := c211xContains(types, "nvidia-tesla-v100")
					mtOk, mtSt := c211v100MTypeOffered(ftok, p.Num, z, shape.MachineType)
					if off {
						hits = append(hits, z)
					}
					fmt.Printf("C211V100_OFFER proj=%s region=%s zone=%s http=%d n=%d v100=%v mt_%s_http=%d\n",
						p.Name, reg, z, zst, len(types), off, shape.MachineType, mtSt)
					if off && mtOk && chosen == "" {
						chosen = reg
						chosenZone = z
						chosenShape = shape
					}
					time.Sleep(120 * time.Millisecond)
				}
				if len(hits) > 0 {
					r.Offering[reg] = strings.Join(hits, "+")
				} else if r.Offering[reg] == "" {
					r.Offering[reg] = "none"
				}
				if chosen != "" {
					break
				}
			}
			if chosen != "" {
				break
			}
		}
		if chosen == "" && len(v100) > 0 {
			// record the machine-type offering matrix even when no region qualified
			for _, shape := range c211v100Shapes {
				okAny := false
				for _, reg := range c211v100RegionPref {
					for _, z := range c211v100ZonesByRegion[reg] {
						if ok, _ := c211v100MTypeOffered(ftok, p.Num, z, shape.MachineType); ok {
							okAny = true
						}
					}
				}
				r.MT[shape.MachineType] = fmt.Sprintf("offered_any=%v", okAny)
			}
		}
		r.Chosen = chosen
		r.ChosenZone = chosenZone
		if chosen != "" {
			r.Shape = fmt.Sprintf("%s-x%d", chosenShape.MachineType, chosenShape.AccelCount)
		}
		fmt.Printf("C211V100_CHOICE proj=%s region=%s zone=%s shape=%s quota=%s offering=%s\n",
			p.Name, chosen, chosenZone, r.Shape, r.QuotaV100[chosen], c211v100Join(r.Offering))
		if chosen == "" {
			if v100["us-central1"] == 0 && v100["europe-west4"] == 0 && v100["us-west1"] == 0 && v100["asia-east1"] == 0 {
				r.Verdict = "V100_QUOTA_ZERO"
			} else {
				r.Verdict = "NO_REGION_WITH_QUOTA_AND_V100_OFFERING"
			}
			fmt.Printf("C211V100_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		// Best-effort GCS proof sink.
		usedBucket, bucketCreated := "", ""
		bname, bst, bbody := c211v100BucketCreate(ftok, p.Num, chosen)
		r.BucketMake = fmt.Sprintf("http=%d verbatim=%s", bst, c211xTrunc(bbody, 200))
		fmt.Printf("C211V100_BUCKET_CREATE proj=%s name=%s http=%d verbatim=%s\n", p.Name, bname, bst, c211xTrunc(bbody, 240))
		if bst == 200 {
			bucketCreated = bname
			ist, ibody := c211v100BucketIam(ftok, bname, p.Num, fleetEmail)
			fmt.Printf("C211V100_BUCKET_IAM proj=%s http=%d verbatim=%s\n", p.Name, ist, c211xTrunc(ibody, 240))
			if ist == 200 {
				usedBucket = bname
			}
		}
		r.Bucket = usedBucket

		// ON-DEMAND first: exactly one job alive at a time.
		var jobName string
		var rStates []string
		terminal := false
		logText, gcsText := "", ""
		display := strings.ToLower(fmt.Sprintf("%s-v100-%s-%s", c211v100Prefix, c211v100ShortProj(p.Name), chosenShape.MachineType))
		if len(display) > 128 {
			display = display[:128]
		}
		script := c211v100Script(p.Name, chosenShape, usedBucket)
		jobSpec := map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType": chosenShape.MachineType, "acceleratorType": "NVIDIA_TESLA_V100", "acceleratorCount": chosenShape.AccelCount,
				},
				"replicaCount": 1,
				"diskSpec": map[string]interface{}{
					"bootDiskType": "pd-ssd", "bootDiskSizeGb": 100,
				},
				"containerSpec": map[string]interface{}{
					"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
					"command":  []interface{}{"/bin/bash", "-c"},
					"args":     []interface{}{script},
				},
			}},
			"scheduling": map[string]interface{}{
				"timeout": "900s", "disableRetries": true,
			},
		}
		if usedBucket != "" {
			jobSpec["baseOutputDirectory"] = map[string]interface{}{
				"outputUriPrefix": "gs://" + usedBucket + "/" + c211v100Nonce,
			}
		}
		jobBody := map[string]interface{}{
			"displayName": display,
			"labels":      map[string]interface{}{"c211": c211v100Nonce},
			"jobSpec":     jobSpec,
		}
		body, _ := json.Marshal(jobBody)
		createURL := "https://" + chosen + "-aiplatform.googleapis.com/v1/projects/" + p.Num + "/locations/" + chosen + "/customJobs"
		fmt.Printf("C211V100_REQUEST proj=%s region=%s shape=%s-x%d strategy=ON_DEMAND bytes=%d script_bytes=%d bucket=%s\n",
			p.Name, chosen, chosenShape.MachineType, chosenShape.AccelCount, len(body), len(script), usedBucket)
		cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
		fmt.Printf("C211V100_CREATE proj=%s region=%s shape=%s http=%d verbatim=%s\n",
			p.Name, chosen, chosenShape.MachineType, cst, c211xTrunc(cb, 700))
		if cst != 200 && usedBucket != "" && strings.Contains(strings.ToLower(cb), "output") {
			delete(jobSpec, "baseOutputDirectory")
			body2, _ := json.Marshal(jobBody)
			cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(body2))
			r.BucketMake += fmt.Sprintf(" | retry_without_output http=%d", cst)
			fmt.Printf("C211V100_CREATE_RETRY proj=%s http=%d verbatim=%s\n", p.Name, cst, c211xTrunc(cb, 500))
		}
		r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 600))
		if cst != 200 {
			// verbatim on-demand refusal -> ONE preemptible/SPOT variant of the same shape
			fmt.Printf("C211V100_ONDEMAND_REFUSED proj=%s verbatim=%s\n", p.Name, c211xTrunc(cb, 400))
			spotSpec := map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType": chosenShape.MachineType, "acceleratorType": "NVIDIA_TESLA_V100", "acceleratorCount": chosenShape.AccelCount,
					},
					"replicaCount": 1,
					"diskSpec": map[string]interface{}{
						"bootDiskType": "pd-ssd", "bootDiskSizeGb": 100,
					},
					"containerSpec": map[string]interface{}{
						"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
						"command":  []interface{}{"/bin/bash", "-c"},
						"args":     []interface{}{script},
					},
				}},
				"scheduling": map[string]interface{}{
					"strategy": "SPOT", "timeout": "900s", "disableRetries": true,
				},
			}
			spotBody := map[string]interface{}{
				"displayName": display + "-spot",
				"labels":      map[string]interface{}{"c211": c211v100Nonce},
				"jobSpec":     spotSpec,
			}
			sb, _ := json.Marshal(spotBody)
			sst, srb := c211xHTTP("POST", createURL, ftok, "application/json", string(sb))
			r.Spot = fmt.Sprintf("http=%d %s", sst, c211xTrunc(srb, 600))
			fmt.Printf("C211V100_SPOT_CREATE proj=%s region=%s shape=%s http=%d verbatim=%s\n",
				p.Name, chosen, chosenShape.MachineType, sst, c211xTrunc(srb, 700))
			if sst == 200 {
				cst, cb = sst, srb
			}
		}
		if cst == 200 {
			cm := c211xJSON(cb)
			jobName = c211xStr(cm, "name")
			r.Job = jobName
			r.Spec = c211v100MachineSpec(cm)
			fmt.Printf("C211V100_JOB proj=%s name=%s\n", p.Name, jobName)
			fmt.Printf("C211V100_CREATE_SPEC proj=%s stored={%s} %s\n", p.Name, r.Spec, c211v100JobState(cm))
		} else {
			r.Verdict = "CREATE_REFUSED"
			if bucketCreated != "" {
				r.BucketCle = c211v100BucketCleanup(ftok, bucketCreated)
				fmt.Printf("C211V100_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketCle)
			}
			fmt.Printf("C211V100_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		jobID := jobName
		if i := strings.LastIndex(jobID, "/"); i >= 0 {
			jobID = jobID[i+1:]
		}
		jurl := "https://" + chosen + "-aiplatform.googleapis.com/v1/" + jobName

		lastLogRead := time.Time{}
		pollEnd := time.Now().Add(10 * time.Minute)
		for i := 0; time.Now().Before(pollEnd) && time.Now().Before(deadline) && !terminal; i++ {
			gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
			gm := c211xJSON(gb)
			state := c211xStr(gm, "state")
			if state != "" && (len(rStates) == 0 || rStates[len(rStates)-1] != state) {
				rStates = append(rStates, state)
			}
			r.States = rStates
			fmt.Printf("C211V100_POLL proj=%s i=%d http=%d state=%s\n", p.Name, i, gst, state)
			if state != "" && state != "JOB_STATE_PENDING" && time.Since(lastLogRead) > 20*time.Second {
				txt, qv := c211v100Logs(ftok, p.Num, jobID)
				lastLogRead = time.Now()
				if txt != "" {
					logText += txt
				}
				r.LogQuery = qv
				fmt.Printf("C211V100_LOG_READ proj=%s bytes=%d q=%s\n", p.Name, len(txt), qv)
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

		// Post-terminal evidence collection: log retries plus the independent GCS sink.
		for k := 0; k < 8; k++ {
			txt, qv := c211v100Logs(ftok, p.Num, jobID)
			r.LogQuery = qv
			if txt != "" {
				logText += txt
			}
			if usedBucket != "" && gcsText == "" {
				objs, olst := c211v100BucketListObjects(ftok, usedBucket, c211v100Nonce)
				if len(objs) > 0 {
					parts := []string{}
					for _, o := range objs {
						ost, obody := c211v100ObjectRead(ftok, usedBucket, o)
						parts = append(parts, fmt.Sprintf("%s http=%d bytes=%d", o, ost, len(obody)))
						if ost == 200 {
							gcsText += obody + "\n"
						}
					}
					r.GCSRead = strings.Join(parts, " ; ") + fmt.Sprintf(" list_http=%d", olst)
				} else {
					r.GCSRead = fmt.Sprintf("no_objects list_http=%d prefix=%s", olst, c211v100Nonce)
				}
			}
			if len(c211v100UUIDs(logText+gcsText)) > 0 {
				break
			}
			if k < 7 {
				time.Sleep(20 * time.Second)
			}
		}
		r.LogBytes = len(logText)
		r.GCSBytes = len(gcsText)
		allText := logText + "\n" + gcsText
		r.UUIDs = c211v100UUIDs(allText)
		lines := []string{}
		for _, ln := range strings.Split(allText, "\n") {
			if strings.Contains(ln, "C211V100_") || strings.Contains(ln, "GPU-") {
				lines = append(lines, c211xTrunc(ln, 400))
			}
		}
		r.Device = c211xTrunc(strings.Join(lines, " || "), 4000)
		fmt.Printf("C211V100_LOG proj=%s log_bytes=%d gcs_bytes=%d uuids=%d q=%s gcs=%s\n",
			p.Name, r.LogBytes, r.GCSBytes, len(r.UUIDs), r.LogQuery, c211xTrunc(r.GCSRead, 300))
		for _, ln := range lines {
			fmt.Printf("C211V100_DEVTEXT proj=%s %s\n", p.Name, ln)
		}

		rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
		rbm := c211xJSON(rbBody)
		r.Readback = fmt.Sprintf("http=%d %s %s", rbSt, c211v100JobState(rbm), c211v100MachineSpec(rbm))
		fmt.Printf("C211V100_READBACK proj=%s %s\n", p.Name, r.Readback)

		dst, getAfter, note := c211v100Retire(ftok, chosen, jobName)
		lcSt, lcBody := c211xHTTP("GET", "https://"+chosen+"-aiplatform.googleapis.com/v1/projects/"+p.Num+
			"/locations/"+chosen+"/customJobs?pageSize=100", ftok, "", "")
		leftItems := []string{}
		for _, it := range c211v100List(c211xJSON(lcBody)["customJobs"]) {
			dn := c211xStr(c211v100Map(it), "displayName")
			if strings.HasPrefix(dn, c211v100Prefix) {
				leftItems = append(leftItems, dn+"="+c211xStr(c211v100Map(it), "state"))
			}
		}
		r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d c211_items=%d %v %s",
			dst, getAfter, lcSt, len(leftItems), leftItems, note)
		fmt.Printf("C211V100_ABSENCE proj=%s %s\n", p.Name, r.Absence)

		if bucketCreated != "" {
			r.BucketCle = c211v100BucketCleanup(ftok, bucketCreated)
			fmt.Printf("C211V100_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketCle)
		}

		if len(r.UUIDs) > 0 {
			if r.Spot != "" && strings.HasPrefix(r.Spot, "http=200") && strings.HasPrefix(r.Create, "http=") && !strings.HasPrefix(r.Create, "http=200") {
				r.Verdict = fmt.Sprintf("VERTEX_V100_DEVICE_PROVEN_%s_PREEMPTIBLE", r.Shape)
			} else {
				r.Verdict = fmt.Sprintf("VERTEX_V100_DEVICE_PROVEN_%s", r.Shape)
			}
			proven = append(proven, p.Name+":"+r.Shape+":"+strings.Join(r.UUIDs, ","))
		} else {
			r.Verdict = "NO_DEVICE_LINE"
		}
		fmt.Printf("C211V100_VERDICT proj=%s %s uuids=%d\n", p.Name, r.Verdict, len(r.UUIDs))
	}

	fmt.Printf("C211V100_MATRIX %s\n", c211xJSONString(results))
	for _, pv := range proven {
		fmt.Printf("C211V100_PROVEN %s\n", pv)
	}
	fmt.Printf("C211V100_VERDICT_FINAL elapsed=%ds proven=%d\n", int(time.Since(started).Seconds()), len(proven))
}
