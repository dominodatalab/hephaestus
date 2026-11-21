//go:build functional && gke

// C211 i124560 — host project 153711203585 preemptible (SPOT) 8x large-class
// Vertex custom-training cell, re-opened with the PROVEN independent
// metadata-token evidence sink.
//
// Fact 124366 proved every SPOT 8x large-class customJob was created HTTP 200
// (preemptible quotas b200=64 us-central1, h200=64 in eight regions, h100=64 in
// four) yet produced zero device lines ONLY because the in-job sink was broken
// (storage.objects.create denied / Cloud Logging log_bytes=0).  Fact 124314
// proved the replacement sink: bucket IAM for the compute SA + cloudservices SA
// + aiplatform service agent + fleet SA, and an in-container metadata-token GCS
// upload that returns http=200 even when Cloud Logging reads 0.
//
// This probe runs ONE preemptible 8x customJob at a time, ladder order:
//   H200 a3-ultragpu-8g x8 in the four untried quota regions
//        (asia-south1, asia-south2, europe-west1, us-south1)
//   H200 a3-ultragpu-8g x8 in europe-west4 (the one that reached RUNNING)
//   H100 a3-highgpu-8g x8 in europe-west4/us-central1/us-east4/us-west1
//   B200 a4-highgpu-8g x8 in us-central1
// with diskSpec.bootDiskType=hyperdisk-balanced (the x123921/x123957 fix),
// jobSpec.scheduling.timeout self-termination, the proven metadata-token GCS +
// direct-logging sink, independent customJobs.get readback, delete and
// GET 404 / empty-list absence proof.  Bounded secondary: retire the three
// residual cancelled-but-PENDING x124013 us-central1 customJobs.
//
// Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const c211sNonce = "i124560"
const c211sTag = "C211L"

type c211sProj struct {
	Name string
	Num  string
}

// host project first (mandate), then the four Domino siblings whose aiplatform
// quota matrix is identical (fact 123712).  210628133505 is measured read-only
// only (Service Usage API disabled there).
var c211sProjects = []c211sProj{
	{"domino-eng-platform-dev", "153711203585"},
}

type c211sShape struct {
	Label   string
	Machine string
	Accel   string
	Count   int
	Region  string
	Metric  string
	Poll    int
}

var c211sLadder = []c211sShape{
	{"h200-8x-ass1", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "asia-south1",
		"custom_model_training_preemptible_nvidia_h200_gpus", 300},
	{"h200-8x-ass2", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "asia-south2",
		"custom_model_training_preemptible_nvidia_h200_gpus", 300},
	{"h200-8x-euw1", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "europe-west1",
		"custom_model_training_preemptible_nvidia_h200_gpus", 300},
	{"h200-8x-uss1", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "us-south1",
		"custom_model_training_preemptible_nvidia_h200_gpus", 300},
	{"h200-8x-euw4", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "europe-west4",
		"custom_model_training_preemptible_nvidia_h200_gpus", 180},
	{"h100-8x-euw4", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "europe-west4",
		"custom_model_training_preemptible_nvidia_h100_gpus", 120},
	{"h100-8x", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "us-central1",
		"custom_model_training_preemptible_nvidia_h100_gpus", 120},
	{"h100-8x-use4", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "us-east4",
		"custom_model_training_preemptible_nvidia_h100_gpus", 120},
	{"h100-8x-usw1", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "us-west1",
		"custom_model_training_preemptible_nvidia_h100_gpus", 120},
	{"b200-8x", "a4-highgpu-8g", "NVIDIA_B200", 8, "us-central1",
		"custom_model_training_preemptible_nvidia_b200_gpus", 120},
}

var c211sPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"serviceusage.services.enable", "storage.buckets.create",
	"storage.buckets.delete", "storage.objects.create",
	"iam.serviceAccounts.actAs",
}

