//go:build functional && gke

// C211 i124744 — Domino SIBLING projects: 8x large-class Vertex AI custom-training
// plane, ON-DEMAND refusal first then SPOT, with the PROVEN metadata-token
// evidence sink (i124118/i124272 recipe).
//
// Fact 123712 measured a byte-identical aiplatform large-class matrix in the four
// siblings 210628133505 / 1072394045651 / 463640210929 / 1074386228880:
// on-demand H100 / H100-mega / H200 / B200 / A100-80GB all 0 while
// PREEMPTIBLE H100=64 (europe-west4/us-central1/us-east4/us-west1),
// H100-mega=16 (same four), H200=64 (asia-south1/asia-south2/europe-west1/
// europe-west4/us-central1/us-east4/us-south1/us-west1) and B200=64 (us-central1).
// Fact 124188 proved the i124118 metadata-token GCS sink yields real device lines
// in these same siblings; fact 124650 proved the cross-project identity
// fleetcommand-service-account@domino-eng-platform-dev can act in all four.
//
// Per project, ladder order (highest value first):
//   a4-highgpu-8g + 8x NVIDIA_B200        us-central1
//   a3-ultragpu-8g + 8x NVIDIA_H200_141GB (quota regions)
//   a3-highgpu-8g + 8x NVIDIA_H100_80GB   (quota regions)
//   a3-megagpu-8g + 8x NVIDIA_H100_MEGA_80GB (quota regions)
// For the chosen class: EXACTLY ONE nonce ON-DEMAND 8x customJob (verbatim
// refusal expected, on-demand metric 0) and only on that refusal EXACTLY ONE
// nonce SPOT 8x customJob (hyperdisk-balanced boot disk, scheduling.strategy
// SPOT disableRetries timeout<=900s, metadata-token GCS + direct-logging sink),
// polled to terminal, both log and GCS proof read, independent customJobs.get
// acceleratorCount/state readback, then delete + GET 404 / list-empty, bucket
// cleanup. One customJob alive at a time. Host project 153711203585 untouched.
//
// Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const c211sNonce = "i124744"
const c211sTag = "C211X"
const c211sPrefix = "c211-i124744"

type c211sProj struct {
	Name string
	Num  string
}

// Sibling order per mandate: 210628133505 first (aiplatform customJobs.create
// already proven there in x123443), then 1072394045651, 463640210929, 1074386228880.
var c211sProjects = []c211sProj{
	{"team-eng-infra-testing", "210628133505"},
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
	Poll    int
}

// Ladder order per mandate. Region is resolved at runtime from the per-shape
// region preference list against the measured offering + preemptible quota.
var c211sLadder = []c211sShape{
	{"b200-8x", "a4-highgpu-8g", "NVIDIA_B200", 8, "",
		"custom_model_training_preemptible_nvidia_b200_gpus", 240},
	{"h200-8x", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "",
		"custom_model_training_preemptible_nvidia_h200_gpus", 240},
	{"h100-8x", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "",
		"custom_model_training_preemptible_nvidia_h100_gpus", 240},
	{"h100-mega-8x", "a3-megagpu-8g", "NVIDIA_H100_MEGA_80GB", 8, "",
		"custom_model_training_preemptible_nvidia_h100_mega_gpus", 240},
}

var c211sRegionPref = map[string][]string{
	"a4-highgpu-8g":   {"us-central1"},
	"a3-ultragpu-8g":  {"us-central1", "europe-west4", "europe-west1", "us-east4", "us-west1", "asia-south1", "asia-south2", "us-south1"},
	"a3-highgpu-8g":   {"us-central1", "europe-west4", "us-east4", "us-west1"},
	"a3-megagpu-8g":   {"us-central1", "europe-west4", "us-east4", "us-west1"},
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
// local helpers (c211s* to avoid collisions with the shared c211x* helpers)
// ---------------------------------------------------------------------------

func c211sStr(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	return c211xStr(m, k)
}

func c211sClass(accel string) string {
	switch {
	case strings.Contains(accel, "H200"):
		return "h200"
	case strings.Contains(accel, "H100_MEGA"):
		return "h100-mega"
	case strings.Contains(accel, "H100"):
		return "h100"
	case strings.Contains(accel, "B200"):
		return "b200"
	}
	return strings.ToLower(accel)
}

func c211sJoinF(m map[string]float64) string {
	if len(m) == 0 {
		return "none"
	}
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []string{}
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%.0f", k, m[k]))
	}
	return strings.Join(out, ",")
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

// offeringZones returns the zones where the machine type is offered.
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
	sort.Strings(zones)
	return zones, st
}

