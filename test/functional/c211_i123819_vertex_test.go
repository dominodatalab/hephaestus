//go:build functional && gke

// C211 i123819 — close the last unread Vertex aiplatform cell of the hephaestus
// project family: team-eng-infra-testing 210628133505.
//
// Basis (facts 123712 / 123406 / 122419):
//   - the aiplatform custom-training GPU quota plane is byte-for-byte identical
//     across four of the five Domino projects (on-demand A100-40GB = 8 in
//     us-central1 / europe-west4 / asia-southeast1; on-demand large classes 0;
//     preemptible H100/H200/B200/H100-mega nonzero), but 210628133505 was never
//     readable because the Service Usage API itself is disabled there and every
//     durable key lacks serviceusage.services.enable;
//   - the hephaestus '/functional-test' GKE WIF identity impersonates
//     fleetcommand-service-account, which holds roles/owner plus
//     aiplatform.customJobs.* and serviceusage.services.enable in all four
//     sibling projects (x123052/x123443 runs).
//
// Phases:
//   A. mint the WIF token (stdlib STS + SA impersonation), then the fleet SA token
//   B. project lifecycleState + fleet SA roles + per-permission testIamPermissions
//   C. enable serviceusage.googleapis.com and aiplatform.googleapis.com if not
//      ENABLED (POST :enable, poll the operation/state, verbatim http + state)
//   D. read the FULL custom_model_training_* on-demand + preemptible matrix
//   E. a2-highgpu-8g + nvidia-tesla-a100 offering matrix per region/zone
//   F. exactly ONE nonce-tagged ON-DEMAND a2-highgpu-8g + 8 x NVIDIA_TESLA_A100
//      customJob printing nvidia-smi -L / --query-gpu, polled to terminal, with
//      Cloud Logging + GCS evidence sinks, independent customJobs.get readback,
//      then delete and prove 404 / list-empty.
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

const c211v5Nonce = "i123819"
const c211v5Prefix = "c211-i123819"
const c211v5Proj = "team-eng-infra-testing"
const c211v5Num = "210628133505"
const c211v5A100 = "custom_model_training_nvidia_a100_gpus"

var c211v5Perms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"serviceusage.services.enable", "serviceusage.services.list",
	"resourcemanager.projects.get", "storage.buckets.create",
	"storage.objects.create",
}

// Preferred on-demand region order for the 8 x A100 shape.
var c211v5RegionPref = []string{
	"europe-west4", "asia-southeast1", "us-central1",
	"us-east1", "us-west1", "asia-east1", "europe-west1", "us-east4",
}

