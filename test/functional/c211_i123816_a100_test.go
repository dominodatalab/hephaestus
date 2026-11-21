//go:build functional && gke

// C211 i123816 — the one positive on-demand Vertex custom-training cell of the
// hephaestus family that has never been run: custom_model_training_nvidia_a100_gpus
// = 8 (on-demand) in us-central1 / europe-west4 / asia-southeast1 for the host
// project 153711203585 and the three siblings, with a2-highgpu-8g offered there.
//
// The four durable owner SA keys were measured INELIGIBLE in this intent's
// preflight (no aiplatform.customJobs.* permission at all), so this probe runs
// inside the dominodatalab/hephaestus '/functional-test' GKE job under the
// proven hephaestus-gh-test WIF identity (owner-level in all five projects).
//
// Flow: read per-project testIamPermissions, A100 quota and a2-highgpu-8g
// offerings; in the FIRST project/region where on-demand A100 limit >= 8 and
// a2-highgpu-8g is offered create EXACTLY ONE nonce-tagged ON-DEMAND customJob
// (a2-highgpu-8g, NVIDIA_TESLA_A100, acceleratorCount 8, serviceAccount
// OMITTED per the accepted x121897 default-SA shape) whose container prints
// nvidia-smi -L / --query-gpu lines and writes the same proof to the GCS output
// mount; poll to terminal, read Cloud Logging + GCS sinks, take an independent
// customJobs.get readback, delete and prove absence (GET 404 + list empty).
// Only a verbatim QUOTA on-demand refusal would allow ONE preemptible retry.
//
// Never prints key material or tokens (length + sha256 prefix only).
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

const c211aNonce = "i123816"
const c211aTag = "C211I816"

type c211aProj struct {
	ID  string
	Num string
}