// accelOffer returns whether the accelerator type is offered in the zone and its
// maxCardsPerInstance (verbatim string).
func c211sAccelOffer(jtok, num, zone, accel string) (bool, string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + num + "/zones/" +
		zone + "/acceleratorTypes"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	if st != 200 {
		return false, "", st
	}
	m := c211xJSON(body)
	items, _ := m["items"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		if c211sStr(im, "name") == accel {
			mc := ""
			if v, ok := im["maxCardsPerInstance"]; ok {
				mc = fmt.Sprintf("%v", v)
			}
			return true, mc, st
		}
	}
	return false, "", st
}

func c211sZoneRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// ---------------------------------------------------------------------------
// GCS proof sink (best effort; a refusal is recorded verbatim)
// ---------------------------------------------------------------------------

func c211sBucketName(num string) string { return c211sPrefix + "-" + num }

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

func c211sObjectList(jtok, num string) ([]string, int) {
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
	sort.Strings(objs)
	return objs, st
}

func c211sBucketCleanup(jtok, num string) string {
	objs, lst := c211sObjectList(jtok, num)
	for _, o := range objs {
		du := "https://storage.googleapis.com/storage/v1/b/" + c211sBucketName(num) +
			"/o/" + strings.ReplaceAll(o, "/", "%2F")
		c211xHTTP("DELETE", du, jtok, "", "")
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+
		c211sBucketName(num)+"?project="+num, jtok, "", "")
	getAfter := -1
	for i := 0; i < 6; i++ {
		gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+
			c211sBucketName(num)+"?project="+num, jtok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("list_http=%d objects=%d delete_http=%d get_after=%d verbatim=%s",
		lst, len(objs), dst, getAfter, c211xTrunc(db, 160))
}

// ---------------------------------------------------------------------------
// container script
// ---------------------------------------------------------------------------

func c211sContainerScript(proj, shape, bucket string) string {
	script := `set -u
export PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/bin:/bin:/usr/local/bin:$PATH
echo "C211X_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124744 proj=__PROJ__ shape=__SHAPE__"
echo "C211X_UNAME $(uname -r)"
echo "C211X_LSPCI_RAW $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
echo "C211X_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
echo "C211X_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
SMI=""
for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
  if [ -x "$C" ]; then SMI="$C"; break; fi
done
echo "C211X_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211X_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211X_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211X_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211X_NOSMI"
fi
nvidia-smi -L 2>&1
nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller'
cat /proc/driver/nvidia/version 2>&1
{
  echo "C211X_PROOF_NONCE i124744"
  echo "C211X_PROOF_PROJ __PROJ__"
  echo "C211X_PROOF_SHAPE __SHAPE__"
  echo "C211X_PROOF_HOST $(hostname)"
  echo "C211X_PROOF_AT $(date -u +%FT%TZ)"
  nvidia-smi -L 2>&1
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
  lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller'
  cat /proc/driver/nvidia/version 2>&1
} > /tmp/c211s_proof.txt 2>&1
echo "C211X_PROOF_BEGIN"
cat /tmp/c211s_proof.txt
echo "C211X_PROOF_END"
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
  US=$(curl -s -m 60 -o /tmp/c211s_upload_resp.txt -w "%{http_code}" -X POST --data-binary @/tmp/c211s_proof.txt -H "Authorization: Bearer $MTOK" -H "Content-Type: text/plain" "https://storage.googleapis.com/upload/storage/v1/b/__BUCKET__/o?uploadType=media&name=c211-i124744/__SHAPE__/proof.txt")
  echo "C211X_GCS_UPLOAD http=$US $(head -c 200 /tmp/c211s_upload_resp.txt 2>/dev/null | tr '\n' ' ')"
  US2=$(curl -s -m 60 -o /tmp/c211s_log_resp.txt -w "%{http_code}" -X POST -H "Authorization: Bearer $MTOK" -H "Content-Type: application/json" --data "{\"entries\":[{\"logName\":\"projects/__PROJ__/logs/c211-i124744\",\"resource\":{\"type\":\"global\"},\"textPayload\":\"$(tr '\n' '|' < /tmp/c211s_proof.txt)\"}]}" "https://logging.googleapis.com/v2/entries:write")
  echo "C211X_LOG_WRITE http=$US2 $(head -c 160 /tmp/c211s_log_resp.txt 2>/dev/null | tr '\n' ' ')"
else
  echo "C211X_GCS_UPLOAD no_metadata_token"
fi
sleep 10
echo "C211X_DONE at=$(date -u +%FT%TZ)"`
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
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s" AND "C211X_"`, jobID),
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`"C211X_PROOF_NONCE i124744"`),
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
		time.Sleep(12 * time.Second)
	}
	dst, db := c211xHTTP("DELETE", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
	note += fmt.Sprintf("delete_http=%d verbatim=%s; ", dst, c211xTrunc(db, 200))
	if dst != 200 && dst != 404 {
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
	return st, n, fmt.Sprintf("total=%d c211_items=%d verbatim=%s", n, c211n, c211xTrunc(body, 200))
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
	Offer    map[string]string `json:"offer"`      // machine -> zones
	AccelMax map[string]string `json:"accel_max"`  // "machine@zone" -> maxCards/accel offer
	Bucket   string            `json:"bucket"`
	BucketCr string            `json:"bucket_create"`
	BucketIAM string           `json:"bucket_iam"`
	BucketCle string           `json:"bucket_cleanup"`
}

type c211sAttempt struct {
	Class       string   `json:"class"`
	Machine     string   `json:"machine"`
	Accel       string   `json:"accel"`
	Count       int      `json:"count"`
	Region      string   `json:"region"`
	Zone        string   `json:"zone"`
	AccelMax    string   `json:"accel_max_cards"`
	PreQuota    string   `json:"preemptible_quota"`
	OdQuota     string   `json:"ondemand_quota"`
	OdHTTP      int      `json:"ondemand_http"`
	OdBody      string   `json:"ondemand_body"`
	OdJob       string   `json:"ondemand_job"`
	OdStates    string   `json:"ondemand_states"`
	SpotHTTP    int      `json:"spot_http"`
	SpotBody    string   `json:"spot_body"`
	SpotJob     string   `json:"spot_job"`
	SpotStates  string   `json:"spot_states"`
	FinalState  string   `json:"final_state"`
	Readback    string   `json:"readback"`
	LogBytes    int      `json:"log_bytes"`
	LogTail     string   `json:"log_tail"`
	GCSList     string   `json:"gcs_list"`
	GCSBytes    int      `json:"gcs_bytes"`
	GCSProof    string   `json:"gcs_proof"`
	UUIDs       []string `json:"uuids"`
	DeviceLog   string   `json:"device_log"`
	DeviceGCS   string   `json:"device_gcs"`
	Retire      string   `json:"retire"`
	Absence     string   `json:"absence"`
	ListAfter   string   `json:"list_after"`
	WaitedSec   int      `json:"waited_sec"`
	RetireOK    bool     `json:"retire_ok"`
	Proven      bool     `json:"proven"`
	Notes       []string `json:"notes"`
}

func c211sRunJob(ftok, num, region, jobName, bucket, label string, pollMax time.Duration, deadline time.Time) (string, string, string, string, string, string, int) {
	// returns states, final, logText, gcsText, readback, retireNote, waitedSec
	jobID := jobName
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + jobName
	states := []string{}
	last := ""
	final := ""
	start := time.Now()
	if rem := time.Until(deadline) - 90*time.Second; rem < pollMax {
		pollMax = rem
	}
	if pollMax < 45*time.Second {
		pollMax = 45 * time.Second
	}
	fmt.Printf("%s_POLL_WINDOW shape=%s max=%ds\n", c211sTag, label, int(pollMax.Seconds()))
	for i := 0; time.Since(start) < pollMax; i++ {
		time.Sleep(20 * time.Second)
		gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
		st := c211sStr(c211xJSON(gb), "state")
		if st != last {
			states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(start).Seconds()), st))
			fmt.Printf("%s_POLL shape=%s i=%d http=%d state=%s\n", c211sTag, label, i, gst, st)
			last = st
		}
		if strings.HasPrefix(st, "JOB_STATE_SUCCEEDED") || strings.HasPrefix(st, "JOB_STATE_FAILED") ||
			strings.HasPrefix(st, "JOB_STATE_CANCELLED") || strings.HasPrefix(st, "JOB_STATE_EXPIRED") ||
			strings.HasPrefix(st, "JOB_STATE_PARTIALLY_SUCCEEDED") {
			final = st
			break
		}
	}
	logText, lerr := c211sLogs(ftok, num, jobID)
	if lerr != "" {
		fmt.Printf("%s_LOG_ERR shape=%s %s\n", c211sTag, label, lerr)
	}
	gcsText := ""
	gcsList := ""
	for k := 0; k < 6; k++ {
		if bucket != "" {
			objs, olst := c211sObjectList(ftok, num)
			parts := []string{}
			for _, o := range objs {
				if !strings.Contains(o, label) {
					continue
				}
				ost, obody := c211sObjectRead(ftok, num, o)
				parts = append(parts, fmt.Sprintf("%s http=%d bytes=%d", o, ost, len(obody)))
				if ost == 200 && len(obody) > 0 {
					gcsText += obody + "\n"
				}
			}
			gcsList = fmt.Sprintf("list_http=%d objects=%d [%s]", olst, len(objs), strings.Join(parts, " ; "))
		}
		if strings.Contains(logText+gcsText, "GPU-") {
			break
		}
		lt, _ := c211sLogs(ftok, num, jobID)
		if len(lt) > len(logText) {
			logText = lt
		}
		if k < 5 {
			time.Sleep(15 * time.Second)
		}
	}
	rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
	rbm := c211xJSON(rbBody)
	readback := fmt.Sprintf("http=%d state=%s", rbSt, c211sStr(rbm, "state"))
	if js, ok := rbm["jobSpec"].(map[string]interface{}); ok {
		if wps, ok2 := js["workerPoolSpecs"].([]interface{}); ok2 && len(wps) > 0 {
			wm, _ := wps[0].(map[string]interface{})
			if ms, ok3 := wm["machineSpec"].(map[string]interface{}); ok3 {
				readback += fmt.Sprintf(" machine=%v accel=%v count=%v", ms["machineType"], ms["acceleratorType"], ms["acceleratorCount"])
			}
		}
		if sc, ok2 := js["scheduling"].(map[string]interface{}); ok2 {
			readback += fmt.Sprintf(" strategy=%v timeout=%v", sc["strategy"], sc["timeout"])
		}
	}
	live := !strings.HasPrefix(final, "JOB_STATE_SUCCEEDED") && !strings.HasPrefix(final, "JOB_STATE_FAILED") &&
		!strings.HasPrefix(final, "JOB_STATE_CANCELLED") && !strings.HasPrefix(final, "JOB_STATE_EXPIRED")
	dst, getAfter, note := c211sRetire(ftok, region, jobName, live)
	lstSt, _, lbody := c211sListJobs(ftok, num, region)
	absence := fmt.Sprintf("get_after=%d list_http=%d %s", getAfter, lstSt, lbody)
	fmt.Printf("%s_RETIRE shape=%s delete_http=%d get_after=%d %s\n", c211sTag, label, dst, getAfter, note)
	fmt.Printf("%s_ABSENCE shape=%s get_after=%d list=%s\n", c211sTag, label, getAfter, c211xTrunc(lbody, 200))
	_ = gcsList
	return strings.Join(states, " "), final, logText, gcsText, readback, note + " || " + absence, int(time.Since(start).Seconds())
}