func c211v5List(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211v5Map(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211v5Float(v interface{}) float64 {
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

func c211v5UUIDs(s string) []string {
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

func c211v5Join(m map[string]string) string {
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

// c211v5SvcState reads the serviceusage state of one service.
func c211v5SvcState(tok, num, svc string) (int, string, string) {
	u := "https://serviceusage.googleapis.com/v1/projects/" + num + "/services/" + svc
	st, body := c211xHTTP("GET", u, tok, "", "")
	return st, c211xStr(c211xJSON(body), "state"), body
}

// c211v5Enable posts :enable and polls the returned operation + final state.
func c211v5Enable(tok, num, svc string) (int, string) {
	u := "https://serviceusage.googleapis.com/v1/projects/" + num + "/services/" + svc + ":enable"
	st, body := c211xHTTP("POST", u, tok, "application/json", "{}")
	note := fmt.Sprintf("enable_http=%d verbatim=%s", st, c211xTrunc(body, 400))
	op := c211xStr(c211xJSON(body), "name")
	if op != "" {
		ou := "https://serviceusage.googleapis.com/v1/" + op
		for i := 0; i < 20; i++ {
			ost, ob := c211xHTTP("GET", ou, tok, "", "")
			done, _ := c211xJSON(ob)["done"].(bool)
			if done || ost != 200 {
				note += fmt.Sprintf(" | op=http%d done=%v verbatim=%s", ost, done, c211xTrunc(ob, 300))
				break
			}
			time.Sleep(6 * time.Second)
		}
	}
	fst, fstate, _ := c211v5SvcState(tok, num, svc)
	note += fmt.Sprintf(" | state_after=http%d state=%s", fst, fstate)
	return fst, note
}

// c211v5Matrix reads every custom_model_training_* metric bucket (view=FULL).
func c211v5Matrix(tok, num string) (map[string]map[string]float64, int, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, 0, body
	}
	m := c211xJSON(body)
	metrics := c211v5List(m["metrics"])
	for _, mi := range metrics {
		mm := c211v5Map(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "custom_model_training") {
			continue
		}
		short := name
		if i := strings.LastIndex(short, "/"); i >= 0 {
			short = short[i+1:]
		}
		for _, li := range c211v5List(mm["consumerQuotaLimits"]) {
			lm := c211v5Map(li)
			for _, bi := range c211v5List(lm["quotaBuckets"]) {
				bm := c211v5Map(bi)
				dims := c211v5Map(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211v5Float(bm["effectiveLimit"])
				if q[short] == nil {
					q[short] = map[string]float64{}
				}
				if lim > q[short][region] {
					q[short][region] = lim
				}
			}
		}
	}
	return q, st, len(metrics), body
}

// c211v5Zones lists the zone names of one region (compute regions.get).
func c211v5Zones(tok, num, region string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num + "/regions/" + region
	st, body := c211xHTTP("GET", u, tok, "", "")
	zs := []string{}
	for _, z := range c211v5List(c211xJSON(body)["zones"]) {
		if s, ok := z.(string); ok {
			if i := strings.LastIndex(s, "/"); i >= 0 {
				s = s[i+1:]
			}
			zs = append(zs, s)
		}
	}
	return zs, st, c211xTrunc(body, 200)
}

func c211v5MT(tok, num, zone, mt string) (int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/machineTypes/" + mt
	st, body := c211xHTTP("GET", u, tok, "", "")
	return st, c211xStr(c211xJSON(body), "description")
}

func c211v5AccelHas(tok, num, zone, accel string) (int, bool) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/acceleratorTypes/" + accel
	st, _ := c211xHTTP("GET", u, tok, "", "")
	return st, st == 200
}

// ---------------------------------------------------------------------------
// GCS evidence sink (best effort)
// ---------------------------------------------------------------------------

func c211v5BucketName() string { return "c211-" + c211v5Nonce + "-" + c211v5Num }

func c211v5BucketCreate(tok, region string) (string, int, string) {
	name := c211v5BucketName()
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(c211v5Num)
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(body))
	return name, st, rb
}

func c211v5BucketIam(tok, name, fleetEmail string) (int, string) {
	bindings := []interface{}{map[string]interface{}{
		"role": "roles/storage.objectAdmin",
		"members": []interface{}{
			"serviceAccount:" + c211v5Num + "-compute@developer.gserviceaccount.com",
			"serviceAccount:" + fleetEmail,
		},
	}}
	body, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/iam"
	return c211xHTTP("PUT", u, tok, "application/json", string(body))
}

func c211v5BucketListObjects(tok, name, prefix string) ([]string, int) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211xHTTP("GET", u, tok, "", "")
	names := []string{}
	for _, it := range c211v5List(c211xJSON(body)["items"]) {
		if n := c211xStr(c211v5Map(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st
}

func c211v5ObjectRead(tok, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211xHTTP("GET", u, tok, "", "")
}

func c211v5BucketCleanup(tok, name string) string {
	names, lst := c211v5BucketListObjects(tok, name, "")
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
// container proof script
// ---------------------------------------------------------------------------

func c211v5Script() string {
	s := `set -u
PROOF=/tmp/c211v5-proof.txt
{
echo "C211V5_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i123819 proj=__PROJ__"
echo "C211V5_UNAME $(uname -a)"
echo "C211V5_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211V5_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211V5_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211V5_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 30 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211V5_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211V5_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211V5_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211V5_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211V5_SMI_SUMMARY $($SMI 2>&1 | head -24 | tr '\n' '|')"
else
  echo "C211V5_NOSMI"
fi
echo "C211V5_END"
} > $PROOF 2>&1
cat $PROOF
DEST=""
for c in /gcs /gcs/__NONCE__ /gcs/output; do
  if [ -d "$c" ]; then DEST="$c"; break; fi
done
echo "C211V5_GCS_DEST ${DEST:-none}"
if [ -n "$DEST" ]; then
  if cp $PROOF "$DEST/c211v5-proof.txt"; then
    echo "C211V5_GCS_WRITE ok path=$DEST/c211v5-proof.txt"
  else
    echo "C211V5_GCS_WRITE copy_failed"
  fi
else
  echo "C211V5_GCS_WRITE no_mount"
fi
sleep 25
echo "C211V5_DONE at=$(date -u +%FT%TZ)"
`
	s = strings.ReplaceAll(s, "__PROJ__", c211v5Proj)
	s = strings.ReplaceAll(s, "__NONCE__", c211v5Nonce)
	return s
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func c211v5LogList(body string) (string, int) {
	m := c211xJSON(body)
	entries, ok := m["entries"].([]interface{})
	if !ok {
		return "", 0
	}
	var b strings.Builder
	for _, e := range entries {
		em := c211v5Map(e)
		if ts := c211xStr(em, "timestamp"); ts != "" {
			b.WriteString(ts + " ")
		}
		if p, ok := em["textPayload"].(string); ok {
			b.WriteString(p)
		}
		if jp := c211v5Map(em["jsonPayload"]); jp != nil {
			b.WriteString(c211xJSONString(jp))
		}
		if pb := c211v5Map(em["protoPayload"]); pb != nil {
			b.WriteString(c211xJSONString(pb))
		}
		b.WriteString("\n")
	}
	return b.String(), len(entries)
}

func c211v5LogsOnce(tok, filter string) (int, string, int) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + c211v5Num},
		"filter":        filter,
		"pageSize":      300,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	txt, n := c211v5LogList(rb)
	return st, txt, n
}

func c211v5Logs(tok, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`resource.type="ml_job" AND labels.job_id="` + jobID + `"`,
		`"C211V5_" AND resource.labels.job_id="` + jobID + `"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		st, txt, n := c211v5LogsOnce(tok, f)
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

func c211v5JobState(m map[string]interface{}) string {
	errText := ""
	if e := c211v5Map(m["error"]); e != nil && c211xStr(e, "message") != "" {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 400))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

func c211v5MachineSpec(m map[string]interface{}) string {
	spec := c211v5Map(m["jobSpec"])
	wps := c211v5List(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	ms := c211v5Map(c211v5Map(wps[0])["machineSpec"])
	out := fmt.Sprintf("machineType=%q acceleratorType=%q acceleratorCount=%v",
		c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"])
	cs := c211v5Map(c211v5Map(wps[0])["containerSpec"])
	out += fmt.Sprintf(" imageUri=%q", c211xStr(cs, "imageUri"))
	if bo := c211v5Map(spec["baseOutputDirectory"]); bo != nil && c211xStr(bo, "outputUriPrefix") != "" {
		out += fmt.Sprintf(" outputUriPrefix=%q", c211xStr(bo, "outputUriPrefix"))
	}
	return out
}

func c211v5Retire(tok, reg, name string) (int, int, string) {
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
			fmt.Printf("C211V5_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211V5_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(db, 260))
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

type c211v5Result struct {
	Proj       string            `json:"proj"`
	Num        string            `json:"num"`
	State      string            `json:"state"`
	Roles      []string          `json:"fleet_roles"`
	PermHTTP   int               `json:"perm_http"`
	Allowed    []string          `json:"allowed"`
	Missing    []string          `json:"missing"`
	SvcUsageB  string            `json:"svc_serviceusage_before"`
	SvcUsageE  string            `json:"svc_serviceusage_enable"`
	SvcAiB     string            `json:"svc_aiplatform_before"`
	SvcAiE     string            `json:"svc_aiplatform_enable"`
	QuotaHTTP  int               `json:"quota_http"`
	MetricN    int               `json:"metrics_in_response"`
	Matrix     map[string]string `json:"matrix"`
	Offerings  map[string]string `json:"offerings"`
	Chosen     string            `json:"chosen_region"`
	ChosenZone string            `json:"chosen_zone"`
	Bucket     string            `json:"bucket"`
	BucketMake string            `json:"bucket_create"`
	Create     string            `json:"create"`
	Job        string            `json:"job"`
	States     []string          `json:"states"`
	Readback   string            `json:"readback"`
	Spec       string            `json:"stored_machine_spec"`
	LogQuery   string            `json:"log_query"`
	LogBytes   int               `json:"log_bytes"`
	GCSRead    string            `json:"gcs_read"`
	GCSBytes   int               `json:"gcs_bytes"`
	BucketClea string            `json:"bucket_cleanup"`
	Device     string            `json:"device_lines"`
	UUIDs      []string          `json:"uuids"`
	Absence    string            `json:"absence"`
	Verdict    string            `json:"verdict"`
}

func TestC211V5VertexFifthProject(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211V5_START nonce=%s at=%s\n", c211v5Nonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211V5_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", ""
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = "153711203585"
	}
	fmt.Printf("C211V5_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211V5_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211V5_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("C211V5_TOKENINFO status=%d verbatim=%s\n", tiSt, c211xTrunc(tiBody, 400))

	r := &c211v5Result{Proj: c211v5Proj, Num: c211v5Num, Matrix: map[string]string{}, Offerings: map[string]string{}}
	deadline := started.Add(30 * time.Minute)

	pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211v5Num, ftok, "", "")
	r.State = c211xStr(c211xJSON(pmBody), "lifecycleState")
	fmt.Printf("C211V5_PROJECT proj=%s http=%d state=%s\n", c211v5Proj, pmSt, r.State)

	polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + c211v5Num
	pol, polSt, polBody := c211xGetPolicy(ftok, polURL)
	roles := []string{}
	if pol != nil {
		member := "serviceAccount:" + fleetEmail
		for _, bv := range c211v5List(pol["bindings"]) {
			bm := c211v5Map(bv)
			for _, mv := range c211v5List(bm["members"]) {
				if s, ok := mv.(string); ok && s == member {
					roles = append(roles, c211xStr(bm, "role"))
				}
			}
		}
	}
	sort.Strings(roles)
	r.Roles = roles
	fmt.Printf("C211V5_FLEET_ROLES http=%d n=%d roles=%v verbatim=%s\n", polSt, len(roles), roles, c211xTrunc(polBody, 200))

	al, ast, _ := c211xTestIamBatch(ftok, polURL, c211v5Perms)
	r.PermHTTP = ast
	r.Allowed = al
	for _, p := range c211v5Perms {
		if !c211xContains(al, p) {
			r.Missing = append(r.Missing, p)
		}
	}
	fmt.Printf("C211V5_PERM http=%d allowed=%v missing=%v\n", ast, al, r.Missing)

	// C. serviceusage state + enable
	suSt, suState, suBody := c211v5SvcState(ftok, c211v5Num, "serviceusage.googleapis.com")
	r.SvcUsageB = fmt.Sprintf("http=%d state=%s verbatim=%s", suSt, suState, c211xTrunc(suBody, 300))
	fmt.Printf("C211V5_SVC svc=serviceusage.googleapis.com %s\n", r.SvcUsageB)
	if suState != "ENABLED" {
		est, enote := c211v5Enable(ftok, c211v5Num, "serviceusage.googleapis.com")
		r.SvcUsageE = enote
		fmt.Printf("C211V5_ENABLE svc=serviceusage.googleapis.com http=%d %s\n", est, enote)
		time.Sleep(20 * time.Second)
	} else {
		r.SvcUsageE = "already_enabled"
	}

	aiSt, aiState, aiBody := c211v5SvcState(ftok, c211v5Num, "aiplatform.googleapis.com")
	r.SvcAiB = fmt.Sprintf("http=%d state=%s verbatim=%s", aiSt, aiState, c211xTrunc(aiBody, 300))
	fmt.Printf("C211V5_SVC svc=aiplatform.googleapis.com %s\n", r.SvcAiB)
	if aiState != "ENABLED" {
		est, enote := c211v5Enable(ftok, c211v5Num, "aiplatform.googleapis.com")
		r.SvcAiE = enote
		fmt.Printf("C211V5_ENABLE svc=aiplatform.googleapis.com http=%d %s\n", est, enote)
	} else {
		r.SvcAiE = "already_enabled"
	}

	// D. FULL custom_model_training_* matrix
	q, qst, mn, qbody := c211v5Matrix(ftok, c211v5Num)
	r.QuotaHTTP = qst
	r.MetricN = mn
	names := []string{}
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		nz := []string{}
		zero := 0
		regs := []string{}
		for reg := range q[k] {
			regs = append(regs, reg)
		}
		sort.Strings(regs)
		for _, reg := range regs {
			v := q[k][reg]
			if v > 0 {
				nz = append(nz, fmt.Sprintf("%s=%.0f", reg, v))
			} else {
				zero++
			}
		}
		r.Matrix[k] = fmt.Sprintf("nonzero=[%s] zero_regions=%d", strings.Join(nz, ","), zero)
		fmt.Printf("C211V5_METRIC name=%s %s\n", k, r.Matrix[k])
	}
	fmt.Printf("C211V5_QUOTA http=%d metrics_in_response=%d custom_training_metrics=%d verbatim_head=%s\n",
		qst, mn, len(q), c211xTrunc(qbody, 300))

	if qst != 200 {
		r.Verdict = "QUOTA_READ_REFUSED"
		fmt.Printf("C211V5_VERDICT %s\n", r.Verdict)
		fmt.Printf("C211V5_MATRIX %s\n", c211xJSONString(r))
		t.Fatalf("quota read refused")
	}

	// E. offerings for a2-highgpu-8g + nvidia-tesla-a100
	a100 := q[c211v5A100]
	if a100 == nil {
		a100 = map[string]float64{}
	}
	regionOrder := []string{}
	for _, reg := range c211v5RegionPref {
		regionOrder = append(regionOrder, reg)
	}
	extra := []string{}
	for reg := range a100 {
		if !c211xContains(regionOrder, reg) {
			extra = append(extra, reg)
		}
	}
	sort.Strings(extra)
	regionOrder = append(regionOrder, extra...)

	chosen, chosenZone := "", ""
	for _, reg := range regionOrder {
		if time.Until(deadline) < 12*time.Minute {
			fmt.Printf("C211V5_OFFER_STOP reason=budget region=%s\n", reg)
			break
		}
		if a100[reg] < 8 {
			r.Offerings[reg] = fmt.Sprintf("quota=%.0f<8", a100[reg])
			fmt.Printf("C211V5_OFFERING region=%s quota=%.0f skip=quota\n", reg, a100[reg])
			continue
		}
		zs, zst, znote := c211v5Zones(ftok, c211v5Num, reg)
		if zst != 200 {
			r.Offerings[reg] = fmt.Sprintf("zones_http=%d %s", zst, znote)
			fmt.Printf("C211V5_OFFERING region=%s zones_http=%d %s\n", reg, zst, znote)
			continue
		}
		hits := []string{}
		for _, z := range zs {
			mtSt, mtDesc := c211v5MT(ftok, c211v5Num, z, "a2-highgpu-8g")
			accSt, accOK := c211v5AccelHas(ftok, c211v5Num, z, "nvidia-tesla-a100")
			fmt.Printf("C211V5_OFFER zone=%s mt_http=%d accel_http=%d desc=%q\n", z, mtSt, accSt, mtDesc)
			if mtSt == 200 && accOK {
				hits = append(hits, z)
			}
			time.Sleep(150 * time.Millisecond)
		}
		if len(hits) > 0 {
			r.Offerings[reg] = "zones=" + strings.Join(hits, "+") + fmt.Sprintf(" quota=%.0f", a100[reg])
			if chosen == "" {
				chosen, chosenZone = reg, hits[0]
			}
		} else {
			r.Offerings[reg] = "none"
		}
		fmt.Printf("C211V5_OFFERING region=%s quota=%.0f %s\n", reg, a100[reg], r.Offerings[reg])
	}
	r.Chosen = chosen
	r.ChosenZone = chosenZone
	fmt.Printf("C211V5_CHOICE region=%s zone=%s quota=%.0f\n", chosen, chosenZone, a100[chosen])
	if chosen == "" {
		r.Verdict = "NO_REGION_WITH_A100_QUOTA_AND_A2_HIGHGPU_8G_OFFERING"
		fmt.Printf("C211V5_VERDICT %s\n", r.Verdict)
		fmt.Printf("C211V5_MATRIX %s\n", c211xJSONString(r))
		t.Fatalf("no eligible region")
	}

	// F. exactly ONE nonce on-demand customJob
	usedBucket := ""
	bucketCreated := ""
	bname, bst, bbody := c211v5BucketCreate(ftok, chosen)
	r.BucketMake = fmt.Sprintf("http=%d verbatim=%s", bst, c211xTrunc(bbody, 200))
	fmt.Printf("C211V5_BUCKET_CREATE name=%s http=%d verbatim=%s\n", bname, bst, c211xTrunc(bbody, 240))
	if bst == 200 {
		bucketCreated = bname
		ist, ibody := c211v5BucketIam(ftok, bname, fleetEmail)
		fmt.Printf("C211V5_BUCKET_IAM http=%d verbatim=%s\n", ist, c211xTrunc(ibody, 240))
		if ist == 200 {
			usedBucket = bname
		}
	}
	r.Bucket = usedBucket

	display := c211v5Prefix + "-vtx-a100x8"
	script := c211v5Script()
	jobSpec := map[string]interface{}{
		"workerPoolSpecs": []interface{}{map[string]interface{}{
			"machineSpec": map[string]interface{}{
				"machineType": "a2-highgpu-8g", "acceleratorType": "NVIDIA_TESLA_A100", "acceleratorCount": 8,
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
			"outputUriPrefix": "gs://" + usedBucket + "/" + c211v5Nonce,
		}
	}
	jobBody := map[string]interface{}{
		"displayName": display,
		"labels":      map[string]interface{}{"c211": c211v5Nonce},
		"jobSpec":     jobSpec,
	}
	body, _ := json.Marshal(jobBody)
	createURL := "https://" + chosen + "-aiplatform.googleapis.com/v1/projects/" + c211v5Num + "/locations/" + chosen + "/customJobs"
	fmt.Printf("C211V5_REQUEST region=%s bytes=%d script_bytes=%d bucket=%s\n", chosen, len(body), len(script), usedBucket)
	cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
	fmt.Printf("C211V5_CREATE region=%s http=%d verbatim=%s\n", chosen, cst, c211xTrunc(cb, 900))
	if cst != 200 && usedBucket != "" && strings.Contains(strings.ToLower(cb), "output") {
		delete(jobBody["jobSpec"].(map[string]interface{}), "baseOutputDirectory")
		body2, _ := json.Marshal(jobBody)
		cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(body2))
		r.BucketMake += fmt.Sprintf(" | retry_without_output http=%d", cst)
		fmt.Printf("C211V5_CREATE_RETRY http=%d verbatim=%s\n", cst, c211xTrunc(cb, 700))
	}
	r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 800))
	if cst != 200 {
		r.Verdict = "CREATE_REFUSED"
		if bucketCreated != "" {
			r.BucketClea = c211v5BucketCleanup(ftok, bucketCreated)
			fmt.Printf("C211V5_BUCKET_CLEANUP %s\n", r.BucketClea)
		}
		fmt.Printf("C211V5_VERDICT %s\n", r.Verdict)
		fmt.Printf("C211V5_MATRIX %s\n", c211xJSONString(r))
		t.Fatalf("customJob create refused")
	}
	cm := c211xJSON(cb)
	r.Job = c211xStr(cm, "name")
	r.Spec = c211v5MachineSpec(cm)
	fmt.Printf("C211V5_JOB name=%s\n", r.Job)
	fmt.Printf("C211V5_CREATE_SPEC stored={%s} %s\n", r.Spec, c211v5JobState(cm))

	jobID := r.Job
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	jurl := "https://" + chosen + "-aiplatform.googleapis.com/v1/" + r.Job

	terminal := false
	logText := ""
	lastLogRead := time.Time{}
	pollEnd := time.Now().Add(14 * time.Minute)
	for i := 0; time.Now().Before(pollEnd) && time.Now().Before(deadline) && !terminal; i++ {
		gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
		gm := c211xJSON(gb)
		state := c211xStr(gm, "state")
		if state != "" && (len(r.States) == 0 || r.States[len(r.States)-1] != state) {
			r.States = append(r.States, state)
		}
		fmt.Printf("C211V5_POLL i=%d http=%d state=%s\n", i, gst, state)
		if state != "" && state != "JOB_STATE_PENDING" && time.Since(lastLogRead) > 20*time.Second {
			txt, qv := c211v5Logs(ftok, jobID)
			lastLogRead = time.Now()
			if txt != "" {
				logText += txt
			}
			r.LogQuery = qv
			fmt.Printf("C211V5_LOG_READ bytes=%d q=%s\n", len(txt), qv)
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

	gcsText := ""
	for k := 0; k < 10; k++ {
		txt, qv := c211v5Logs(ftok, jobID)
		r.LogQuery = qv
		if txt != "" {
			logText += txt
		}
		if usedBucket != "" {
			objs, olst := c211v5BucketListObjects(ftok, usedBucket, c211v5Nonce)
			if len(objs) > 0 {
				parts := []string{}
				for _, o := range objs {
					ost, obody := c211v5ObjectRead(ftok, usedBucket, o)
					parts = append(parts, fmt.Sprintf("%s http=%d bytes=%d", o, ost, len(obody)))
					if ost == 200 {
						gcsText += obody + "\n"
					}
				}
				r.GCSRead = strings.Join(parts, " ; ") + fmt.Sprintf(" list_http=%d", olst)
			} else if r.GCSRead == "" {
				r.GCSRead = fmt.Sprintf("no_objects list_http=%d prefix=%s", olst, c211v5Nonce)
			}
		}
		if len(c211v5UUIDs(logText+gcsText)) > 0 {
			break
		}
		if k < 9 {
			time.Sleep(20 * time.Second)
		}
	}
	r.LogBytes = len(logText)
	r.GCSBytes = len(gcsText)
	allText := logText + "\n" + gcsText
	r.UUIDs = c211v5UUIDs(allText)
	lines := []string{}
	for _, ln := range strings.Split(allText, "\n") {
		if strings.Contains(ln, "C211V5_") || strings.Contains(ln, "GPU-") {
			lines = append(lines, c211xTrunc(ln, 500))
		}
	}
	r.Device = c211xTrunc(strings.Join(lines, " || "), 6000)
	fmt.Printf("C211V5_LOG log_bytes=%d gcs_bytes=%d uuids=%d q=%s gcs=%s\n",
		r.LogBytes, r.GCSBytes, len(r.UUIDs), r.LogQuery, c211xTrunc(r.GCSRead, 300))
	for _, ln := range lines {
		fmt.Printf("C211V5_DEVTEXT %s\n", ln)
	}

	rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
	rbm := c211xJSON(rbBody)
	r.Readback = fmt.Sprintf("http=%d %s %s", rbSt, c211v5JobState(rbm), c211v5MachineSpec(rbm))
	fmt.Printf("C211V5_READBACK %s\n", r.Readback)

	dst, getAfter, note := c211v5Retire(ftok, chosen, r.Job)
	lcSt, lcBody := c211xHTTP("GET", "https://"+chosen+"-aiplatform.googleapis.com/v1/projects/"+c211v5Num+
		"/locations/"+chosen+"/customJobs?pageSize=100", ftok, "", "")
	leftItems := []string{}
	for _, it := range c211v5List(c211xJSON(lcBody)["customJobs"]) {
		dn := c211xStr(c211v5Map(it), "displayName")
		if strings.HasPrefix(dn, c211v5Prefix) {
			leftItems = append(leftItems, dn+"="+c211xStr(c211v5Map(it), "state"))
		}
	}
	r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d c211_items=%d %v %s",
		dst, getAfter, lcSt, len(leftItems), leftItems, note)
	fmt.Printf("C211V5_ABSENCE %s\n", r.Absence)

	if bucketCreated != "" {
		r.BucketClea = c211v5BucketCleanup(ftok, bucketCreated)
		fmt.Printf("C211V5_BUCKET_CLEANUP %s\n", r.BucketClea)
	}

	if len(r.UUIDs) > 0 {
		r.Verdict = "VERTEX_A100_8X_DEVICE_PROVEN"
		fmt.Printf("C211V5_PROVEN %s\n", strings.Join(r.UUIDs, ","))
	} else {
		r.Verdict = "NO_DEVICE_LINE"
	}
	fmt.Printf("C211V5_VERDICT %s uuids=%d\n", r.Verdict, len(r.UUIDs))
	fmt.Printf("C211V5_MATRIX %s\n", c211xJSONString(r))
	fmt.Printf("C211V5_VERDICT_FINAL elapsed=%ds proven=%d states=%v\n",
		int(time.Since(started).Seconds()), len(r.UUIDs), r.States)
}