var c211sQuotaMetrics = []string{
	"custom_model_training_nvidia_b200_gpus",
	"custom_model_training_nvidia_h200_gpus",
	"custom_model_training_nvidia_h100_gpus",
	"custom_model_training_nvidia_h100_mega_gpus",
	"custom_model_training_nvidia_a100_80gb_gpus",
	"custom_model_training_nvidia_a100_gpus",
	"custom_model_training_preemptible_nvidia_b200_gpus",
	"custom_model_training_preemptible_nvidia_h200_gpus",
	"custom_model_training_preemptible_nvidia_h100_gpus",
	"custom_model_training_preemptible_nvidia_h100_mega_gpus",
	"custom_model_training_preemptible_nvidia_a100_80gb_gpus",
	"custom_model_training_preemptible_nvidia_a100_gpus",
}

// ---------------------------------------------------------------------------
// local helpers (c211s* to avoid collisions with the c211x* helper file)
// ---------------------------------------------------------------------------

func c211sStr(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	return c211xStr(m, k)
}

func c211sQuota(jtok, num string) (map[string]map[string]float64, int, string) {
	out := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	if st != 200 {
		return out, st, c211xTrunc(body, 400)
	}
	m := c211xJSON(body)
	items, _ := m["metrics"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		id := ""
		if s := c211sStr(im, "metric"); s != "" {
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
					row[region] = f
				}
			}
		}
		if len(row) > 0 {
			out[id] = row
		}
	}
	return out, st, ""
}

func c211sOffering(jtok, num, machine string) ([]string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num +
		"/aggregated/machineTypes?filter=name%3D" + machine
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
			if c211sStr(xm, "name") == machine {
				parts := strings.Split(scope, "/")
				zones = append(zones, parts[len(parts)-1])
			}
		}
	}
	return zones, st
}

func c211sClass(accel string) string {
	switch {
	case strings.Contains(accel, "H200"):
		return "h200"
	case strings.Contains(accel, "H100"):
		return "h100"
	case strings.Contains(accel, "B200"):
		return "b200"
	}
	return accel
}

func c211sJoinF(m map[string]float64) string {
	if len(m) == 0 {
		return "none"
	}
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	// stable order
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

func c211sBucketName(num string) string { return "c211-i124560-" + num }

func c211sBucketCreate(jtok, num, region string) (int, string) {
	body := fmt.Sprintf(`{"name":"%s","location":"%s","storageClass":"STANDARD",`+
		`"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`, c211sBucketName(num), region)
	st, rb := c211xHTTP("POST", "https://storage.googleapis.com/storage/v1/b?project="+num,
		jtok, "application/json", body)
	return st, c211xTrunc(rb, 300)
}

func c211sBucketIam(jtok, num, fleetEmail string) (int, string) {
	body := fmt.Sprintf(`{"bindings":[{"role":"roles/storage.objectAdmin","members":[`+
		`"serviceAccount:%s-compute@developer.gserviceaccount.com",`+
		`"serviceAccount:%s@cloudservices.gserviceaccount.com",`+
		`"serviceAccount:service-%s@gcp-sa-aiplatform.iam.gserviceaccount.com",`+
		`"serviceAccount:%s"]}]}`, num, num, num, fleetEmail)
	st, rb := c211xHTTP("PUT", "https://storage.googleapis.com/storage/v1/b/"+
		c211sBucketName(num)+"/iam?project="+num, jtok, "application/json", body)
	return st, c211xTrunc(rb, 300)
}

func c211sObjectRead(jtok, num, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + c211sBucketName(num) +
		"/o/" + strings.ReplaceAll(obj, "/", "%2F") + "?alt=media"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	return st, body
}

func c211sBucketCleanup(jtok, num string) string {
	u := "https://storage.googleapis.com/storage/v1/b/" + c211sBucketName(num) + "/o?pageSize=100"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	objs := []string{}
	if st == 200 {
		m := c211xJSON(body)
		if items, ok := m["items"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				if n := c211sStr(im, "name"); n != "" {
					objs = append(objs, n)
				}
			}
		}
	}
	for _, o := range objs {
		du := "https://storage.googleapis.com/storage/v1/b/" + c211sBucketName(num) +
			"/o/" + strings.ReplaceAll(o, "/", "%2F")
		c211xHTTP("DELETE", du, jtok, "", "")
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+
		c211sBucketName(num)+"?project="+num, jtok, "", "")
	gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+
		c211sBucketName(num)+"?project="+num, jtok, "", "")
	return fmt.Sprintf("objects=%d delete_http=%d get_after=%d verbatim=%s", len(objs), dst, gst, c211xTrunc(db, 160))
}

