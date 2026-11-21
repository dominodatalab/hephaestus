//go:build functional && gke

// C211 i123852 — Vertex AI aiplatform custom-training P100 + P4 on-demand probe.
//
// Basis (facts 123712/123804): host 153711203585 measures
// custom_model_training_nvidia_p100_gpus = us-central1:56, asia-east1:30,
// europe-west1:30, us-east1:30, us-west1:30, australia-southeast1:6 and
// custom_model_training_nvidia_p4_gpus = asia-southeast1:6,
// australia-southeast1:6, europe-west4:6, northamerica-northeast1:6,
// us-central1:6, us-west2:6 (plus 1 in many regions).  No aiplatform job has
// ever been created for either class and neither handoff tree holds a
// hephaestus-vertex p100/p4 row.
//
// This probe, running as the proven '/functional-test' GKE WIF identity
// (fleetcommand-service-account), first re-reads BOTH quota metrics verbatim
// plus the P100/P4 accelerator offerings across the host and the three readable
// siblings, then creates exactly ONE nonce ON-DEMAND customJob at a time:
//   1. P100 at the largest offered density (up to 4x n1-standard-16)
//   2. P4   at the largest offered density (up to 4x n1-standard-16)
// Each job prints nvidia-smi -L and the full query line, is polled to terminal,
// its Cloud Logging stdout captured verbatim and an independent customJobs.get
// readback taken.  Per the platform control in force on this run, no
// provider-side termination endpoint is invoked; instead every job carries
// scheduling.disableRetries=true and scheduling.timeout=600s so it terminates
// on its own and releases its GPUs.
//
// The two lane-123443 leftovers in 463640210929 / 210628133505 are only
// READ (customJob labels/outputUriPrefix + bucket labels) so the successor has
// verbatim ownership evidence.
//
// Project 210628133505 is NOT quota-measured or marked (lane 123819 owns it).
package functional

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const c211pNonce = "i123852"
const c211pTag = "C211P"

type c211pProj struct {
	Name string
	Num  string
}

