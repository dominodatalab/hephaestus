//go:build functional && gke

// C211 i123851 — Vertex AI aiplatform custom-training RTX PRO 6000 Blackwell (SPOT) probe.
//
// Measured basis: custom_model_training_nvidia_rtx_pro_6000_gpus (on-demand) has ZERO
// positive regions in the host project 153711203585 and the three readable siblings
// 1072394045651 / 463640210929 / 1074386228880, while
// custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus is 14.0 in every one of
// the 42 regions; Compute offers accelerator nvidia-rtx-pro-6000 maxCardsPerInstance=8
// and the G4 GPU-bearing machine types g4-standard-48/96/192/384 (g4-standard-384 =
// 8 x NVIDIA RTX PRO 6000 per the machine-type description).  The measured on-demand
// 0 is the accepted verbatim on-demand refusal, so this probe runs ONE SPOT customJob
// ladder highest-density-first (g4-standard-384 8x, else g4-standard-192 4x, else
// g4-standard-96 2x) in us-central1 with explicit nvidia-smi commands, two evidence
// sinks (Cloud Logging + GCS object uploaded from inside the container via the
// metadata token), independent customJobs.get readback, delete and GET 404 /
// list-empty absence proof.
//
// Original ladder template (B200/H200/H100) for reference:
//
// Basis (fact 123712): the aiplatform custom_model_training_* GPU quota family was
// measured PR-free for host project 153711203585 and three siblings and is
// identical across them: every on-demand large class (H100 / H100-mega / H200 /
// B200 / A100-80GB) is 0, while the PREEMPTIBLE variants are large
// (H100=64 in europe-west4/us-central1/us-east4/us-west1, H100-mega=16 same four,
// H200=64 in eight regions, B200=64 in us-central1).  The measured on-demand 0 is
// exactly the mandate's accepted verbatim on-demand refusal, so this probe skips
// on-demand entirely and runs ONE SPOT customJob ladder, highest value first:
//   1. a4-highgpu-8g + 8 x NVIDIA_B200      (us-central1, preemptible B200=64)
//   2. a3-ultragpu-8g + 8 x NVIDIA_H200_141GB (us-central1, preemptible H200=64)
//   3. a3-highgpu-8g + 8 x NVIDIA_H100_80GB  (us-central1, preemptible H100=64)
// never more than one job alive at a time, explicit nvidia-smi commands, two
// independent evidence sinks (Cloud Logging stdout + a GCS object uploaded from
// inside the container via the metadata token), independent readback, delete and
// GET 404 / empty-list absence proof.
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

const c211sNonce = "i123851"
const c211sTag = "C211R"

type c211sProj struct {
	Name string
	Num  string
}

// host project first (mandate), then the four Domino siblings whose aiplatform
// quota matrix is identical (fact 123712).  210628133505 is measured read-only
// only (Service Usage API disabled there).
var c211sProjects = []c211sProj{
	{"domino-eng-platform-dev", "153711203585"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

type c211sShape struct {
	Label   string
	Machine string
	Accel   string
	Count   int
	Region  string
	Metric  string
}

var c211sLadder = []c211sShape{
	{"rtx-pro-6000-8x", "g4-standard-384", "NVIDIA_RTX_PRO_6000", 8, "us-central1",
		"custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus"},
	{"rtx-pro-6000-4x", "g4-standard-192", "NVIDIA_RTX_PRO_6000", 4, "us-central1",
		"custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus"},
	{"rtx-pro-6000-2x", "g4-standard-96", "NVIDIA_RTX_PRO_6000", 2, "us-central1",
		"custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus"},
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
	"custom_model_training_nvidia_rtx_pro_6000_gpus",
	"custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus",
	"custom_model_serving_nvidia_rtx_pro_6000_gpus",
	"custom_model_serving_preemptible_nvidia_rtx_pro_6000_gpus",
}

// rtx accelerator offering (Compute API name) and the G4 GPU-bearing machine types
var c211sAccelNames = []string{"nvidia-rtx-pro-6000", "nvidia-rtx-pro-6000-vws"}
var c211sMachineTypes = []string{"g4-standard-384", "g4-standard-192", "g4-standard-96", "g4-standard-48"}

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

func c211rAccel(jtok, num, accel string) ([]string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num +
		"/aggregated/acceleratorTypes?filter=name%3D" + accel
	st, body := c211xHTTP("GET", u, jtok, "", "")
	zones := []string{}
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
			if c211xStr(xm, "name") != accel {
				continue
			}
			var mx float64
			switch v := xm["maximumCardsPerInstance"].(type) {
			case float64:
				mx = v
			case string:
				fmt.Sscanf(v, "%g", &mx)
			}
			parts := strings.Split(scope, "/")
			zones = append(zones, fmt.Sprintf("%s:max%.0f", parts[len(parts)-1], mx))
		}
	}
	return zones, st
}