func c211sContainerScript(proj, shape, bucket string) string {
	script := `set -u
export PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/bin:/bin:/usr/local/bin:$PATH
echo "C211L_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124560 proj=__PROJ__ shape=__SHAPE__"
echo "C211L_UNAME $(uname -r)"
echo "C211L_LSPCI_RAW $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
echo "C211L_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
echo "C211L_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
SMI=""
for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
  if [ -x "$C" ]; then SMI="$C"; break; fi
done
echo "C211L_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211L_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211L_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211L_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211L_NOSMI"
fi
nvidia-smi -L 2>&1
nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
{
  echo "C211L_PROOF_NONCE i124560"
  echo "C211L_PROOF_PROJ __PROJ__"
  echo "C211L_PROOF_SHAPE __SHAPE__"
  echo "C211L_PROOF_HOST $(hostname)"
  echo "C211L_PROOF_AT $(date -u +%FT%TZ)"
  nvidia-smi -L 2>&1
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
  lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller'
  cat /proc/driver/nvidia/version 2>&1
} > /tmp/c211s_proof.txt 2>&1
echo "C211L_PROOF_BEGIN"
cat /tmp/c211s_proof.txt
echo "C211L_PROOF_END"
if ! command -v curl >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl >/dev/null 2>&1) || true
  fi
fi
MTOK=""
if command -v curl >/dev/null 2>&1; then
  MTOK=$(curl -s -m 20 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | sed -e 's/.*"access_token" *: *"//' -e 's/".*//')
fi
if [ -n "$MTOK" ]; then
  US=$(curl -s -m 60 -o /tmp/c211s_upload_resp.txt -w "%{http_code}" -X POST --data-binary @/tmp/c211s_proof.txt -H "Authorization: Bearer $MTOK" -H "Content-Type: text/plain" "https://storage.googleapis.com/upload/storage/v1/b/__BUCKET__/o?uploadType=media&name=c211-i124560/__SHAPE__/proof.txt")
  echo "C211L_GCS_UPLOAD http=$US $(head -c 200 /tmp/c211s_upload_resp.txt 2>/dev/null | tr '\n' ' ')"
  US2=$(curl -s -m 60 -o /tmp/c211s_log_resp.txt -w "%{http_code}" -X POST -H "Authorization: Bearer $MTOK" -H "Content-Type: application/json" --data "{\"entries\":[{\"logName\":\"projects/__PROJ__/logs/c211-i124560\",\"resource\":{\"type\":\"global\"},\"textPayload\":\"$(tr '\n' '|' < /tmp/c211s_proof.txt)\"}]}" "https://logging.googleapis.com/v2/entries:write")
  echo "C211L_LOG_WRITE http=$US2 $(head -c 160 /tmp/c211s_log_resp.txt 2>/dev/null | tr '\n' ' ')"
else
  echo "C211L_GCS_UPLOAD no_metadata_token"
fi
echo "C211L_DONE at=$(date -u +%FT%TZ)"`
	script = strings.ReplaceAll(script, "__PROJ__", proj)
	script = strings.ReplaceAll(script, "__SHAPE__", shape)
	script = strings.ReplaceAll(script, "__BUCKET__", bucket)
	return script
}

func c211sLogList(jtok, num, filter string) (int, string) {
	body := fmt.Sprintf(`{"resourceNames":["projects/%s"],"filter":%q,"orderBy":"timestamp asc","pageSize":500}`,
		num, filter)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", jtok,
		"application/json", body)
	return st, rb
}