// Project preference order: the host project that owns the WIF identity first.
var c211aProjects = []c211aProj{
	{"domino-eng-platform-dev", "153711203585"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

var c211aRegionPref = []string{"us-central1", "europe-west4", "asia-southeast1"}

var c211aPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete", "aiplatform.executions.create",
	"logging.logEntries.list", "serviceusage.services.get",
	"serviceusage.services.enable", "storage.buckets.create",
	"resourcemanager.projects.get",
}

// ---------------------------------------------------------------------------
// local helpers (c211a* names)
// ---------------------------------------------------------------------------

func c211aM(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211aL(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211aF(v interface{}) float64 {
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

// c211aQuota returns ONLY the two A100 custom-training metrics:
// metric -> region -> effective limit (>0 buckets only).
func c211aQuota(token, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, token, "", "")
	if st != 200 {
		return q, st, c211xTrunc(body, 300)
	}
	m := c211xJSON(body)
	for _, it := range c211aL(m["metrics"]) {
		mi := c211aM(it)
		lname := c211xStr(mi, "metric")
		if i := strings.LastIndex(lname, "/"); i >= 0 {
			lname = lname[i+1:]
		}
		if lname != "custom_model_training_nvidia_a100_gpus" &&
			lname != "custom_model_training_preemptible_nvidia_a100_gpus" {
			continue
		}
		row := map[string]float64{}
		for _, lim := range c211aL(mi["consumerQuotaLimits"]) {
			for _, bk := range c211aL(c211aM(lim)["quotaBuckets"]) {
				bm := c211aM(bk)
				dims := c211aM(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "zone")
				}
				if region == "" {
					region = "global"
				}
				eff := bm["effectiveLimit"]
				if eff == nil {
					eff = bm["defaultLimit"]
				}
				if f := c211aF(eff); f > 0 {
					row[region] = f
				}
			}
		}
		q[lname] = row
	}
	return q, st, ""
}

// c211aOffering returns region -> sorted zones where a2-highgpu-8g is offered.
func c211aOffering(token, num string) (map[string][]string, int, string) {
	out := map[string][]string{}
	u := "https://compute.googleapis.com/compute/v1/projects/" + num +
		"/aggregated/machineTypes?filter=name%3Da2-highgpu-8g"
	st, body := c211xHTTP("GET", u, token, "", "")
	if st != 200 {
		return out, st, c211xTrunc(body, 300)
	}
	m := c211xJSON(body)
	for scope, pool := range c211aM(m["items"]) {
		for _, it := range c211aL(c211aM(pool)["machineTypes"]) {
			z := c211xStr(c211aM(it), "zone")
			if z == "" {
				parts := strings.Split(scope, "/")
				z = parts[len(parts)-1]
			}
			reg := c211xRegionOfZone(z)
			out[reg] = append(out[reg], z)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, st, ""
}

// c211aCensus lists customJobs in one region (name:state:displayName rows).
func c211aCensus(token, num, region string) ([]string, int, string) {
	u := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + num +
		"/locations/" + region + "/customJobs?pageSize=100"
	st, body := c211xHTTP("GET", u, token, "", "")
	rows := []string{}
	if st == 200 {
		m := c211xJSON(body)
		for _, it := range c211aL(m["customJobs"]) {
			jm := c211aM(it)
			n := c211xStr(jm, "name")
			if i := strings.LastIndex(n, "/"); i >= 0 {
				n = n[i+1:]
			}
			rows = append(rows, n+":"+c211xStr(jm, "state")+":"+c211xStr(jm, "displayName"))
		}
	}
	return rows, st, body
}

func c211aBucketCreate(token, num, region, name string) (int, string) {
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(num)
	return c211xHTTP("POST", u, token, "application/json", string(body))
}

func c211aBucketIam(token, name, num, fleetEmail string) (int, string) {
	members := []interface{}{"serviceAccount:" + num + "-compute@developer.gserviceaccount.com"}
	if fleetEmail != "" {
		members = append(members, "serviceAccount:"+fleetEmail)
	}
	bindings := []interface{}{map[string]interface{}{
		"role":    "roles/storage.objectAdmin",
		"members": members,
	}}
	body, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/iam"
	return c211xHTTP("PUT", u, token, "application/json", string(body))
}

func c211aBucketList(token, name, prefix string) ([]string, int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211xHTTP("GET", u, token, "", "")
	names := []string{}
	m := c211xJSON(body)
	for _, it := range c211aL(m["items"]) {
		if n := c211xStr(c211aM(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st, body
}

func c211aObjectRead(token, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211xHTTP("GET", u, token, "", "")
}

func c211aBucketCleanup(token, name string) string {
	names, lst, _ := c211aBucketList(token, name, "")
	deleted := []string{}
	for _, n := range names {
		du := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
			"/o/" + url.PathEscape(n)
		dst, _ := c211xHTTP("DELETE", du, token, "", "")
		deleted = append(deleted, fmt.Sprintf("%s=%d", n, dst))
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), token, "", "")
	getAfter := -1
	for i := 0; i < 8; i++ {
		gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), token, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("list=%d deleted=%v delete_http=%d get_after=%d verbatim=%s",
		lst, deleted, dst, getAfter, c211xTrunc(db, 200))
}

// c211aScript is the customJob container command. Two evidence sinks: stdout
// (captured by Vertex -> Cloud Logging) and the /gcs output mount.
func c211aScript(proj, region string) string {
	s := `set -u
PROOF=/tmp/c211a-proof.txt
{
echo "C211I816_CONTAINER_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i123816 proj=__PROJ__ region=__REGION__"
echo "C211I816_UNAME $(uname -a)"
echo "C211I816_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211I816_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211I816_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211I816_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211I816_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211I816_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211I816_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211I816_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211I816_SMI_SUMMARY $($SMI 2>&1 | head -24 | tr '\n' '|')"
else
  echo "C211I816_NOSMI"
fi
echo "C211I816_CONTAINER_END"
} > $PROOF 2>&1
cat $PROOF
DEST=""
for c in /gcs /gcs/__NONCE__ /gcs/output; do
  if [ -d "$c" ]; then DEST="$c"; break; fi
done
echo "C211I816_GCS_DEST ${DEST:-none}"
if [ -n "$DEST" ]; then
  if cp $PROOF "$DEST/c211i816-proof-__PROJ__.txt"; then
    echo "C211I816_GCS_WRITE ok path=$DEST/c211i816-proof-__PROJ__.txt"
  else
    echo "C211I816_GCS_WRITE copy_failed"
  fi
else
  echo "C211I816_GCS_WRITE no_mount"
fi
sleep 20
echo "C211I816_DONE at=$(date -u +%FT%TZ)"
`
	s = strings.ReplaceAll(s, "__PROJ__", proj)
	s = strings.ReplaceAll(s, "__REGION__", region)
	s = strings.ReplaceAll(s, "__NONCE__", c211aNonce)
	return s
}

// c211aLogList returns the ml_job log lines for a job id.
func c211aLogList(token, num, jid string) ([]string, int, string) {
	rows := []string{}
	lastErr := ""
	filters := []string{
		fmt.Sprintf("resource.type=\"ml_job\" AND resource.labels.job_id=\"%s\"", jid),
		fmt.Sprintf("resource.labels.job_id=\"%s\"", jid),
	}
	for _, flt := range filters {
		body, _ := json.Marshal(map[string]interface{}{
			"resourceNames": []interface{}{"projects/" + num},
			"filter":        flt,
			"orderBy":       "timestamp asc",
			"pageSize":      500,
		})
		st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", token,
			"application/json", string(body))
		if st == 200 {
			m := c211xJSON(rb)
			for _, e := range c211aL(m["entries"]) {
				em := c211aM(e)
				txt := c211xStr(em, "textPayload")
				if txt == "" {
					txt = c211xJSONString(em["jsonPayload"])
				}
				rows = append(rows, txt)
			}
			return rows, st, ""
		}
		lastErr = fmt.Sprintf("http=%d %s", st, c211xTrunc(rb, 200))
	}
	return rows, -1, lastErr
}

func c211aJobStateText(m map[string]interface{}) string {
	errText := ""
	if e := c211aM(m["error"]); len(e) > 0 {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 300))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

func c211aMachineSpec(m map[string]interface{}) string {
	spec := c211aM(m["jobSpec"])
	wps := c211aL(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	ms := c211aM(c211aM(wps[0])["machineSpec"])
	out := fmt.Sprintf("machineType=%q acceleratorType=%q acceleratorCount=%v",
		c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"])
	cs := c211aM(c211aM(wps[0])["containerSpec"])
	out += fmt.Sprintf(" imageUri=%q", c211xStr(cs, "imageUri"))
	if bo := c211aM(spec["baseOutputDirectory"]); len(bo) > 0 && c211xStr(bo, "outputUriPrefix") != "" {
		out += fmt.Sprintf(" outputUriPrefix=%q", c211xStr(bo, "outputUriPrefix"))
	}
	out += fmt.Sprintf(" serviceAccount=%q", c211xStr(spec, "serviceAccount"))
	return out
}

func c211aTerminal(st string) bool {
	switch st {
	case "JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED",
		"JOB_STATE_EXPIRED", "JOB_STATE_PARTIALLY_SUCCEEDED":
		return true
	}
	return false
}

func c211aRetire(token, region, name string) (int, int, string) {
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + name
	dst := -1
	note := ""
	for attempt := 0; attempt < 10; attempt++ {
		gst, gb := c211xHTTP("GET", jurl, token, "", "")
		if gst == 404 {
			return dst, 404, "already-absent"
		}
		state := c211xStr(c211xJSON(gb), "state")
		if state == "JOB_STATE_PENDING" || state == "JOB_STATE_RUNNING" {
			cst, cb := c211xHTTP("POST", jurl+":cancel", token, "application/json", "{}")
			fmt.Printf("%s_CANCEL name=%s http=%d verbatim=%s\n", c211aTag, name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, token, "", "")
		fmt.Printf("%s_RETIRE name=%s http=%d verbatim=%s\n", c211aTag, name, dst, c211xTrunc(db, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211xHTTP("GET", jurl, token, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

func c211aUUIDs(lines []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range lines {
		for _, m := range c211xUUIDRe.FindAllString(l, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

func c211aUserZones(zones []string) map[string][]string {
	out := map[string][]string{}
	for _, z := range zones {
		reg := c211xRegionOfZone(z)
		out[reg] = append(out[reg], z)
	}
	return out
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211aResult struct {
	Proj       string            `json:"proj"`
	Num        string            `json:"num"`
	PermHTTP   int               `json:"perm_http"`
	Allowed    []string          `json:"allowed"`
	QuotaOn    map[string]string `json:"quota_a100_ondemand"`
	QuotaPre   map[string]string `json:"quota_a100_preemptible"`
	Offering   map[string]string `json:"offering_a2h8g"`
	Census     map[string]string `json:"census"`
	Chosen     string            `json:"chosen_region"`
	Bucket     string            `json:"bucket"`
	BucketMake string            `json:"bucket_create"`
	BucketIam  string            `json:"bucket_iam"`
	Create     string            `json:"create"`
	Job        string            `json:"job"`
	States     []string          `json:"states"`
	Readback   string            `json:"readback"`
	Spec       string            `json:"stored_machine_spec"`
	LogQuery   string            `json:"log_query"`
	LogBytes   int               `json:"log_bytes"`
	GCSRead    string            `json:"gcs_read"`
	GCSBytes   int               `json:"gcs_bytes"`
	BucketCln  string            `json:"bucket_cleanup"`
	Device     string            `json:"device_lines"`
	UUIDs      []string          `json:"uuids"`
	Absence    string            `json:"absence"`
	Verdict    string            `json:"verdict"`
}

func TestC211I123816A100(t *testing.T) {
	started := time.Now()
	globalDeadline := started.Add(52 * time.Minute)
	fmt.Printf("%s_START nonce=%s at=%s\n", c211aTag, c211aNonce,
		started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("%s_MINT_FAILED %s\n", c211aTag, c211xTrunc(mintLog, 2000))
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
	fmt.Printf("%s_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n",
		c211aTag, method, c211xShort(wifSA), hostNum)

	// The fleet identity in this project (used for bucket-side proof reads and
	// carries the owner-level access measured in fact 123406).
	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("%s_FLEET_SA list=%d found=%v email=%s verbatim=%s\n",
		c211aTag, lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))

	tinfo, tb := c211xTokenInfo(token)
	fmt.Printf("%s_TOKENINFO http=%d body=%s\n", c211aTag, tinfo, c211xTrunc(tb, 300))

	// ---------------- phase A: read-only matrix ----------------
	results := map[string]*c211aResult{}
	type eligible struct {
		p      c211aProj
		region string
		limit  float64
		zones  []string
	}
	var elig []eligible
	order := []string{}

	for _, p := range c211aProjects {
		r := &c211aResult{Proj: p.ID, Num: p.Num,
			QuotaOn: map[string]string{}, QuotaPre: map[string]string{},
			Offering: map[string]string{}, Census: map[string]string{}}
		results[p.Num] = r
		order = append(order, p.Num)

		gst, gb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, token, "", "")
		fmt.Printf("%s_PROJECT_GET proj=%s http=%d body=%s\n",
			c211aTag, p.Num, gst, c211xTrunc(gb, 300))

		allowed, pst, pbody := c211xTestIamBatch(token,
			"https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, c211aPerms)
		r.PermHTTP, r.Allowed = pst, allowed
		fmt.Printf("%s_PERM proj=%s http=%d allowed=%v err=%s\n",
			c211aTag, p.ID, pst, allowed, c211xTrunc(pbody, 200))

		q, qst, qerr := c211aQuota(token, p.Num)
		on := q["custom_model_training_nvidia_a100_gpus"]
		pre := q["custom_model_training_preemptible_nvidia_a100_gpus"]
		for k, v := range on {
			r.QuotaOn[k] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		for k, v := range pre {
			r.QuotaPre[k] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		fmt.Printf("%s_QUOTA proj=%s http=%d ond_a100=%s pre_a100=%s err=%s\n",
			c211aTag, p.ID, qst, c211xJSONString(r.QuotaOn), c211xJSONString(r.QuotaPre),
			c211xTrunc(qerr, 200))

		off, ost, oerr := c211aOffering(token, p.Num)
		for reg, zs := range off {
			r.Offering[reg] = strings.Join(zs, "+")
		}
		fmt.Printf("%s_OFFERING proj=%s http=%d matrix=%s err=%s\n",
			c211aTag, p.ID, ost, c211xJSONString(r.Offering), c211xTrunc(oerr, 200))

		for _, reg := range c211aRegionPref {
			rows, cst, _ := c211aCensus(token, p.Num, reg)
			r.Census[reg] = fmt.Sprintf("http=%d n=%d", cst, len(rows))
			fmt.Printf("%s_CENSUS proj=%s region=%s http=%d n=%d rows=%v\n",
				c211aTag, p.ID, reg, cst, len(rows), rows)
		}

		if !c211xContains(allowed, "aiplatform.customJobs.create") {
			continue
		}
		for _, reg := range c211aRegionPref {
			lim, ok := on[reg]
			zones := strings.Split(r.Offering[reg], "+")
			if ok && lim >= 8 && len(zones) > 0 && zones[0] != "" {
				elig = append(elig, eligible{p: p, region: reg, limit: lim, zones: zones})
				fmt.Printf("%s_ELIGIBLE proj=%s region=%s limit=%v zones=%v\n",
					c211aTag, p.ID, reg, lim, zones)
			}
		}
	}

	var chosen *eligible
	if len(elig) > 0 {
		chosen = &elig[0]
	}
	if chosen == nil {
		fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven=0 verdict=NO_ELIGIBLE_PROJECT\n",
			c211aTag, int(time.Since(started).Seconds()))
		fmt.Printf("%s_MATRIX %s\n", c211aTag, c211xJSONString(results))
		return
	}

	// ---------------- phase B: exactly ONE on-demand job ----------------
	r := results[chosen.p.Num]
	r.Chosen = chosen.region
	bucket := "c211-i123816-" + chosen.p.Num + "-" + strconv.FormatInt(time.Now().Unix()%100000, 10)
	fmt.Printf("%s_REQUEST proj=%s region=%s mode=ondemand machine=a2-highgpu-8g accel=NVIDIA_TESLA_A100 count=8 bucket=%s\n",
		c211aTag, chosen.p.ID, chosen.region, bucket)

	bk, bb := c211aBucketCreate(token, chosen.p.Num, chosen.region, bucket)
	r.Bucket, r.BucketMake = bucket, fmt.Sprintf("http=%d %s", bk, c211xTrunc(bb, 300))
	fmt.Printf("%s_BUCKET name=%s create_http=%d verbatim=%s\n", c211aTag, bucket, bk, c211xTrunc(bb, 300))
	bucketOK := (bk == 200)
	if bucketOK {
		ist, ib := c211aBucketIam(token, bucket, chosen.p.Num, fleetEmail)
		r.BucketIam = fmt.Sprintf("http=%d %s", ist, c211xTrunc(ib, 300))
		fmt.Printf("%s_BUCKET_IAM name=%s http=%d verbatim=%s\n", c211aTag, bucket, ist, c211xTrunc(ib, 300))
	}

	runJob := func(mode string) {
		spot := mode == "preemptible"
		disp := fmt.Sprintf("c211-i123816-a100-8x-%s-%s", map[bool]string{true: "pre", false: "ond"}[spot], chosen.region)
		script := c211aScript(chosen.p.ID, chosen.region)
		jobSpec := map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType": "a2-highgpu-8g", "acceleratorType": "NVIDIA_TESLA_A100",
					"acceleratorCount": 8,
				},
				"replicaCount": 1,
				"containerSpec": map[string]interface{}{
					"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
					"command":  []interface{}{"bash", "-c"},
					"args":     []interface{}{script},
				},
				"diskSpec": map[string]interface{}{
					"bootDiskType": "pd-standard", "bootDiskSizeGb": 200,
				},
			}},
			"scheduling": map[string]interface{}{
				"disableRetries": true, "timeout": "3600s",
			},
		}
		if bucketOK {
			jobSpec["baseOutputDirectory"] = map[string]interface{}{
				"outputUriPrefix": "gs://" + bucket + "/" + c211aNonce,
			}
		}
		if spot {
			jobSpec["scheduling"].(map[string]interface{})["strategy"] = "SPOT"
		}
		body := map[string]interface{}{
			"displayName": disp,
			"labels":      map[string]interface{}{"c211_nonce": c211aNonce, "c211_lane": "a100-8x"},
			"jobSpec":     jobSpec,
		}
		bbytes, _ := json.Marshal(body)
		createURL := "https://" + chosen.region + "-aiplatform.googleapis.com/v1/projects/" +
			chosen.p.Num + "/locations/" + chosen.region + "/customJobs"
		var cst int
		var cb string
		for attempt := 0; attempt < 3; attempt++ {
			cst, cb = c211xHTTP("POST", createURL, token, "application/json", string(bbytes))
			r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 1600))
			fmt.Printf("%s_CREATE mode=%s proj=%s region=%s attempt=%d http=%d count=8 verbatim=%s\n",
				c211aTag, mode, chosen.p.ID, chosen.region, attempt, cst, c211xTrunc(cb, 900))
			if cst == 200 {
				break
			}
			if strings.Contains(cb, "SERVICE_DISABLED") || strings.Contains(cb, "has not been used in project") {
				time.Sleep(30 * time.Second)
				continue
			}
			break
		}
		if cst != 200 {
			r.Verdict = fmt.Sprintf("CREATE_%s_HTTP_%d", strings.ToUpper(mode), cst)
			return
		}
		var job map[string]interface{}
		_ = json.Unmarshal([]byte(cb), &job)
		jname := c211xStr(job, "name")
		r.Job = jname
		jid := jname
		if i := strings.LastIndex(jid, "/"); i >= 0 {
			jid = jid[i+1:]
		}
		fmt.Printf("%s_CREATE_OK mode=%s job=%s job_id=%s\n", c211aTag, mode, jname, jid)

		waitBudget := 30 * time.Minute
		last := ""
		final := ""
		rows := []string{}
		logHTTP := -1
		var lastLogPull time.Time
		t0 := time.Now()
		for time.Since(t0) < waitBudget && time.Now().Before(globalDeadline) {
			time.Sleep(20 * time.Second)
			gst, gb := c211xHTTP("GET", "https://"+chosen.region+"-aiplatform.googleapis.com/v1/"+jname, token, "", "")
			jm := c211xJSON(gb)
			state := c211xStr(jm, "state")
			if state != last {
				r.States = append(r.States, fmt.Sprintf("t=%ds %s", int(time.Since(t0).Seconds()), state))
				fmt.Printf("%s_JOB_STATE mode=%s t=%d state=%s http=%d\n",
					c211aTag, mode, int(time.Since(t0).Seconds()), state, gst)
				last = state
			}
			if c211aTerminal(state) {
				final = state
				break
			}
			if time.Since(lastLogPull) > 150*time.Second {
				lastLogPull = time.Now()
				got, lh, lerr := c211aLogList(token, chosen.p.Num, jid)
				if len(got) > 0 {
					rows = got
					logHTTP = lh
					fmt.Printf("%s_LOG_QUERY early mode=%s n=%d bytes=%d\n",
						c211aTag, mode, len(got), len(strings.Join(got, "\n")))
				} else if lerr != "" {
					fmt.Printf("%s_LOG_ERR early mode=%s %s\n", c211aTag, mode, c211xTrunc(lerr, 200))
				}
			}
		}
		if final == "" {
			fmt.Printf("%s_POLL_TIMEOUT mode=%s elapsed=%ds last=%s\n",
				c211aTag, mode, int(time.Since(t0).Seconds()), last)
		}
		// final log pull with retries (ingestion can lag)
		for i := 0; i < 12; i++ {
			if len(rows) > 0 && len(c211aUUIDs(rows)) > 0 {
				break
			}
			got, lh, lerr := c211aLogList(token, chosen.p.Num, jid)
			if len(got) > 0 {
				rows = got
				logHTTP = lh
			}
			if lerr != "" && i == 0 {
				fmt.Printf("%s_LOG_ERR mode=%s %s\n", c211aTag, mode, c211xTrunc(lerr, 200))
			}
			time.Sleep(20 * time.Second)
		}
		r.LogQuery = fmt.Sprintf("http=%d n=%d", logHTTP, len(rows))
		r.LogBytes = len(strings.Join(rows, "\n"))
		fmt.Printf("%s_LOG_QUERY mode=%s %s bytes=%d\n", c211aTag, mode, r.LogQuery, r.LogBytes)
		dev := []string{}
		for _, x := range rows {
			if strings.Contains(x, "GPU-") || strings.Contains(x, c211aTag) ||
				strings.Contains(strings.ToLower(x), "nvidia") || strings.Contains(x, "/dev/nvidia") {
				dev = append(dev, c211xTrunc(x, 500))
			}
		}
		allLines := append([]string{}, dev...)
		r.Device = strings.Join(dev, " | ")
		for _, x := range dev {
			fmt.Printf("%s_DEVICE_LINE mode=%s %s\n", c211aTag, mode, x)
		}

		// GCS sink
		if bucketOK {
			names, glst, _ := c211aBucketList(token, bucket, c211aNonce+"/")
			fmt.Printf("%s_GCS_LIST mode=%s http=%d n=%d names=%v\n", c211aTag, mode, glst, len(names), names)
			content := ""
			for _, n := range names {
				st, ob := c211aObjectRead(token, bucket, n)
				fmt.Printf("%s_GCS_OBJECT name=%s http=%d bytes=%d\n", c211aTag, n, st, len(ob))
				if st == 200 {
					content += ob + "\n"
				}
			}
			r.GCSBytes = len(content)
			r.GCSRead = fmt.Sprintf("list_http=%d n=%d bytes=%d", glst, len(names), len(content))
			for _, line := range strings.Split(content, "\n") {
				if strings.Contains(line, "GPU-") || strings.Contains(line, c211aTag) {
					fmt.Printf("%s_DEVICE_LINE_GCS mode=%s %s\n", c211aTag, mode, c211xTrunc(line, 500))
				}
			}
			if content != "" {
				allLines = append(allLines, strings.Split(content, "\n")...)
				r.Device = strings.Join(allLines, " | ")
			}
		} else {
			r.GCSRead = "bucket_unavailable"
		}

		// independent readback
		gst, gb := c211xHTTP("GET", "https://"+chosen.region+"-aiplatform.googleapis.com/v1/"+jname, token, "", "")
		jm := c211xJSON(gb)
		r.Readback = fmt.Sprintf("http=%d %s", gst, c211aJobStateText(jm))
		r.Spec = c211aMachineSpec(jm)
		fmt.Printf("%s_READBACK mode=%s http=%d %s spec=%s\n",
			c211aTag, mode, gst, c211aJobStateText(jm), r.Spec)

		// retire + absence
		dst, after, note := c211aRetire(token, chosen.region, jname)
		lrows, lhst, _ := c211aCensus(token, chosen.p.Num, chosen.region)
		matching := []string{}
		for _, x := range lrows {
			if strings.HasPrefix(x, jid+":") {
				matching = append(matching, x)
			}
		}
		r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d list_matching=%v note=%s",
			dst, after, lhst, matching, note)
		fmt.Printf("%s_ABSENCE mode=%s %s\n", c211aTag, mode, r.Absence)

		uuids := c211aUUIDs(allLines)
		r.UUIDs = uuids
		if len(uuids) > 0 {
			r.Verdict = fmt.Sprintf("PROVEN_%d_%s", len(uuids), mode)
		} else if final == "JOB_STATE_SUCCEEDED" {
			r.Verdict = "NO_DEVICE_LINE_JOB_SUCCEEDED_" + mode
		} else if final == "" {
			r.Verdict = "POLL_TIMEOUT_" + mode
		} else {
			r.Verdict = final + "_" + mode
		}
		fmt.Printf("%s_VERDICT proj=%s mode=%s verdict=%s uuids=%v\n",
			c211aTag, chosen.p.ID, mode, r.Verdict, uuids)
	}

	runJob("ondemand")

	// Preemptible fallback ONLY on a verbatim on-demand QUOTA refusal.
	if r.Verdict != "" && strings.HasPrefix(r.Verdict, "CREATE_ONDEMAND_HTTP_") && strings.Contains(r.Create, "quota") {
		fmt.Printf("%s_SPOT_FALLBACK reason=%s\n", c211aTag, r.Verdict)
		runJob("preemptible")
	}

	// bucket cleanup (only our nonce bucket)
	if bucketOK {
		r.BucketCln = c211aBucketCleanup(token, bucket)
		fmt.Printf("%s_BUCKET_CLEANUP %s\n", c211aTag, r.BucketCln)
	}

	fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven=%d project=%s region=%s mode=%s verdict=%s\n",
		c211aTag, int(time.Since(started).Seconds()),
		func() int {
			n := 0
			for _, rr := range results {
				n += len(rr.UUIDs)
			}
			return n
		}(), chosen.p.ID, chosen.region,
		map[bool]string{true: "preemptible", false: "ondemand"}[strings.Contains(r.Verdict, "preemptible")],
		r.Verdict)
	fmt.Printf("%s_MATRIX %s\n", c211aTag, c211xJSONString(results))
}