func c211sJSONStringStable(m map[string]interface{}) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestC211I124744Sibling8x(t *testing.T) {
	started := time.Now()
	deadline := started.Add(40 * time.Minute)
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
		c211sTag, lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 160))
	ftok, fst, fbody := "", -1, ""
	if fleetEmail != "" {
		ftok, fst, fbody = c211xGenerateAccessToken(token, fleetEmail)
	}
	fmt.Printf("%s_FLEET_TOKEN status=%d token=%s verbatim=%s\n",
		c211sTag, fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		ftok = token
		fmt.Printf("%s_FLEET_TOKEN_FALLBACK using=wif_token\n", c211sTag)
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("%s_TOKENINFO status=%d verbatim=%s\n", c211sTag, tiSt, c211xTrunc(tiBody, 300))

	recs := map[string]*c211sProjRec{}
	order := []string{}
	attempts := []*c211sAttempt{}
	provenAll := []string{}

	for _, p := range c211sProjects {
		r := &c211sProjRec{Proj: p.Name, Num: p.Num, Quota: map[string]string{},
			Offer: map[string]string{}, AccelMax: map[string]string{}}
		recs[p.Num] = r
		order = append(order, p.Num)
		if time.Until(deadline) < 7*time.Minute {
			fmt.Printf("%s_SKIP_PROJECT proj=%s reason=budget remaining=%ds\n", c211sTag, p.Name,
				int(time.Until(deadline).Seconds()))
			continue
		}
		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, ftok, "", "")
		r.State = c211sStr(c211xJSON(pmBody), "lifecycleState")
		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+p.Num+
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
			r.Offer[sh.Machine] = fmt.Sprintf("http=%d zones=%s", ost, strings.Join(z, ","))
		}
		fmt.Printf("%s_PROJECT proj=%s num=%s get_http=%d state=%s svc_http=%d svc=%s perm_http=%d allowed=%s denied=%s quota_http=%d err=%s\n",
			c211sTag, p.Name, p.Num, pmSt, r.State, svSt, r.SvcState, ast,
			c211xTrunc(strings.Join(r.Allowed, ","), 400), strings.Join(r.Denied, ","), qst, qerr)
		for _, mt := range c211sQuotaMetrics {
			if r.Quota[mt] != "none" {
				fmt.Printf("%s_QUOTA proj=%s %s %s\n", c211sTag, p.Name, mt, r.Quota[mt])
			}
		}
		for _, sh := range c211sLadder {
			fmt.Printf("%s_OFFER proj=%s machine=%s %s\n", c211sTag, p.Name, sh.Machine, r.Offer[sh.Machine])
		}
		if !c211xContains(al, "aiplatform.customJobs.create") {
			fmt.Printf("%s_NO_CREATE_PERM proj=%s\n", c211sTag, p.Name)
			continue
		}
		if r.SvcState != "" && r.SvcState != "ENABLED" {
			fmt.Printf("%s_SVC_NOT_ENABLED proj=%s state=%s\n", c211sTag, p.Name, r.SvcState)
			continue
		}

		// best-effort per-project GCS sink bucket
		if c211xContains(al, "storage.buckets.create") {
			bst, bb := c211sBucketCreate(ftok, p.Num, "us-central1")
			r.BucketCr = fmt.Sprintf("http=%d verbatim=%s", bst, bb)
			fmt.Printf("%s_BUCKET_CREATE proj=%s name=%s http=%d verbatim=%s\n",
				c211sTag, p.Name, c211sBucketName(p.Num), bst, bb)
			if bst == 200 || bst == 409 {
				r.Bucket = c211sBucketName(p.Num)
				if fleetEmail != "" {
					ist, ib := c211sBucketIam(ftok, p.Num, fleetEmail)
					r.BucketIAM = fmt.Sprintf("http=%d verbatim=%s", ist, ib)
					fmt.Printf("%s_BUCKET_IAM proj=%s http=%d verbatim=%s\n", c211sTag, p.Name, ist, ib)
				}
			}
		}

		attempted := map[string]bool{}
		retireFailed := false
		for _, sh := range c211sLadder {
			if time.Until(deadline) < 7*time.Minute || retireFailed {
				break
			}
			class := c211sClass(sh.Accel)
			if attempted[class] {
				continue
			}
			// resolve region+zone: first region in preference list with a zone
			// offering the machine and positive preemptible quota (when measured)
			chosenRegion, chosenZone := "", ""
			zones := []string{}
			if v := r.Offer[sh.Machine]; v != "" {
				if i := strings.Index(v, "zones="); i >= 0 {
					zs := v[i+len("zones="):]
					if zs != "" {
						zones = strings.Split(zs, ",")
					}
				}
			}
			preQ := q[sh.Metric]
			for _, reg := range c211sRegionPref[sh.Machine] {
				if len(preQ) > 0 {
					if preQ[reg] < float64(sh.Count) {
						continue
					}
				}
				for _, z := range zones {
					if c211sZoneRegion(z) == reg {
						chosenRegion, chosenZone = reg, z
						break
					}
				}
				if chosenRegion != "" {
					break
				}
			}
			if chosenRegion == "" {
				fmt.Printf("%s_LADDER_SKIP proj=%s shape=%s reason=no-region-with-offering-and-preemptible-quota offer=%s\n",
					c211sTag, p.Name, sh.Label, c211xTrunc(r.Offer[sh.Machine], 200))
				continue
			}
			offered, maxCards, ast2 := c211sAccelOffer(ftok, p.Num, chosenZone, sh.Accel)
			r.AccelMax[sh.Machine+"@"+chosenZone] = fmt.Sprintf("offered=%v maxCardsPerInstance=%s http=%d", offered, maxCards, ast2)
			fmt.Printf("%s_ACCEL_OFFER proj=%s zone=%s accel=%s offered=%v maxCards=%s http=%d\n",
				c211sTag, p.Name, chosenZone, sh.Accel, offered, maxCards, ast2)
			attempted[class] = true
			ar := &c211sAttempt{Class: class, Machine: sh.Machine, Accel: sh.Accel,
				Count: sh.Count, Region: chosenRegion, Zone: chosenZone,
				AccelMax: fmt.Sprintf("offered=%v maxCards=%s", offered, maxCards),
				PreQuota: c211sJoinF(preQ), OdQuota: r.Quota[strings.Replace(sh.Metric, "preemptible_", "", 1)]}
			attempts = append(attempts, ar)
			if !offered && ast2 == 200 {
				ar.Notes = append(ar.Notes, "accelerator type not offered in zone")
				fmt.Printf("%s_SHAPE_SKIP proj=%s shape=%s reason=accel-not-offered zone=%s\n",
					c211sTag, p.Name, sh.Label, chosenZone)
				continue
			}
			if maxCards != "" {
				var mc int
				fmt.Sscanf(maxCards, "%d", &mc)
				if mc > 0 && mc < sh.Count {
					ar.Notes = append(ar.Notes, fmt.Sprintf("maxCardsPerInstance=%d below %d", mc, sh.Count))
					fmt.Printf("%s_SHAPE_SKIP proj=%s shape=%s reason=maxcards=%s\n", c211sTag, p.Name, sh.Label, maxCards)
					continue
				}
			}

			mkBody := func(mode string) map[string]interface{} {
				script := c211sContainerScript(p.Name, sh.Label+"-"+mode, r.Bucket)
				disp := fmt.Sprintf("%s-%s-%s-%s", c211sPrefix, class, mode, p.Num)
				if len(disp) > 128 {
					disp = disp[:128]
				}
				jobSpec := map[string]interface{}{
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
				}
				if mode == "sp" {
					jobSpec["scheduling"] = map[string]interface{}{
						"strategy": "SPOT", "disableRetries": true, "timeout": "900s",
					}
				} else {
					jobSpec["scheduling"] = map[string]interface{}{"timeout": "900s"}
				}
				if fleetEmail != "" {
					jobSpec["serviceAccount"] = fleetEmail
				}
				return map[string]interface{}{
					"displayName": disp,
					"labels":      map[string]interface{}{"c211": c211sNonce},
					"jobSpec":     jobSpec,
				}
			}
			createURL := "https://" + chosenRegion + "-aiplatform.googleapis.com/v1/projects/" + p.Num +
				"/locations/" + chosenRegion + "/customJobs"
			postJob := func(mode string) (int, string, string) {
				body := mkBody(mode)
				b, _ := json.Marshal(body)
				fmt.Printf("%s_REQUEST proj=%s shape=%s mode=%s region=%s zone=%s bytes=%d\n",
					c211sTag, p.Name, sh.Label, mode, chosenRegion, chosenZone, len(b))
				cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(b))
				fmt.Printf("%s_CREATE proj=%s shape=%s mode=%s http=%d verbatim=%s\n",
					c211sTag, p.Name, sh.Label, mode, cst, c211xTrunc(cb, 800))
				if cst != 200 && fleetEmail != "" && strings.Contains(cb, "act as service_account") {
					delete(body["jobSpec"].(map[string]interface{}), "serviceAccount")
					b2, _ := json.Marshal(body)
					cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(b2))
					fmt.Printf("%s_CREATE_RETRY_NO_SA proj=%s shape=%s mode=%s http=%d verbatim=%s\n",
						c211sTag, p.Name, sh.Label, mode, cst, c211xTrunc(cb, 800))
				}
				job := ""
				if cst == 200 {
					job = c211sStr(c211xJSON(cb), "name")
				}
				return cst, cb, job
			}

			// (b) EXACTLY ONE on-demand 8x attempt -> verbatim refusal expected
			cst, cb, job := postJob("od")
			ar.OdHTTP = cst
			ar.OdBody = c211xTrunc(cb, 800)
			ar.OdJob = job
			liveJob := ""
			if cst == 200 && job != "" {
				fmt.Printf("%s_ONDEMAND_ACCEPTED proj=%s shape=%s job=%s\n", c211sTag, p.Name, sh.Label, job)
				liveJob = job
			} else {
				fmt.Printf("%s_ONDEMAND_REFUSED proj=%s shape=%s http=%d\n", c211sTag, p.Name, sh.Label, cst)
				// (c) only on the verbatim refusal: EXACTLY ONE SPOT variant
				sst, sb, sjob := postJob("sp")
				ar.SpotHTTP = sst
				ar.SpotBody = c211xTrunc(sb, 800)
				ar.SpotJob = sjob
				if sst == 200 && sjob != "" {
					liveJob = sjob
				}
			}
			if liveJob == "" {
				ar.FinalState = "NOT_CREATED"
				ar.RetireOK = true
				if r.Bucket != "" {
					// bucket kept for later attempts in this project
				}
				continue
			}
			mode := "sp"
			if ar.OdJob != "" {
				mode = "od"
			}
			states, final, logText, gcsText, readback, retireNote, waited :=
				c211sRunJob(ftok, p.Num, chosenRegion, liveJob, r.Bucket, sh.Label+"-"+mode, time.Duration(sh.Poll)*time.Second, deadline)
			if mode == "od" {
				ar.OdStates = states
			} else {
				ar.SpotStates = states
			}
			ar.FinalState = final
			ar.Readback = readback
			ar.Retire = retireNote
			ar.WaitedSec = waited
			ar.RetireOK = strings.Contains(retireNote, "get_after=404")
			// UUIDs from BOTH sinks
			uuids := []string{}
			seen := map[string]bool{}
			for _, ln := range strings.Split(logText, "\n") {
				for _, u := range c211xUUIDRe.FindAllString(ln, -1) {
					if !seen[u] {
						seen[u] = true
						uuids = append(uuids, u)
					}
				}
			}
			devLog := []string{}
			for _, ln := range strings.Split(logText, "\n") {
				if strings.Contains(ln, "C211X_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
					devLog = append(devLog, c211xTrunc(ln, 400))
				}
			}
			devGCS := []string{}
			for _, ln := range strings.Split(gcsText, "\n") {
				if strings.Contains(ln, "C211X_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
					devGCS = append(devGCS, c211xTrunc(ln, 400))
				}
			}
			ar.UUIDs = uuids
			ar.DeviceLog = c211xTrunc(strings.Join(devLog, "\n"), 4000)
			ar.DeviceGCS = c211xTrunc(strings.Join(devGCS, "\n"), 4000)
			ar.LogBytes = len(logText)
			ar.LogTail = c211xTrunc(logText, 2500)
			ar.GCSBytes = len(gcsText)
			ar.GCSProof = c211xTrunc(gcsText, 2500)
			objs, olst := c211sObjectList(ftok, p.Num)
			ar.GCSList = fmt.Sprintf("list_http=%d objects=%d %v", olst, len(objs), objs)
			for _, ln := range devLog {
				fmt.Printf("%s_DEVTEXT_LOG proj=%s shape=%s %s\n", c211sTag, p.Name, sh.Label, ln)
			}
			for _, ln := range devGCS {
				fmt.Printf("%s_DEVTEXT_GCS proj=%s shape=%s %s\n", c211sTag, p.Name, sh.Label, ln)
			}
			fmt.Printf("%s_LOG proj=%s shape=%s mode=%s log_bytes=%d gcs_bytes=%d uuids=%d gcs=%s\n",
				c211sTag, p.Name, sh.Label, mode, ar.LogBytes, ar.GCSBytes, len(uuids), c211xTrunc(ar.GCSList, 300))
			fmt.Printf("%s_READBACK proj=%s shape=%s %s\n", c211sTag, p.Name, sh.Label, ar.Readback)
			if summary, e := json.Marshal(ar); e == nil {
				fmt.Printf("%s_ATTEMPT_SUMMARY %s\n", c211sTag, c211xTrunc(string(summary), 6000))
			}
			if len(uuids) > 0 {
				ar.Proven = true
				provenAll = append(provenAll, fmt.Sprintf("%s:%s:%s:%d", p.Name, p.Num, sh.Label, len(uuids)))
				fmt.Printf("%s_PROVEN proj=%s shape=%s uuids=%s\n", c211sTag, p.Name, sh.Label,
					c211xTrunc(strings.Join(uuids, ","), 600))
				break // one proof per project is the deliverable
			}
			if !ar.RetireOK {
				retireFailed = true
				fmt.Printf("%s_RETIRE_FAILED proj=%s shape=%s - no further job in this project\n",
					c211sTag, p.Name, sh.Label)
			}
		}
		if r.Bucket != "" {
			r.BucketCle = c211sBucketCleanup(ftok, p.Num)
			fmt.Printf("%s_BUCKET_CLEANUP proj=%s %s\n", c211sTag, p.Name, r.BucketCle)
		}
	}

	out := map[string]interface{}{
		"nonce":      c211sNonce,
		"proven":     provenAll,
		"projects":   recs,
		"attempts":   attempts,
		"elapsed":    int(time.Since(started).Seconds()),
		"quotas":     func() map[string]interface{} { m := map[string]interface{}{}; for _, p := range c211sProjects { m[p.Num] = recs[p.Num].Quota }; return m }(),
	}
	blob := c211sJSONStringStable(out)
	fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven=%d\n", c211sTag,
		int(time.Since(started).Seconds()), len(provenAll))
	fmt.Printf("%s_JSON %s\n", c211sTag, c211xTrunc(blob, 24000))
	fmt.Printf("%s_END %s\n", c211sTag, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}