func c211sLogs(jtok, num, jobID string) (string, string) {
	filters := []string{
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s" AND "C211L_"`, jobID),
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`"C211L_PROOF_NONCE i124560"`),
	}
	var lines []string
	lastErr := ""
	for _, f := range filters {
		st, rb := c211sLogList(jtok, num, f)
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

func c211sRetire(jtok, region, name string, live bool) (int, int, string) {
	note := ""
	if live {
		cst, cb := c211xHTTP("POST", "https://"+region+"-aiplatform.googleapis.com/v1/"+name+":cancel",
			jtok, "application/json", `{}`)
		note += fmt.Sprintf("cancel_http=%d verbatim=%s; ", cst, c211xTrunc(cb, 200))
		time.Sleep(10 * time.Second)
	}
	dst, db := c211xHTTP("DELETE", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
	note += fmt.Sprintf("delete_http=%d verbatim=%s; ", dst, c211xTrunc(db, 200))
	if dst != 200 && dst != 404 {
		// a PENDING job is refused until cancellation propagates: one bounded retry
		time.Sleep(12 * time.Second)
		dst2, db2 := c211xHTTP("DELETE", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
		note += fmt.Sprintf("delete_retry_http=%d verbatim=%s; ", dst2, c211xTrunc(db2, 200))
		dst = dst2
	}
	getAfter := -1
	loops := 10
	if dst != 200 && dst != 404 {
		loops = 1
	}
	for i := 0; i < loops; i++ {
		gst, _ := c211xHTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(6 * time.Second)
	}
	return dst, getAfter, note
}

func c211sListJobs(jtok, num, region string) (int, int, string) {
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
				if strings.Contains(c211sStr(im, "displayName"), "c211") {
					c211n++
				}
			}
		}
	}
	return st, n, fmt.Sprintf("total=%d c211_items=%d verbatim=%s", n, c211n, c211xTrunc(body, 160))
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211sProjRec struct {
	Proj     string            `json:"proj"`
	Num      string            `json:"num"`
	State    string            `json:"state"`
	PermHTTP int               `json:"perm_http"`
	Allowed  []string          `json:"allowed"`
	Denied   []string          `json:"denied"`
	SvcState string            `json:"svc_state"`
	Quota    map[string]string `json:"quota"`
	Offer    map[string]string `json:"offer"`
}

func TestC211I124560Vertex8xSpot(t *testing.T) {
	started := time.Now()
	deadline := started.Add(42 * time.Minute)
	fmt.Printf("%s_START nonce=%s at=%s\n", c211sTag, c211sNonce,
		started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("%s_MINT_FAILED %s\n", c211sTag, c211xTrunc(mintLog, 2000))
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
		c211sTag, method, c211xShort(wifSA), hostNum)

	// fleet token (project-bound owner identity used by every proven probe)
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
		c211sTag, lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	ftok, fst, fbody := "", -1, ""
	if fleetEmail != "" {
		ftok, fst, fbody = c211xGenerateAccessToken(token, fleetEmail)
	}
	fmt.Printf("%s_FLEET_TOKEN status=%d token=%s verbatim=%s\n",
		c211sTag, fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		// fall back to the WIF token itself (it may already hold the roles)
		ftok = token
		fmt.Printf("%s_FLEET_TOKEN_FALLBACK using=wif_token\n", c211sTag)
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("%s_TOKENINFO status=%d verbatim=%s\n", c211sTag, tiSt, c211xTrunc(tiBody, 300))

	// phase 1 — per-project reach + quota + offering matrix (read-only)
	recs := map[string]*c211sProjRec{}
	order := []string{}
	target := ""
	for _, p := range c211sProjects {
		r := &c211sProjRec{Proj: p.Name, Num: p.Num, Quota: map[string]string{}, Offer: map[string]string{}}
		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, ftok, "", "")
		r.State = c211sStr(c211xJSON(pmBody), "lifecycleState")
		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1beta1/projects/"+p.Num+
			"/services/aiplatform.googleapis.com", ftok, "", "")
		r.SvcState = c211sStr(c211xJSON(svBody), "state")
		al, ast, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, c211sPerms)
		r.Allowed, r.PermHTTP = al, ast
		r.Denied = []string{}
		for _, perm := range c211sPerms {
			if !c211xContains(al, perm) {
				r.Denied = append(r.Denied, perm)
			}
		}
		q, qst, qerr := c211sQuota(ftok, p.Num)
		for _, mt := range c211sQuotaMetrics {
			r.Quota[mt] = c211sJoinF(q[mt])
		}
		for _, sh := range c211sLadder {
			z, ost := c211sOffering(ftok, p.Num, sh.Machine)
			r.Offer[sh.Machine] = fmt.Sprintf("http=%d zones=%s", ost, c211xTrunc(strings.Join(z, ","), 220))
		}
		fmt.Printf("%s_PROJECT proj=%s num=%s get_http=%d state=%s svc_http=%d svc=%s perm_http=%d allowed=%s denied=%s quota_http=%d err=%s\n",
			c211sTag, p.Name, p.Num, pmSt, r.State, svSt, r.SvcState, ast,
			c211xTrunc(strings.Join(r.Allowed, ","), 320), strings.Join(r.Denied, ","), qst, qerr)
		for _, mt := range c211sQuotaMetrics {
			if r.Quota[mt] != "none" {
				fmt.Printf("%s_QUOTA proj=%s %s %s\n", c211sTag, p.Name, mt, r.Quota[mt])
			}
		}
		for m, o := range r.Offer {
			fmt.Printf("%s_OFFER proj=%s %s %s\n", c211sTag, p.Name, m, o)
		}
		recs[p.Num] = r
		order = append(order, p.Num)
		if target == "" && c211xContains(al, "aiplatform.customJobs.create") {
			target = p.Num
		}
	}
	if target == "" {
		fmt.Printf("%s_VERDICT_NO_AI_PLATFORM_CREATE projects=%d\n", c211sTag, len(order))
		emit := map[string]interface{}{
			"nonce": c211sNonce, "target": "", "recs": recs,
			"verdict": "no_project_allows_aiplatform.customJobs.create",
			"elapsed": int(time.Since(started).Seconds()),
		}
		b, _ := json.Marshal(emit)
		fmt.Printf("%s_JSON %s\n", c211sTag, c211xTrunc(string(b), 12000))
		return
	}
	tp := recs[target]
	fmt.Printf("%s_TARGET proj=%s num=%s\n", c211sTag, tp.Proj, tp.Num)
	// bounded secondary — retire the three residual cancelled-but-PENDING
	// x124013 us-central1 customJobs (the only surviving c211 resources).
	for _, leftover := range []string{
		"projects/153711203585/locations/us-central1/customJobs/4154618287436595200",
		"projects/153711203585/locations/us-central1/customJobs/5991523985450336256",
		"projects/153711203585/locations/us-central1/customJobs/4863372278794027008",
	} {
		gst, gb := c211xHTTP("GET", "https://us-central1-aiplatform.googleapis.com/v1/"+leftover, ftok, "", "")
		lstate := c211sStr(c211xJSON(gb), "state")
		fmt.Printf("%s_RESIDUAL_GET job=%s http=%d state=%s verbatim=%s\n",
			c211sTag, leftover, gst, lstate, c211xTrunc(gb, 300))
		if gst == 200 {
			live := strings.HasPrefix(lstate, "JOB_STATE_PENDING") || strings.HasPrefix(lstate, "JOB_STATE_RUNNING") || strings.HasPrefix(lstate, "JOB_STATE_QUEUED")
			if live {
				cst, cb := c211xHTTP("POST", "https://us-central1-aiplatform.googleapis.com/v1/"+leftover+":cancel", ftok, "application/json", `{}`)
				fmt.Printf("%s_RESIDUAL_CANCEL job=%s http=%d verbatim=%s\n",
					c211sTag, leftover, cst, c211xTrunc(cb, 200))
				time.Sleep(15 * time.Second)
			}
			dst, getAfter, note := c211sRetire(ftok, "us-central1", leftover, false)
			fmt.Printf("%s_RESIDUAL_RETIRE job=%s delete_http=%d get_after=%d %s\n",
				c211sTag, leftover, dst, getAfter, note)
		}
	}
	if !c211xContains(tp.Allowed, "storage.buckets.create") {
		fmt.Printf("%s_NO_BUCKET_PERM continuing=logging_only\n", c211sTag)
	}

	// phase 2 — ONE SPOT job ladder, highest value first, one job alive at a time
	type attemptRec struct {
		Shape       string `json:"shape"`
		Machine     string `json:"machine"`
		Accel       string `json:"accel"`
		Count       int    `json:"count"`
		Region      string `json:"region"`
		CreateHTTP  int    `json:"create_http"`
		CreateBody  string `json:"create_body"`
		Job         string `json:"job"`
		States      string `json:"states"`
		FinalState  string `json:"final_state"`
		Readback    string `json:"readback"`
		LogBytes    int    `json:"log_bytes"`
		Logs        string `json:"logs_tail"`
		Device      string `json:"device_lines"`
		UUIDs       string `json:"uuids"`
		GCSHTTP     int    `json:"gcs_http"`
		GCSBytes    int    `json:"gcs_bytes"`
		GCSProof    string `json:"gcs_proof"`
		Retire      string `json:"retire"`
		Absence     string `json:"absence"`
		ListAfter   string `json:"list_after"`
		WaitedSec   int    `json:"waited_sec"`
	}
	attempts := []*attemptRec{}
	proven := ""
	provenClasses := map[string]string{}

	for _, sh := range c211sLadder {
		class := c211sClass(sh.Accel)
		if provenClasses[class] != "" {
			fmt.Printf("%s_LADDER_SKIP shape=%s class=%s reason=class-already-proven\n",
				c211sTag, sh.Label, class)
			continue
		}
		if time.Until(deadline) < 90*time.Second {
			fmt.Printf("%s_BUDGET_SKIP shape=%s remaining=%ds\n", c211sTag, sh.Label,
				int(time.Until(deadline).Seconds()))
			break
		}
		ar := &attemptRec{Shape: sh.Label, Machine: sh.Machine, Accel: sh.Accel,
			Count: sh.Count, Region: sh.Region}
		quotaRow := tp.Quota[sh.Metric]
		offRow := tp.Offer[sh.Machine]
		fmt.Printf("%s_LADDER shape=%s machine=%s accel=%s count=%d region=%s quota=%s offer=%s\n",
			c211sTag, sh.Label, sh.Machine, sh.Accel, sh.Count, sh.Region, quotaRow, c211xTrunc(offRow, 200))
		if quotaRow == "none" || strings.Contains(offRow, "zones=,") || strings.HasSuffix(offRow, "zones=") {
			ar.CreateHTTP = -2
			ar.CreateBody = "skipped: no measured preemptible quota or no offering in region"
			attempts = append(attempts, ar)
			continue
		}
		bucket := ""
		if c211xContains(tp.Allowed, "storage.buckets.create") {
			bst, bb := c211sBucketCreate(ftok, tp.Num, sh.Region)
			fmt.Printf("%s_BUCKET_CREATE name=%s http=%d verbatim=%s\n", c211sTag, c211sBucketName(tp.Num), bst, bb)
			if bst == 200 || bst == 409 {
				bucket = c211sBucketName(tp.Num)
				if fleetEmail != "" {
					ist, ib := c211sBucketIam(ftok, tp.Num, fleetEmail)
					fmt.Printf("%s_BUCKET_IAM http=%d verbatim=%s\n", c211sTag, ist, ib)
				}
			}
		}
		script := c211sContainerScript(tp.Proj, sh.Label, bucket)
		disp := fmt.Sprintf("c211-i124560-vtx-%s-spot", sh.Label)
		jobBody := map[string]interface{}{
			"displayName": disp,
			"jobSpec": map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType":      sh.Machine,
						"acceleratorType":  sh.Accel,
						"acceleratorCount": sh.Count,
					},
					"replicaCount": 1,
					"containerSpec": map[string]interface{}{
						"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
						"command":  []string{"bash", "-c"},
						"args":     []string{script},
					},
					"diskSpec": map[string]interface{}{
						"bootDiskType": "hyperdisk-balanced", "bootDiskSizeGb": 300,
					},
				}},
				"scheduling": map[string]interface{}{
					"strategy": "SPOT", "disableRetries": true, "timeout": "1200s",
				},
			},
		}
		if fleetEmail != "" {
			jobBody["jobSpec"].(map[string]interface{})["serviceAccount"] = fleetEmail
		}
		jbytes, _ := json.Marshal(jobBody)
		fmt.Printf("%s_REQUEST shape=%s bytes=%d strategy=SPOT\n", c211sTag, sh.Label, len(jbytes))
		url := "https://" + sh.Region + "-aiplatform.googleapis.com/v1/projects/" + tp.Num +
			"/locations/" + sh.Region + "/customJobs"
		cst, cb := c211xHTTP("POST", url, ftok, "application/json", string(jbytes))
		ar.CreateHTTP = cst
		ar.CreateBody = c211xTrunc(cb, 900)
		fmt.Printf("%s_CREATE shape=%s http=%d verbatim=%s\n", c211sTag, sh.Label, cst, ar.CreateBody)
		if cst != 200 {
			// one shape-name retry without serviceAccount if it is an actAs refusal
			if fleetEmail != "" && strings.Contains(cb, "act as service_account") {
				delete(jobBody["jobSpec"].(map[string]interface{}), "serviceAccount")
				jb2, _ := json.Marshal(jobBody)
				cst, cb = c211xHTTP("POST", url, ftok, "application/json", string(jb2))
				ar.CreateHTTP = cst
				ar.CreateBody = c211xTrunc(cb, 900)
				fmt.Printf("%s_CREATE_RETRY_NO_SA shape=%s http=%d verbatim=%s\n", c211sTag, sh.Label, cst, ar.CreateBody)
			}
			if bucket != "" {
				fmt.Printf("%s_BUCKET_CLEANUP %s\n", c211sTag, c211sBucketCleanup(ftok, tp.Num))
			}
			attempts = append(attempts, ar)
			continue
		}
		jm := c211xJSON(cb)
		name := c211sStr(jm, "name")
		ar.Job = name
		jobID := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			jobID = name[i+1:]
		}
		fmt.Printf("%s_JOB shape=%s name=%s\n", c211sTag, sh.Label, name)

		states := []string{}
		last := ""
		final := ""
		devStart := time.Now()
		pollMax := time.Duration(sh.Poll) * time.Second
		if rem := deadline.Sub(time.Now()) - 75*time.Second; rem < pollMax {
			pollMax = rem
		}
		if pollMax < 45*time.Second {
			pollMax = 45 * time.Second
		}
		fmt.Printf("%s_POLL_WINDOW shape=%s max=%ds\n", c211sTag, sh.Label, int(pollMax.Seconds()))
		for i := 0; time.Since(devStart) < pollMax; i++ {
			time.Sleep(20 * time.Second)
			gst, gb := c211xHTTP("GET", "https://"+sh.Region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
			st := c211sStr(c211xJSON(gb), "state")
			if st != last {
				states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(devStart).Seconds()), st))
				fmt.Printf("%s_POLL shape=%s i=%d http=%d state=%s\n", c211sTag, sh.Label, i, gst, st)
				last = st
			}
			if i%3 == 2 || strings.HasPrefix(st, "JOB_STATE_SUCCEEDED") ||
				strings.HasPrefix(st, "JOB_STATE_FAILED") || strings.HasPrefix(st, "JOB_STATE_CANCELLED") ||
				strings.HasPrefix(st, "JOB_STATE_EXPIRED") {
				lt, lerr := c211sLogs(ftok, tp.Num, jobID)
				if strings.Contains(lt, "GPU-") {
					ar.Logs = lt
					break
				}
				if lerr != "" {
					fmt.Printf("%s_LOG_ERR shape=%s %s\n", c211sTag, sh.Label, lerr)
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
		// final evidence collection
		lt, lerr := c211sLogs(ftok, tp.Num, jobID)
		if len(lt) > len(ar.Logs) {
			ar.Logs = lt
		}
		ar.LogBytes = len(ar.Logs)
		if lerr != "" {
			fmt.Printf("%s_LOG_ERR_FINAL shape=%s %s\n", c211sTag, sh.Label, lerr)
		}
		if bucket != "" {
			gst, gb := c211sObjectRead(ftok, tp.Num, "c211-i124560/"+sh.Label+"/proof.txt")
			ar.GCSHTTP = gst
			ar.GCSBytes = len(gb)
			ar.GCSProof = c211xTrunc(gb, 2000)
			fmt.Printf("%s_GCS_READ shape=%s http=%d body=%s\n", c211sTag, sh.Label, gst, c211xTrunc(gb, 1200))
			if ar.Logs == "" && gst == 200 {
				ar.Logs = gb
				ar.LogBytes = len(gb)
			}
		}
		devLines := []string{}
		uuids := []string{}
		for _, ln := range strings.Split(ar.Logs, "\n") {
			if strings.Contains(ln, "C211L_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
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
		ar.Logs = c211xTrunc(ar.Logs, 3000)
		for _, ln := range devLines {
			fmt.Printf("%s_DEVICE_LINE shape=%s %s\n", c211sTag, sh.Label, ln)
		}
		fmt.Printf("%s_LOG shape=%s bytes=%d uuids=%d q=%s\n", c211sTag, sh.Label,
			ar.LogBytes, len(uuids), c211xTrunc(strings.Join(uuids, ","), 400))
		rbSt, rbBody := c211xHTTP("GET", "https://"+sh.Region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
		rbm := c211xJSON(rbBody)
		ar.Readback = fmt.Sprintf("http=%d state=%s", rbSt, c211sStr(rbm, "state"))
		if wps, ok := rbm["jobSpec"].(map[string]interface{})["workerPoolSpecs"].([]interface{}); ok && len(wps) > 0 {
			wm, _ := wps[0].(map[string]interface{})
			if ms, ok2 := wm["machineSpec"].(map[string]interface{}); ok2 {
				ar.Readback += fmt.Sprintf(" machine=%v accel=%v count=%v", ms["machineType"], ms["acceleratorType"], ms["acceleratorCount"])
			}
			if sc, ok2 := rbm["jobSpec"].(map[string]interface{})["scheduling"].(map[string]interface{}); ok2 {
				ar.Readback += fmt.Sprintf(" strategy=%v", sc["strategy"])
			}
		}
		fmt.Printf("%s_READBACK shape=%s %s\n", c211sTag, sh.Label, ar.Readback)
		live := !strings.HasPrefix(final, "JOB_STATE_SUCCEEDED") && !strings.HasPrefix(final, "JOB_STATE_FAILED") &&
			!strings.HasPrefix(final, "JOB_STATE_CANCELLED") && !strings.HasPrefix(final, "JOB_STATE_EXPIRED")
		dst, getAfter, note := c211sRetire(ftok, sh.Region, name, live)
		ar.Retire = note
		fmt.Printf("%s_RETIRE shape=%s delete_http=%d get_after=%d %s\n", c211sTag, sh.Label, dst, getAfter, note)
		lstSt, _, lbody := c211sListJobs(ftok, tp.Num, sh.Region)
		ar.ListAfter = fmt.Sprintf("http=%d %s", lstSt, c211xTrunc(lbody, 200))
		ar.Absence = fmt.Sprintf("get_after=%d list=%s", getAfter, ar.ListAfter)
		fmt.Printf("%s_ABSENCE shape=%s get_after=%d list=%s\n", c211sTag, sh.Label, getAfter, ar.ListAfter)
		if bucket != "" {
			fmt.Printf("%s_BUCKET_CLEANUP shape=%s %s\n", c211sTag, sh.Label, c211sBucketCleanup(ftok, tp.Num))
		}
		if len(uuids) > 0 {
			provenClasses[class] = fmt.Sprintf("%s:%s:%d", tp.Proj, sh.Label, len(uuids))
			proven = strings.Join([]string{proven, fmt.Sprintf("%s:%s:%d", tp.Proj, sh.Label, len(uuids))}, ";")
		}
		attempts = append(attempts, ar)
	}

	out := map[string]interface{}{
		"nonce":      c211sNonce,
		"target":     target,
		"target_proj": tp.Proj,
		"proven":     proven,
		"attempts":   attempts,
		"elapsed":    int(time.Since(started).Seconds()),
	}
	b, _ := json.Marshal(out)
	fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven=%s\n", c211sTag,
		int(time.Since(started).Seconds()), c211xTrunc(proven, 400))
	fmt.Printf("%s_JSON %s\n", c211sTag, c211xTrunc(string(b), 16000))
	fmt.Printf("%s_END %s\n", c211sTag, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}