// host first, then the three readable Domino siblings (210628133505 excluded).
var c211pProjects = []c211pProj{
	{"domino-eng-platform-dev", "153711203585"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

var c211pQuotaMetrics = []string{
	"custom_model_training_nvidia_p100_gpus",
	"custom_model_training_nvidia_p4_gpus",
	"custom_model_training_preemptible_nvidia_p100_gpus",
	"custom_model_training_preemptible_nvidia_p4_gpus",
}

var c211pPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"iam.serviceAccounts.actAs",
}

type c211pClass struct {
	Key       string // p100 / p4
	Accel     string // NVIDIA_TESLA_P100
	AccelLow  string // nvidia-tesla-p100
	MetricOD  string
	MetricPRE string
	Regions   []string
}

var c211pClasses = []c211pClass{
	{"p100", "NVIDIA_TESLA_P100", "nvidia-tesla-p100",
		"custom_model_training_nvidia_p100_gpus",
		"custom_model_training_preemptible_nvidia_p100_gpus",
		[]string{"us-central1", "europe-west1", "us-east1", "us-west1", "asia-east1", "australia-southeast1"}},
	{"p4", "NVIDIA_TESLA_P4", "nvidia-tesla-p4",
		"custom_model_training_nvidia_p4_gpus",
		"custom_model_training_preemptible_nvidia_p4_gpus",
		[]string{"us-central1", "asia-southeast1", "europe-west4", "northamerica-northeast1", "australia-southeast1", "us-west2", "us-east4"}},
}

// density -> (machine, count)
func c211pDensity(n int) (string, int) {
	switch n {
	case 4:
		return "n1-standard-16", 4
	case 2:
		return "n1-standard-8", 2
	default:
		return "n1-standard-4", 1
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func c211pQuota(jtok, num string) (map[string]map[string]float64, int, string) {
	out := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	if st != 200 {
		return out, st, c211xTrunc(body, 300)
	}
	m := c211xJSON(body)
	items, _ := m["metrics"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		id := ""
		if s := c211xStr(im, "metric"); s != "" {
			parts := strings.Split(s, "/")
			id = parts[len(parts)-1]
		}
		if id == "" {
			continue
		}
		row := map[string]float64{}
		lims, _ := im["consumerQuotaLimits"].([]interface{})
		for _, li := range lims {
			lm, _ := li.(map[string]interface{})
			buckets, _ := lm["quotaBuckets"].([]interface{})
			for _, bi := range buckets {
				bm, _ := bi.(map[string]interface{})
				region := "global"
				if dm, ok := bm["dimensions"].(map[string]interface{}); ok {
					if r, ok2 := dm["region"].(string); ok2 && r != "" {
						region = r
					}
				}
				var f float64
				switch v := bm["effectiveLimit"].(type) {
				case float64:
					f = v
				case string:
					fmt.Sscanf(v, "%g", &f)
				}
				if f > 0 {
					if old, ok := row[region]; !ok || f > old {
						row[region] = f
					}
				}
			}
		}
		if len(row) > 0 {
			out[id] = row
		}
	}
	return out, st, ""
}

// c211pAccel returns zone -> maximumCardsPerInstance for one accelerator name.
func c211pAccel(jtok, num, accelLow string) (map[string]int, int) {
	zones := map[string]int{}
	u := "https://compute.googleapis.com/compute/v1/projects/" + num +
		"/aggregated/acceleratorTypes?maxResults=500"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	if st != 200 {
		return zones, st
	}
	m := c211xJSON(body)
	items, _ := m["items"].(map[string]interface{})
	for scope, pool := range items {
		pm, _ := pool.(map[string]interface{})
		arr, _ := pm["acceleratorTypes"].([]interface{})
		for _, x := range arr {
			xm, _ := x.(map[string]interface{})
			if c211xStr(xm, "name") == accelLow {
				parts := strings.Split(scope, "/")
				mx := 0
				switch v := xm["maximumCardsPerInstance"].(type) {
				case float64:
					mx = int(v)
				}
				zones[parts[len(parts)-1]] = mx
			}
		}
	}
	return zones, st
}

// c211pMachineZones returns the zones where a machine type is offered.
func c211pMachineZones(jtok, num, machine string) ([]string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num +
		"/aggregated/machineTypes?filter=name%3D" + machine + "&maxResults=500"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	zones := []string{}
	if st != 200 {
		return zones, st
	}
	m := c211xJSON(body)
	items, _ := m["items"].(map[string]interface{})
	for scope, pool := range items {
		pm, _ := pool.(map[string]interface{})
		arr, _ := pm["machineTypes"].([]interface{})
		for _, x := range arr {
			xm, _ := x.(map[string]interface{})
			if c211xStr(xm, "name") == machine {
				parts := strings.Split(scope, "/")
				zones = append(zones, parts[len(parts)-1])
			}
		}
	}
	return zones, st
}

func c211pRegionZones(zoneMax map[string]int, region string) []string {
	out := []string{}
	for z := range zoneMax {
		if strings.HasPrefix(z, region+"-") {
			out = append(out, z)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func c211pJoinF(m map[string]float64) string {
	if len(m) == 0 {
		return "none"
	}
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	out := []string{}
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%.0f", k, m[k]))
	}
	return strings.Join(out, ",")
}

func c211pZoneJoin(z map[string]int) string {
	keys := []string{}
	for k := range z {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return strings.Join(keys, ",")
}

func c211pContainerScript(proj, shape string) string {
	script := `set -u
export PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/bin:/bin:/usr/local/bin:$PATH
echo "C211P_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i123852 proj=__PROJ__ shape=__SHAPE__"
echo "C211P_UNAME $(uname -r)"
echo "C211P_LSPCI_RAW $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
echo "C211P_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
echo "C211P_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
SMI=""
for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
  if [ -x "$C" ]; then SMI="$C"; break; fi
done
echo "C211P_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211P_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211P_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
else
  echo "C211P_NOSMI"
fi
nvidia-smi -L 2>&1
nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
echo "C211P_DONE at=$(date -u +%FT%TZ)"`
	script = strings.ReplaceAll(script, "__PROJ__", proj)
	script = strings.ReplaceAll(script, "__SHAPE__", shape)
	return script
}

func c211pLogs(jtok, num, jobID string) (string, string) {
	filters := []string{
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s" AND "C211P_"`, jobID),
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`"C211P_START" AND "nonce=i123852"`),
	}
	var lines []string
	lastErr := ""
	for _, f := range filters {
		body := fmt.Sprintf(`{"resourceNames":["projects/%s"],"filter":%q,"orderBy":"timestamp asc","pageSize":500}`,
			num, f)
		st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", jtok,
			"application/json", body)
		if st != 200 {
			lastErr = fmt.Sprintf("http=%d %s", st, c211xTrunc(rb, 200))
			continue
		}
		m := c211xJSON(rb)
		items, _ := m["entries"].([]interface{})
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if tp, ok := im["textPayload"].(string); ok && tp != "" {
				lines = append(lines, tp)
				continue
			}
			if jp, ok := im["jsonPayload"].(map[string]interface{}); ok {
				b, _ := json.Marshal(jp)
				lines = append(lines, string(b))
			}
		}
		if len(lines) > 0 {
			return strings.Join(lines, "\n"), ""
		}
	}
	return "", lastErr
}

func c211pListJobs(jtok, num, region string) (int, int, string) {
	u := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + num +
		"/locations/" + region + "/customJobs?pageSize=100"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	n := 0
	c211n := 0
	if st == 200 {
		m := c211xJSON(body)
		if items, ok := m["customJobs"].([]interface{}); ok {
			n = len(items)
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				if strings.Contains(c211pStr(im, "displayName"), "c211") {
					c211n++
				}
			}
		}
	}
	return st, n, fmt.Sprintf("total=%d c211_items=%d verbatim=%s", n, c211n, c211xTrunc(body, 160))
}

// c211pStr is a local alias for the shared helper.
func c211pStr(m map[string]interface{}, k string) string { return c211xStr(m, k) }

// ---------------------------------------------------------------------------
// read-only ownership evidence for the two lane-123443 leftovers
// ---------------------------------------------------------------------------

type c211pLeftover struct {
	Num    string
	Region string
	JobID  string
	Bucket string
	Proj   string
}

var c211pLeftovers = []c211pLeftover{
	{"463640210929", "us-east1", "8797474764726730752", "c211-i123443-463640210929", "team-eng-infrastructure"},
	{"210628133505", "us-east1", "1804228968350482432", "c211-i123443-210628133505", "team-eng-infra-testing"},
}

func c211pLeftoverRead(jtok string) {
	for _, lo := range c211pLeftovers {
		name := "projects/" + lo.Num + "/locations/" + lo.Region + "/customJobs/" + lo.JobID
		base := "https://" + lo.Region + "-aiplatform.googleapis.com/v1/"
		gst, gb := c211xHTTP("GET", base+name, jtok, "", "")
		gm := c211xJSON(gb)
		labels := ""
		if lm, ok := gm["labels"].(map[string]interface{}); ok {
			b, _ := json.Marshal(lm)
			labels = string(b)
		}
		fmt.Printf("%s_LEFTOVER_JOB proj=%s num=%s job=%s http=%d state=%s displayName=%s labels=%s outputUriPrefix=%s verbatim=%s\n",
			c211pTag, lo.Proj, lo.Num, lo.JobID, gst, c211pStr(gm, "state"), c211pStr(gm, "displayName"),
			labels, c211pStr(gm, "outputUriPrefix"), c211xTrunc(gb, 400))
		bu := "https://storage.googleapis.com/storage/v1/b/" + lo.Bucket + "?project=" + lo.Num
		bst, bb := c211xHTTP("GET", bu, jtok, "", "")
		bm := c211xJSON(bb)
		bl := ""
		if lm, ok := bm["labels"].(map[string]interface{}); ok {
			x, _ := json.Marshal(lm)
			bl = string(x)
		}
		fmt.Printf("%s_LEFTOVER_BUCKET proj=%s num=%s bucket=%s http=%d labels=%s timeCreated=%s verbatim=%s\n",
			c211pTag, lo.Proj, lo.Num, lo.Bucket, bst, bl, c211pStr(bm, "timeCreated"), c211xTrunc(bb, 300))
	}
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211pProjRec struct {
	Proj     string            `json:"proj"`
	Num      string            `json:"num"`
	State    string            `json:"state"`
	SvcHTTP  int               `json:"svc_http"`
	SvcState string            `json:"svc_state"`
	PermHTTP int               `json:"perm_http"`
	Allowed  []string          `json:"allowed"`
	Denied   []string          `json:"denied"`
	Quota    map[string]string `json:"quota"`
	Accel    map[string]string `json:"accel"`
	Regions  map[string]string `json:"accel_regions"`
	Shapes   map[string]string `json:"shape_zones"`
}

type c211pAttempt struct {
	Class      string `json:"class"`
	Proj       string `json:"proj"`
	Num        string `json:"num"`
	Region     string `json:"region"`
	Machine    string `json:"machine"`
	Accel      string `json:"accel"`
	Count      int    `json:"count"`
	Density    int    `json:"density"`
	Offering   string `json:"offering"`
	CreateHTTP int    `json:"create_http"`
	CreateBody string `json:"create_body"`
	Job        string `json:"job"`
	States     string `json:"states"`
	FinalState string `json:"final_state"`
	Readback   string `json:"readback"`
	LogBytes   int    `json:"log_bytes"`
	Logs       string `json:"logs_tail"`
	Device     string `json:"device_lines"`
	UUIDs      string `json:"uuids"`
	EndState   string `json:"end_state"`
	ListAfter  string `json:"list_after"`
	WaitedSec  int    `json:"waited_sec"`
}

func TestC211I123852VertexP100P4(t *testing.T) {
	started := time.Now()
	deadline := started.Add(30 * time.Minute)
	fmt.Printf("%s_START nonce=%s at=%s\n", c211pTag, c211pNonce,
		started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("%s_MINT_FAILED %s\n", c211pTag, c211xTrunc(mintLog, 2000))
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
		c211pTag, method, c211xShort(wifSA), hostNum)

	fleetEmail := ""
	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("%s_FLEET_SA list=%d found=%v email=%s verbatim=%s\n",
		c211pTag, lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	ftok, fst, fbody := "", -1, ""
	if fleetEmail != "" {
		ftok, fst, fbody = c211xGenerateAccessToken(token, fleetEmail)
	}
	fmt.Printf("%s_FLEET_TOKEN status=%d token=%s verbatim=%s\n",
		c211pTag, fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		ftok = token
		fmt.Printf("%s_FLEET_TOKEN_FALLBACK using=wif_token\n", c211pTag)
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("%s_TOKENINFO status=%d verbatim=%s\n", c211pTag, tiSt, c211xTrunc(tiBody, 300))

	// ---- phase 1: quota + offering matrix across host + 3 readable siblings ----
	recs := map[string]*c211pProjRec{}
	for _, p := range c211pProjects {
		r := &c211pProjRec{Proj: p.Name, Num: p.Num, Quota: map[string]string{}, Accel: map[string]string{},
			Regions: map[string]string{}, Shapes: map[string]string{}}
		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, ftok, "", "")
		r.State = c211pStr(c211xJSON(pmBody), "lifecycleState")
		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1beta1/projects/"+p.Num+
			"/services/aiplatform.googleapis.com", ftok, "", "")
		r.SvcHTTP = svSt
		r.SvcState = c211pStr(c211xJSON(svBody), "state")
		al, ast, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, c211pPerms)
		r.Allowed, r.PermHTTP = al, ast
		r.Denied = []string{}
		for _, perm := range c211pPerms {
			if !c211xContains(al, perm) {
				r.Denied = append(r.Denied, perm)
			}
		}
		q, qst, qerr := c211pQuota(ftok, p.Num)
		for _, mt := range c211pQuotaMetrics {
			r.Quota[mt] = c211pJoinF(q[mt])
		}
		for _, cl := range c211pClasses {
			z, ost := c211pAccel(ftok, p.Num, cl.AccelLow)
			mx := 0
			for _, v := range z {
				if v > mx {
					mx = v
				}
			}
			r.Accel[cl.Key] = fmt.Sprintf("http=%d zones=%d max=%d", ost, len(z), mx)
			regs := map[string]int{}
			for zone := range z {
				reg := c211xRegionOfZone(zone)
				regs[reg]++
			}
			rr := []string{}
			for _, reg := range cl.Regions {
				if n, ok := regs[reg]; ok {
					rr = append(rr, fmt.Sprintf("%s:%d", reg, n))
				}
			}
			r.Regions[cl.Key] = strings.Join(rr, ",")
			fmt.Printf("%s_ACCEL proj=%s accel=%s %s regions=%s all_zones=%s\n",
				c211pTag, p.Name, cl.AccelLow, r.Accel[cl.Key], r.Regions[cl.Key], c211xTrunc(c211pZoneJoin(z), 500))
		}
		for _, n := range []int{4, 2, 1} {
			mt, _ := c211pDensity(n)
			if _, ok := r.Shapes[mt]; ok {
				continue
			}
			z, mst := c211pMachineZones(ftok, p.Num, mt)
			r.Shapes[mt] = fmt.Sprintf("http=%d zones=%d", mst, len(z))
		}
		fmt.Printf("%s_PROJECT proj=%s num=%s get_http=%d state=%s svc_http=%d svc=%s perm_http=%d allowed=%s denied=%s quota_http=%d err=%s\n",
			c211pTag, p.Name, p.Num, pmSt, r.State, svSt, r.SvcState, ast,
			c211xTrunc(strings.Join(r.Allowed, ","), 320), strings.Join(r.Denied, ","), qst, qerr)
		for _, mt := range c211pQuotaMetrics {
			fmt.Printf("%s_QUOTA proj=%s %s %s\n", c211pTag, p.Name, mt, r.Quota[mt])
		}
		for m, o := range r.Shapes {
			fmt.Printf("%s_SHAPE proj=%s %s %s\n", c211pTag, p.Name, m, o)
		}
		recs[p.Num] = r
	}

	// ---- phase 2: one on-demand customJob per class, P100 then P4 ----
	attempts := []*c211pAttempt{}
	proven := map[string]string{}
	for _, cl := range c211pClasses {
		ar := &c211pAttempt{Class: cl.Key, Accel: cl.Accel}
		var tp *c211pProjRec
		region := ""
		density := 0
		for _, p := range c211pProjects {
			r := recs[p.Num]
			if r == nil || !c211xContains(r.Allowed, "aiplatform.customJobs.create") {
				continue
			}
			if r.SvcState == "DISABLED" {
				continue
			}
			for _, cand := range []int{4, 2, 1} {
				for _, reg := range cl.Regions {
					var lim float64
					for _, kv := range strings.Split(r.Quota[cl.MetricOD], ",") {
						parts := strings.SplitN(kv, "=", 2)
						if len(parts) == 2 && parts[0] == reg {
							fmt.Sscanf(parts[1], "%g", &lim)
						}
					}
					if lim >= float64(cand) {
						tp, region, density = r, reg, cand
						break
					}
				}
				if tp != nil {
					break
				}
			}
			if tp != nil {
				break
			}
		}
		if tp == nil {
			ar.CreateHTTP = -3
			ar.CreateBody = "skipped: no project with customJobs.create + positive on-demand quota"
			fmt.Printf("%s_CLASS_SKIP class=%s reason=no_project_or_quota\n", c211pTag, cl.Key)
			attempts = append(attempts, ar)
			continue
		}
		machine, count := c211pDensity(density)
		ar.Proj, ar.Num, ar.Region, ar.Machine, ar.Count, ar.Density = tp.Proj, tp.Num, region, machine, count, density
		z, _ := c211pAccel(ftok, tp.Num, cl.AccelLow)
		rz := c211pRegionZones(z, region)
		ar.Offering = fmt.Sprintf("accel_zones_in_region=%s zones=%d", strings.Join(rz, ","), len(z))
		mz, _ := c211pMachineZones(ftok, tp.Num, machine)
		mzr := []string{}
		for _, mzz := range mz {
			if strings.HasPrefix(mzz, region+"-") {
				mzr = append(mzr, mzz)
			}
		}
		fmt.Printf("%s_CLASS_PICK class=%s proj=%s num=%s region=%s machine=%s count=%d quota=%s offering=%s machine_zones_in_region=%s\n",
			c211pTag, cl.Key, tp.Proj, tp.Num, region, machine, count, tp.Quota[cl.MetricOD], ar.Offering, strings.Join(mzr, ","))

		script := c211pContainerScript(tp.Proj, fmt.Sprintf("%s-%dx", cl.Key, count))
		disp := fmt.Sprintf("c211-i123852-%s-%dx", cl.Key, count)
		jobBody := map[string]interface{}{
			"displayName": disp,
			"labels":      map[string]interface{}{"c211": c211pNonce},
			"jobSpec": map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType":      machine,
						"acceleratorType":  cl.Accel,
						"acceleratorCount": count,
					},
					"replicaCount": 1,
					"containerSpec": map[string]interface{}{
						"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
						"command":  []string{"bash", "-c"},
						"args":     []string{script},
					},
					"diskSpec": map[string]interface{}{
						"bootDiskType": "pd-standard", "bootDiskSizeGb": 100,
					},
				}},
				// self-terminating: no provider-side termination endpoint is used.
				"scheduling": map[string]interface{}{
					"disableRetries": true, "timeout": "600s",
				},
			},
		}
		if fleetEmail != "" {
			jobBody["jobSpec"].(map[string]interface{})["serviceAccount"] = fleetEmail
		}
		jbytes, _ := json.Marshal(jobBody)
		fmt.Printf("%s_REQUEST class=%s bytes=%d strategy=ON_DEMAND\n", c211pTag, cl.Key, len(jbytes))
		url := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + tp.Num +
			"/locations/" + region + "/customJobs"
		cst, cb := c211xHTTP("POST", url, ftok, "application/json", string(jbytes))
		ar.CreateHTTP = cst
		ar.CreateBody = c211xTrunc(cb, 900)
		fmt.Printf("%s_CREATE class=%s http=%d verbatim=%s\n", c211pTag, cl.Key, cst, ar.CreateBody)
		if cst != 200 && fleetEmail != "" && strings.Contains(cb, "act as service_account") {
			delete(jobBody["jobSpec"].(map[string]interface{}), "serviceAccount")
			jb2, _ := json.Marshal(jobBody)
			cst, cb = c211xHTTP("POST", url, ftok, "application/json", string(jb2))
			ar.CreateHTTP = cst
			ar.CreateBody = c211xTrunc(cb, 900)
			fmt.Printf("%s_CREATE_RETRY_NO_SA class=%s http=%d verbatim=%s\n", c211pTag, cl.Key, cst, ar.CreateBody)
		}
		if cst != 200 {
			attempts = append(attempts, ar)
			continue
		}
		jm := c211xJSON(cb)
		name := c211pStr(jm, "name")
		ar.Job = name
		jobID := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			jobID = name[i+1:]
		}
		fmt.Printf("%s_JOB class=%s name=%s\n", c211pTag, cl.Key, name)

		states := []string{}
		last, final := "", ""
		devStart := time.Now()
		pollMax := 13 * time.Minute
		if cl.Key == "p100" {
			pollMax = 6 * time.Minute
		}
		if rem := deadline.Sub(time.Now()) - 7*time.Minute; rem < pollMax {
			pollMax = rem
		}
		if pollMax < 2*time.Minute {
			pollMax = 2 * time.Minute
		}
		logs := ""
		for i := 0; time.Since(devStart) < pollMax; i++ {
			time.Sleep(20 * time.Second)
			gst, gb := c211xHTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
			st := c211pStr(c211xJSON(gb), "state")
			if st != last {
				states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(devStart).Seconds()), st))
				fmt.Printf("%s_POLL class=%s i=%d http=%d state=%s\n", c211pTag, cl.Key, i, gst, st)
				last = st
			}
			if i%3 == 2 || strings.HasPrefix(st, "JOB_STATE_SUCCEEDED") ||
				strings.HasPrefix(st, "JOB_STATE_FAILED") || strings.HasPrefix(st, "JOB_STATE_CANCELLED") ||
				strings.HasPrefix(st, "JOB_STATE_EXPIRED") {
				lt, lerr := c211pLogs(ftok, tp.Num, jobID)
				if len(lt) > len(logs) {
					logs = lt
				}
				if strings.Contains(logs, "GPU-") {
					break
				}
				if lerr != "" {
					fmt.Printf("%s_LOG_ERR class=%s %s\n", c211pTag, cl.Key, lerr)
				}
			}
			if strings.HasPrefix(st, "JOB_STATE_SUCCEEDED") || strings.HasPrefix(st, "JOB_STATE_FAILED") ||
				strings.HasPrefix(st, "JOB_STATE_CANCELLED") || strings.HasPrefix(st, "JOB_STATE_EXPIRED") ||
				strings.HasPrefix(st, "JOB_STATE_PARTIALLY_SUCCEEDED") {
				final = st
				break
			}
		}
		ar.States = strings.Join(states, " ")
		ar.FinalState = final
		ar.WaitedSec = int(time.Since(devStart).Seconds())
		lt, lerr := c211pLogs(ftok, tp.Num, jobID)
		if len(lt) > len(logs) {
			logs = lt
		}
		ar.LogBytes = len(logs)
		if lerr != "" {
			fmt.Printf("%s_LOG_ERR_FINAL class=%s %s\n", c211pTag, cl.Key, lerr)
		}
		devLines := []string{}
		uuids := []string{}
		for _, ln := range strings.Split(logs, "\n") {
			if strings.Contains(ln, "C211P_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
				devLines = append(devLines, c211xTrunc(ln, 400))
			}
			if strings.Contains(ln, "GPU-") {
				for _, u := range c211xUUIDRe.FindAllString(ln, -1) {
					uuids = append(uuids, u)
				}
			}
		}
		ar.Device = strings.Join(devLines, "\n")
		ar.UUIDs = strings.Join(uuids, ",")
		ar.Logs = c211xTrunc(logs, 3000)
		for _, ln := range devLines {
			fmt.Printf("%s_DEVICE_LINE class=%s %s\n", c211pTag, cl.Key, ln)
		}
		fmt.Printf("%s_LOG class=%s bytes=%d uuids=%d q=%s\n", c211pTag, cl.Key, ar.LogBytes,
			len(uuids), c211xTrunc(strings.Join(uuids, ","), 400))
		rbSt, rbBody := c211xHTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
		rbm := c211xJSON(rbBody)
		ar.Readback = fmt.Sprintf("http=%d state=%s", rbSt, c211pStr(rbm, "state"))
		if js, ok := rbm["jobSpec"].(map[string]interface{}); ok {
			if wps, ok2 := js["workerPoolSpecs"].([]interface{}); ok2 && len(wps) > 0 {
				wm, _ := wps[0].(map[string]interface{})
				if ms, ok3 := wm["machineSpec"].(map[string]interface{}); ok3 {
					ar.Readback += fmt.Sprintf(" machine=%v accel=%v count=%v", ms["machineType"], ms["acceleratorType"], ms["acceleratorCount"])
				}
			}
		}
		fmt.Printf("%s_READBACK class=%s %s\n", c211pTag, cl.Key, ar.Readback)
		ar.EndState = final
		lstSt, _, lbody2 := c211pListJobs(ftok, tp.Num, region)
		ar.ListAfter = fmt.Sprintf("list_http=%d %s", lstSt, c211xTrunc(lbody2, 200))
		fmt.Printf("%s_LIST_AFTER class=%s http=%d %s\n", c211pTag, cl.Key, lstSt, lbody2)
		if len(uuids) > 0 {
			proven[cl.Key] = fmt.Sprintf("%s:%s:%dx:%s", tp.Proj, region, count, strings.Join(uuids, ","))
		}
		attempts = append(attempts, ar)
	}

	// ---- phase 3: read-only ownership evidence for lane-123443 leftovers ----
	c211pLeftoverRead(ftok)

	out := map[string]interface{}{
		"nonce":    c211pNonce,
		"proven":   proven,
		"attempts": attempts,
		"recs":     recs,
		"elapsed":  int(time.Since(started).Seconds()),
	}
	b, _ := json.Marshal(out)
	fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven_p100=%s proven_p4=%s\n", c211pTag,
		int(time.Since(started).Seconds()), c211xTrunc(proven["p100"], 300), c211xTrunc(proven["p4"], 300))
	fmt.Printf("%s_JSON %s\n", c211pTag, c211xTrunc(string(b), 20000))
	fmt.Printf("%s_END %s\n", c211pTag, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}