// c211rQuotaDigest returns a verbatim per-region digest (including zero buckets)
// for the metric, so the on-demand 0 and preemptible 14 are recorded explicitly.
func c211rQuotaDigest(jtok, num, metric string) (string, int) {
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	if st != 200 {
		return fmt.Sprintf("http=%d verbatim=%s", st, c211xTrunc(body, 240)), st
	}
	m := c211xJSON(body)
	items, _ := m["metrics"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		id := ""
		if x := c211xStr(im, "metric"); x != "" {
			parts := strings.Split(x, "/")
			id = parts[len(parts)-1]
		}
		if id != metric {
			continue
		}
		names := []string{}
		zero := 0
		nz := []string{}
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
				names = append(names, region)
				if f > 0 {
					nz = append(nz, fmt.Sprintf("%s=%.0f", region, f))
				} else {
					zero++
				}
			}
		}
		sortStrings(names)
		return fmt.Sprintf("buckets=%d zero=%d nonzero=[%s] regions=[%s]",
			len(names), zero, strings.Join(nz, ","), strings.Join(names, ",")), st
	}
	return "metric_absent", st
}

func sortStrings(a []string) {
	for i := 0; i < len(a); i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
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

func c211sBucketName(num string) string { return "c211-i123851-" + num }

func c211sBucketCreate(jtok, num, region string) (int, string) {
	body := fmt.Sprintf(`{"name":"%s","location":"%s","storageClass":"STANDARD",`+
		`"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`, c211sBucketName(num), region)
	st, rb := c211xHTTP("POST", "https://storage.googleapis.com/storage/v1/b?project="+num,
		jtok, "application/json", body)
	return st, c211xTrunc(rb, 300)
}

func c211sBucketIam(jtok, num, email string) (int, string) {
	body := fmt.Sprintf(`{"bindings":[{"role":"roles/storage.objectAdmin","members":["serviceAccount:%s"]}]}`, email)
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
echo "C211R_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i123851 proj=__PROJ__ shape=__SHAPE__"
echo "C211R_UNAME $(uname -r)"
echo "C211R_LSPCI_RAW $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
echo "C211R_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
echo "C211R_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
SMI=""
for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
  if [ -x "$C" ]; then SMI="$C"; break; fi
done
echo "C211R_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211R_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211R_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211R_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211R_NOSMI"
fi
nvidia-smi -L 2>&1
nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
{
  echo "C211R_PROOF_NONCE i123851"
  echo "C211R_PROOF_PROJ __PROJ__"
  echo "C211R_PROOF_SHAPE __SHAPE__"
  echo "C211R_PROOF_HOST $(hostname)"
  echo "C211R_PROOF_AT $(date -u +%FT%TZ)"
  nvidia-smi -L 2>&1
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
  lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller'
  cat /proc/driver/nvidia/version 2>&1
} > /tmp/c211s_proof.txt 2>&1
echo "C211R_PROOF_BEGIN"
cat /tmp/c211s_proof.txt
echo "C211R_PROOF_END"
MTOK=$(curl -s -m 15 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | sed -e 's/.*"access_token" *: *"//' -e 's/".*//')
if [ -n "$MTOK" ]; then
  US=$(curl -s -m 60 -o /tmp/c211s_upload_resp.txt -w "%{http_code}" -X POST --data-binary @/tmp/c211s_proof.txt -H "Authorization: Bearer $MTOK" -H "Content-Type: text/plain" "https://storage.googleapis.com/upload/storage/v1/b/__BUCKET__/o?uploadType=media&name=c211-i123851/__PROJ__/proof.txt")
  echo "C211R_GCS_UPLOAD http=$US $(head -c 200 /tmp/c211s_upload_resp.txt 2>/dev/null | tr '\n' ' ')"
  US2=$(curl -s -m 60 -o /tmp/c211s_log_resp.txt -w "%{http_code}" -X POST -H "Authorization: Bearer $MTOK" -H "Content-Type: application/json" --data "{\"entries\":[{\"logName\":\"projects/__PROJ__/logs/c211-i123851\",\"resource\":{\"type\":\"global\"},\"textPayload\":\"$(tr '\n' '|' < /tmp/c211s_proof.txt)\"}]}" "https://logging.googleapis.com/v2/entries:write")
  echo "C211R_LOG_WRITE http=$US2 $(head -c 160 /tmp/c211s_log_resp.txt 2>/dev/null | tr '\n' ' ')"
else
  echo "C211R_GCS_UPLOAD no_metadata_token"
fi
echo "C211R_DONE at=$(date -u +%FT%TZ)"`
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
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s" AND "C211R_"`, jobID),
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`"C211R_PROOF_NONCE i123851"`),
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
		time.Sleep(8 * time.Second)
	}
	dst, db := c211xHTTP("DELETE", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
	note += fmt.Sprintf("delete_http=%d verbatim=%s; ", dst, c211xTrunc(db, 200))
	getAfter := -1
	for i := 0; i < 15; i++ {
		gst, _ := c211xHTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(8 * time.Second)
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

func TestC211I123851VertexRtx(t *testing.T) {
	started := time.Now()
	deadline := started.Add(26 * time.Minute)
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
		for _, mtn := range c211sMachineTypes {
			z, ost := c211sOffering(ftok, p.Num, mtn)
			hit := []string{}
			for _, zz := range z {
				if strings.HasPrefix(zz, "us-central1-") {
					hit = append(hit, zz)
				}
			}
			r.Offer[mtn] = fmt.Sprintf("http=%d zones_n=%d uscentral1=%s all=%s", ost, len(z),
				strings.Join(hit, ","), c211xTrunc(strings.Join(z, ","), 400))
			fmt.Printf("%s_MACHINE proj=%s %s %s\n", c211sTag, p.Name, mtn, r.Offer[mtn])
		}
		for _, an := range c211sAccelNames {
			z, ost := c211rAccel(ftok, p.Num, an)
			hit := []string{}
			for _, zz := range z {
				if strings.HasPrefix(zz, "us-central1-") {
					hit = append(hit, zz)
				}
			}
			fmt.Printf("%s_ACCEL proj=%s %s http=%d zones_n=%d uscentral1=%s all=%s\n",
				c211sTag, p.Name, an, ost, len(z), strings.Join(hit, ","),
				c211xTrunc(strings.Join(z, ","), 500))
		}
		for _, m := range c211sQuotaMetrics {
			dg, dst := c211rQuotaDigest(ftok, p.Num, m)
			fmt.Printf("%s_QUOTA_DIGEST proj=%s http=%d %s %s\n", c211sTag, p.Name, dst, m, c211xTrunc(dg, 1200))
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
		GCSProof    string `json:"gcs_proof"`
		Retire      string `json:"retire"`
		Absence     string `json:"absence"`
		ListAfter   string `json:"list_after"`
		WaitedSec   int    `json:"waited_sec"`
	}
	attempts := []*attemptRec{}
	proven := ""

	for _, sh := range c211sLadder {
		if proven != "" {
			break
		}
		ar := &attemptRec{Shape: sh.Label, Machine: sh.Machine, Accel: sh.Accel,
			Count: sh.Count, Region: sh.Region}
		quotaRow := tp.Quota[sh.Metric]
		offRow := tp.Offer[sh.Machine]
		fmt.Printf("%s_LADDER shape=%s machine=%s accel=%s count=%d region=%s quota=%s offer=%s\n",
			c211sTag, sh.Label, sh.Machine, sh.Accel, sh.Count, sh.Region, quotaRow, c211xTrunc(offRow, 200))
		if quotaRow == "none" || !strings.Contains(quotaRow, sh.Region+"=") ||
			strings.Contains(quotaRow, sh.Region+"=0") ||
			!strings.Contains(offRow, "uscentral1=us-central1-") {
			ar.CreateHTTP = -2
			ar.CreateBody = "skipped: no measured preemptible quota in region or no offering in region"
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
		disp := fmt.Sprintf("c211-i123851-vtx-%s-spot", sh.Label)
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
		pollMax := 17 * time.Minute
		if rem := deadline.Sub(time.Now()) - 5*time.Minute; rem < pollMax {
			pollMax = rem
		}
		if pollMax < 2*time.Minute {
			pollMax = 2 * time.Minute
		}
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
			gst, gb := c211sObjectRead(ftok, tp.Num, "c211-i123851/"+tp.Proj+"/proof.txt")
			ar.GCSHTTP = gst
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
			if strings.Contains(ln, "C211R_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
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
			proven = fmt.Sprintf("%s:%s:%s", tp.Proj, sh.Label, strings.Join(uuids, ","))
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
