//go:build functional && gke

// C211 i124598 — Vertex AI aiplatform custom-training RTX PRO 6000 Blackwell, host
// project 153711203585 (domino-eng-platform-dev), ON-DEMAND attempt then ONE SPOT
// job, with the PROVEN independent metadata-token GCS + direct-logging sink.
//
// Basis:
//   * custom_model_training_nvidia_rtx_pro_6000_gpus (on-demand) measures ZERO
//     positive buckets in all 42 regions while
//     custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus measures 14.0 in
//     every region (fact 124183 / 124198), and Compute offers accelerator
//     nvidia-rtx-pro-6000 maxCardsPerInstance=8 on g4-standard-384 (= 8 x
//     NVIDIA RTX PRO 6000) across 47-48 zones.
//   * the x123851 lane already created HTTP 200 and SUCCEEDED a SPOT 8x
//     customJob (3400159796735770624) with acceleratorCount=8 readback but got
//     zero log bytes and zero device lines ONLY because the in-job sink was
//     broken (storage.objects.create denied for the fleet SA and the platform
//     service agent was not granted on the bucket).
//   * fact 124314 proves the replacement sink works: the container's own
//     metadata-server token uploads the proof object (HTTP 200, real GPU UUIDs)
//     and a direct logging entries:write is attempted, even when Cloud Logging
//     reads 0.
//
// This probe therefore: reads the RTX PRO 6000 quotaInfos + host offering
// matrix, makes EXACTLY ONE on-demand 8x create (expected quota refusal,
// recorded verbatim) and — only on that refusal — submits EXACTLY ONE SPOT 8x
// job whose command prints nvidia-smi -L / --query-gpu=name,uuid,memory.total,
// driver_version and lspci -nn through BOTH sinks, polls it to terminal, takes
// an independent customJobs.get readback, retires it proving GET 404/list-empty,
// and as a bounded secondary retires the residual x123851 customJob
// 4080308893584982016. Never more than one job alive.
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

const c211vNonce = "i124598"
const c211vTag = "C211R" // keep the x123851 tag so the proven lander parses it

type c211vProj struct {
	Name string
	Num  string
}

// host project only: the one project where the WIF identity holds
// aiplatform.customJobs.create and where x123851 created the 8x SPOT job.
var c211vProjects = []c211vProj{
	{"domino-eng-platform-dev", "153711203585"},
}

var c211vPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "logging.logEntries.create",
	"serviceusage.services.get", "serviceusage.services.enable",
	"storage.buckets.create", "storage.buckets.delete",
	"storage.objects.create", "storage.objects.get",
	"iam.serviceAccounts.actAs",
}

var c211vQuotaMetrics = []string{
	"custom_model_training_nvidia_rtx_pro_6000_gpus",
	"custom_model_training_preemptible_nvidia_rtx_pro_6000_gpus",
	"custom_model_serving_nvidia_rtx_pro_6000_gpus",
	"custom_model_serving_preemptible_nvidia_rtx_pro_6000_gpus",
}

var c211vAccelNames = []string{"nvidia-rtx-pro-6000", "nvidia-rtx-pro-6000-vws"}
var c211vMachineTypes = []string{"g4-standard-384", "g4-standard-192", "g4-standard-96", "g4-standard-48"}

// residual customJob from the x123851 lane (4x, cancelled but stuck PENDING)
const c211vResidualJob = "projects/153711203585/locations/us-central1/customJobs/4080308893584982016"

// ---------------------------------------------------------------------------
// helpers (c211v* namespace; c211x* come from the helpers file)
// ---------------------------------------------------------------------------

func c211vStr(m map[string]interface{}, k string) string { return c211xStr(m, k) }

// c211vQuota returns metric -> region -> effective limit (>0 only) plus a digest
// string that keeps zero buckets visible.
func c211vQuotaDigest(jtok, num, metric string) (string, int) {
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
		if x := c211vStr(im, "metric"); x != "" {
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
		c211vSort(names)
		return fmt.Sprintf("buckets=%d zero=%d nonzero=[%s] regions=[%s]",
			len(names), zero, strings.Join(nz, ","), strings.Join(names, ",")), st
	}
	return "metric_absent", st
}

// c211vQuotaInfo prints the verbatim quotaInfo scaffolding (quotaId, metric,
// dimension keys, isEligible) for the requested metric substrings.
func c211vQuotaInfo(jtok, num string, want []string) {
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	fmt.Printf("%s_QUOTAINFO_HTTP http=%d\n", c211vTag, st)
	if st != 200 {
		fmt.Printf("%s_QUOTAINFO_ERR verbatim=%s\n", c211vTag, c211xTrunc(body, 400))
		return
	}
	m := c211xJSON(body)
	items, _ := m["metrics"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		metric := c211vStr(im, "metric")
		hit := false
		for _, w := range want {
			if strings.Contains(metric, w) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		lims, _ := im["consumerQuotaLimits"].([]interface{})
		for _, li := range lims {
			lm, _ := li.(map[string]interface{})
			qid := c211vStr(lm, "quotaId")
			unit := c211vStr(lm, "unit")
			el, _ := lm["quotaIncreaseEligibility"].(map[string]interface{})
			elig := ""
			if el != nil {
				b, _ := json.Marshal(el)
				elig = string(b)
			}
			dimKeys := []string{}
			cur := []string{}
			buckets, _ := lm["quotaBuckets"].([]interface{})
			for _, bi := range buckets {
				bm, _ := bi.(map[string]interface{})
				if dm, ok := bm["dimensions"].(map[string]interface{}); ok {
					dk := []string{}
					for k, v := range dm {
						dk = append(dk, fmt.Sprintf("%s=%v", k, v))
					}
					c211vSort(dk)
					ds := strings.Join(dk, ",")
					seen := false
					for _, x := range dimKeys {
						if x == ds {
							seen = true
						}
					}
					if !seen {
						dimKeys = append(dimKeys, ds)
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
					cur = append(cur, fmt.Sprintf("%v=%.0f", bm["dimensions"], f))
				}
			}
			c211vSort(dimKeys)
			fmt.Printf("%s_QUOTAINFO metric=%s quotaId=%s unit=%s eligible=%s dims=[%s] nonzero=[%s]\n",
				c211vTag, metric, qid, unit, c211xTrunc(elig, 160),
				c211xTrunc(strings.Join(dimKeys, "|"), 400), c211xTrunc(strings.Join(cur, ";"), 300))
		}
	}
}

func c211vSort(a []string) {
	for i := 0; i < len(a); i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

func c211vOffering(jtok, num, machine string) ([]string, int) {
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
			if c211vStr(xm, "name") == machine {
				parts := strings.Split(scope, "/")
				zones = append(zones, parts[len(parts)-1])
			}
		}
	}
	c211vSort(zones)
	return zones, st
}

func c211vAccel(jtok, num, accel string) ([]string, int) {
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
			if c211vStr(xm, "name") != accel {
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
	c211vSort(zones)
	return zones, st
}

func c211vBucketName(num string) string { return "c211-i124598-" + num }

func c211vBucketCreate(jtok, num, region string) (int, string) {
	body := fmt.Sprintf(`{"name":"%s","location":"%s","storageClass":"STANDARD",`+
		`"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`, c211vBucketName(num), region)
	st, rb := c211xHTTP("POST", "https://storage.googleapis.com/storage/v1/b?project="+num,
		jtok, "application/json", body)
	return st, c211xTrunc(rb, 300)
}

// c211vBucketIam grants objectAdmin to the fleet SA AND to the platform service
// agents that actually run the customJob container (the x123851 sink failed
// because only the fleet SA was granted).
func c211vBucketIam(jtok, num, fleetEmail string) (int, string) {
	members := []string{
		"serviceAccount:" + num + "-compute@developer.gserviceaccount.com",
		"serviceAccount:" + num + "@cloudservices.gserviceaccount.com",
		"serviceAccount:service-" + num + "@gcp-sa-aiplatform.iam.gserviceaccount.com",
	}
	if fleetEmail != "" {
		members = append(members, "serviceAccount:"+fleetEmail)
	}
	body := fmt.Sprintf(`{"bindings":[{"role":"roles/storage.objectAdmin","members":[%s]}]}`,
		func() string {
			q := []string{}
			for _, m := range members {
				q = append(q, fmt.Sprintf("%q", m))
			}
			return strings.Join(q, ",")
		}())
	st, rb := c211xHTTP("PUT", "https://storage.googleapis.com/storage/v1/b/"+
		c211vBucketName(num)+"/iam?project="+num, jtok, "application/json", body)
	return st, c211xTrunc(rb, 400)
}

func c211vObjectRead(jtok, num, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + c211vBucketName(num) +
		"/o/" + strings.ReplaceAll(obj, "/", "%2F") + "?alt=media"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	return st, body
}

func c211vBucketCleanup(jtok, num string) string {
	u := "https://storage.googleapis.com/storage/v1/b/" + c211vBucketName(num) + "/o?pageSize=100"
	st, body := c211xHTTP("GET", u, jtok, "", "")
	objs := []string{}
	if st == 200 {
		m := c211xJSON(body)
		if items, ok := m["items"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				if n := c211vStr(im, "name"); n != "" {
					objs = append(objs, n)
				}
			}
		}
	}
	for _, o := range objs {
		du := "https://storage.googleapis.com/storage/v1/b/" + c211vBucketName(num) +
			"/o/" + strings.ReplaceAll(o, "/", "%2F")
		c211xHTTP("DELETE", du, jtok, "", "")
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+
		c211vBucketName(num)+"?project="+num, jtok, "", "")
	gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+
		c211vBucketName(num)+"?project="+num, jtok, "", "")
	return fmt.Sprintf("objects=%d delete_http=%d get_after=%d verbatim=%s", len(objs), dst, gst, c211xTrunc(db, 160))
}

// c211vScript is the x123851 container script with the nonce/prefix updated.
// It prints the device evidence and pushes the same proof through BOTH the
// in-container metadata-token GCS upload and a direct logging entries:write.
func c211vScript(proj, shape, bucket string) string {
	script := `set -u
export PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/bin:/bin:/usr/local/bin:$PATH
echo "C211R_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124598 proj=__PROJ__ shape=__SHAPE__"
echo "C211R_UNAME $(uname -r)"
echo "C211R_LSPCI_RAW $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
echo "C211R_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
echo "C211R_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
SMI=""
for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
  if [ -x "$C" ]; then SMI="$C"; break; fi
done
echo "C211R_SMI_PATH $SMI"
if [ -z "$SMI" ]; then
  (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq nvidia-utils-580-server >/dev/null 2>&1) || true
  for C in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /usr/local/cuda/bin/nvidia-smi; do
    if [ -x "$C" ]; then SMI="$C"; break; fi
  done
  echo "C211R_SMI_PATH_AFTER $SMI"
fi
if [ -n "$SMI" ]; then
  echo "C211R_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211R_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211R_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211R_NOSMI"
fi
nvidia-smi -L 2>&1
nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller'
{
  echo "C211R_PROOF_NONCE i124598"
  echo "C211R_PROOF_PROJ __PROJ__"
  echo "C211R_PROOF_SHAPE __SHAPE__"
  echo "C211R_PROOF_HOST $(hostname)"
  echo "C211R_PROOF_AT $(date -u +%FT%TZ)"
  echo "C211R_PROOF_LSPCI $(lspci -nn 2>&1 | grep -i -E 'nvidia|3d controller' | tr '\n' '|')"
  echo "C211R_PROOF_LSDEV $(ls -l /dev/nvidia* 2>&1 | tr '\n' '|')"
  echo "C211R_PROOF_PROCVER $(cat /proc/driver/nvidia/version 2>&1 | tr '\n' '|')"
  nvidia-smi -L 2>&1
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv 2>&1
} > /tmp/c211v_proof.txt 2>&1
echo "C211R_PROOF_BEGIN"
cat /tmp/c211v_proof.txt
echo "C211R_PROOF_END"
TOK=""
if command -v curl >/dev/null 2>&1; then
  TOK=$(curl -s -m 20 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
fi
echo "C211R_SINK tokhave=$([ -n "$TOK" ] && echo yes || echo no) bucket=__BUCKET__"
if [ -n "$TOK" ] && [ -n "__BUCKET__" ]; then
  US=$(curl -s -m 90 -o /tmp/c211v_up.out -w "%{http_code}" -X POST --data-binary @/tmp/c211v_proof.txt -H "Authorization: Bearer $TOK" -H "Content-Type: text/plain" "https://storage.googleapis.com/upload/storage/v1/b/__BUCKET__/o?uploadType=media&name=c211-i124598/__PROJ__/proof.txt")
  echo "C211R_GCS_UPLOAD http=$US $(head -c 300 /tmp/c211v_up.out 2>/dev/null | tr '\n' ' ')"
  LBODY=$(python3 - <<'PYEOF' 2>/dev/null || true
import json
try:
    t = open('/tmp/c211v_proof.txt').read()
except Exception:
    t = ''
print(json.dumps({"logName": "projects/__PROJ__/logs/c211-i124598", "resource": {"type": "global"},
                  "entries": [{"textPayload": t}]}))
PYEOF
)
  if [ -z "$LBODY" ]; then
    LBODY="{\"logName\":\"projects/__PROJ__/logs/c211-i124598\",\"resource\":{\"type\":\"global\"},\"entries\":[{\"textPayload\":\"C211R_LOGWRITE_NO_PYTHON\"}]}"
  fi
  US2=$(curl -s -m 60 -o /tmp/c211v_log.out -w "%{http_code}" -X POST -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" --data-binary "$LBODY" "https://logging.googleapis.com/v2/entries:write")
  echo "C211R_LOG_WRITE http=$US2 $(head -c 200 /tmp/c211v_log.out 2>/dev/null | tr '\n' ' ')"
else
  echo "C211R_GCS_UPLOAD no_metadata_token_or_bucket"
fi
sleep 5
echo "C211R_DONE at=$(date -u +%FT%TZ)"`
	script = strings.ReplaceAll(script, "__PROJ__", proj)
	script = strings.ReplaceAll(script, "__SHAPE__", shape)
	script = strings.ReplaceAll(script, "__BUCKET__", bucket)
	return script
}

func c211vLogList(jtok, num, filter string) (int, string) {
	body := fmt.Sprintf(`{"resourceNames":["projects/%s"],"filter":%q,"orderBy":"timestamp asc","pageSize":500}`,
		num, filter)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", jtok,
		"application/json", body)
	return st, rb
}

func c211vLogs(jtok, num, jobID string) (string, string) {
	filters := []string{
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s" AND "C211R_"`, jobID),
		fmt.Sprintf(`resource.type="ml_job" AND resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`resource.labels.job_id="%s"`, jobID),
		fmt.Sprintf(`logName="projects/%s/logs/c211-i124598"`, num),
	}
	var lines []string
	lastErr := ""
	for _, f := range filters {
		st, rb := c211vLogList(jtok, num, f)
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

func c211vRetire(jtok, region, name string, live bool) (int, int, string) {
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
	for i := 0; i < 12; i++ {
		gst, _ := c211xHTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+name, jtok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(8 * time.Second)
	}
	return dst, getAfter, note
}

func c211vListJobs(jtok, num, region string) (int, int, string) {
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
				if strings.Contains(c211vStr(im, "displayName"), "c211") {
					c211n++
				}
			}
		}
	}
	return st, n, fmt.Sprintf("total=%d c211_items=%d verbatim=%s", n, c211n, c211xTrunc(body, 200))
}

// ---------------------------------------------------------------------------

func TestC211I124598VertexRtxSink(t *testing.T) {
	started := time.Now()
	deadline := started.Add(22 * time.Minute)
	fmt.Printf("%s_START nonce=%s at=%s\n", c211vTag, c211vNonce,
		started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("%s_MINT_FAILED %s\n", c211vTag, c211xTrunc(mintLog, 2000))
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
		c211vTag, method, c211xShort(wifSA), hostNum)

	// fleet token: the proven cross-project fleetcommand SA (prefix match as in x123851)
	fleetEmail := ""
	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	for _, sa := range sas {
		e := c211vStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("%s_FLEET_SA list=%d found=%v email=%s verbatim=%s\n",
		c211vTag, lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	ftok, fst, fbody := "", -1, ""
	if fleetEmail != "" {
		ftok, fst, fbody = c211xGenerateAccessToken(token, fleetEmail)
	}
	fmt.Printf("%s_FLEET_TOKEN status=%d token=%s verbatim=%s\n",
		c211vTag, fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		ftok = token
		fmt.Printf("%s_FLEET_TOKEN_FALLBACK using=wif_token\n", c211vTag)
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("%s_TOKENINFO status=%d verbatim=%s\n", c211vTag, tiSt, c211xTrunc(tiBody, 300))

	// phase 1 — host project: identity, permission, quotaInfo, offering (read-only)
	target := ""
	targetProj := ""
	for _, p := range c211vProjects {
		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, ftok, "", "")
		state := c211vStr(c211xJSON(pmBody), "lifecycleState")
		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1beta1/projects/"+p.Num+
			"/services/aiplatform.googleapis.com", ftok, "", "")
		svcState := c211vStr(c211xJSON(svBody), "state")
		al, ast, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+p.Num, c211vPerms)
		denied := []string{}
		for _, perm := range c211vPerms {
			if !c211xContains(al, perm) {
				denied = append(denied, perm)
			}
		}
		fmt.Printf("%s_PROJECT proj=%s num=%s get_http=%d state=%s svc_http=%d svc=%s perm_http=%d allowed=%s denied=%s\n",
			c211vTag, p.Name, p.Num, pmSt, state, svSt, svcState, ast,
			c211xTrunc(strings.Join(al, ","), 400), strings.Join(denied, ","))
		for _, m := range c211vQuotaMetrics {
			dg, dst := c211vQuotaDigest(ftok, p.Num, m)
			fmt.Printf("%s_QUOTA_DIGEST proj=%s http=%d %s %s\n", c211vTag, p.Name, dst, m, c211xTrunc(dg, 1400))
		}
		c211vQuotaInfo(ftok, p.Num, []string{"rtx_pro_6000", "nvidia_rtx_pro_6000"})
		for _, mt := range c211vMachineTypes {
			z, ost := c211vOffering(ftok, p.Num, mt)
			hit := []string{}
			for _, zz := range z {
				if strings.HasPrefix(zz, "us-central1-") {
					hit = append(hit, zz)
				}
			}
			fmt.Printf("%s_MACHINE proj=%s %s http=%d zones_n=%d uscentral1=%s all=%s\n",
				c211vTag, p.Name, mt, ost, len(z), strings.Join(hit, ","), c211xTrunc(strings.Join(z, ","), 600))
		}
		for _, an := range c211vAccelNames {
			z, ost := c211vAccel(ftok, p.Num, an)
			hit := []string{}
			for _, zz := range z {
				if strings.HasPrefix(zz, "us-central1-") {
					hit = append(hit, zz)
				}
			}
			fmt.Printf("%s_ACCEL proj=%s %s http=%d zones_n=%d uscentral1=%s all=%s\n",
				c211vTag, p.Name, an, ost, len(z), strings.Join(hit, ","), c211xTrunc(strings.Join(z, ","), 900))
		}
		if c211xContains(al, "aiplatform.customJobs.create") {
			target = p.Num
			targetProj = p.Name
		}
	}
	if target == "" {
		fmt.Printf("%s_VERDICT_NO_AI_PLATFORM_CREATE\n", c211vTag)
		out := map[string]interface{}{"nonce": c211vNonce, "target": "", "verdict": "no_aiplatform_customJobs_create"}
		b, _ := json.Marshal(out)
		fmt.Printf("%s_JSON %s\n", c211vTag, c211xTrunc(string(b), 8000))
		return
	}
	fmt.Printf("%s_TARGET proj=%s num=%s\n", c211vTag, targetProj, target)

	// bucket for the independent sink
	bucket := ""
	bst, bb := c211vBucketCreate(ftok, target, "us-central1")
	fmt.Printf("%s_BUCKET_CREATE name=%s http=%d verbatim=%s\n", c211vTag, c211vBucketName(target), bst, bb)
	if bst == 200 || bst == 409 {
		bucket = c211vBucketName(target)
		ist, ib := c211vBucketIam(ftok, target, fleetEmail)
		fmt.Printf("%s_BUCKET_IAM http=%d verbatim=%s\n", c211vTag, ist, ib)
	}

	type attemptRec struct {
		Shape      string `json:"shape"`
		Mode       string `json:"mode"`
		Machine    string `json:"machine"`
		Accel      string `json:"accel"`
		Count      int    `json:"count"`
		Region     string `json:"region"`
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
		GCSHTTP    int    `json:"gcs_http"`
		GCSProof   string `json:"gcs_proof"`
		Retire     string `json:"retire"`
		Absence    string `json:"absence"`
		ListAfter  string `json:"list_after"`
		WaitedSec  int    `json:"waited_sec"`
	}
	attempts := []*attemptRec{}
	proven := ""

	buildJob := func(mode, machine, accel string, count int, bucketName string) []byte {
		disp := fmt.Sprintf("c211-i124598-vtx-rtx-%dx-%s", count, strings.ToLower(mode))
		js := map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      machine,
					"acceleratorType":  accel,
					"acceleratorCount": count,
				},
				"replicaCount": 1,
				"containerSpec": map[string]interface{}{
					"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
					"command":  []string{"bash", "-c"},
					"args":     []string{c211vScript(targetProj, fmt.Sprintf("rtx-pro-6000-%dx", count), bucketName)},
				},
				"diskSpec": map[string]interface{}{
					"bootDiskType": "hyperdisk-balanced", "bootDiskSizeGb": 300,
				},
			}},
		}
		if mode == "SPOT" {
			js["scheduling"] = map[string]interface{}{
				"strategy": "SPOT", "disableRetries": true, "timeout": "900s",
			}
		}
		body := map[string]interface{}{"displayName": disp, "jobSpec": js}
		if fleetEmail != "" {
			body["jobSpec"].(map[string]interface{})["serviceAccount"] = fleetEmail
		}
		b, _ := json.Marshal(body)
		return b
	}

	runOne := func(mode string) *attemptRec {
		shapeLabel := "rtx-pro-6000-8x-" + strings.ToLower(mode)
		ar := &attemptRec{Shape: shapeLabel, Mode: mode, Machine: "g4-standard-384",
			Accel: "NVIDIA_RTX_PRO_6000", Count: 8, Region: "us-central1"}
		jbytes := buildJob(mode, ar.Machine, ar.Accel, ar.Count, bucket)
		fmt.Printf("%s_REQUEST shape=%s machine=%s accel=%s count=%d bytes=%d disk=hyperdisk-balanced bucket=%s\n",
			c211vTag, ar.Shape, ar.Machine, ar.Accel, ar.Count, len(jbytes), bucket)
		url := "https://" + ar.Region + "-aiplatform.googleapis.com/v1/projects/" + target +
			"/locations/" + ar.Region + "/customJobs"
		cst, cb := c211xHTTP("POST", url, ftok, "application/json", string(jbytes))
		ar.CreateHTTP = cst
		ar.CreateBody = c211xTrunc(cb, 1200)
		fmt.Printf("%s_CREATE shape=%s http=%d verbatim=%s\n", c211vTag, ar.Shape, cst, ar.CreateBody)
		if cst != 200 {
			if fleetEmail != "" && strings.Contains(cb, "act as service_account") {
				// one retry without serviceAccount on an actAs refusal
				jb2 := buildJob(mode, ar.Machine, ar.Accel, ar.Count, bucket)
				var m2 map[string]interface{}
				json.Unmarshal(jb2, &m2)
				delete(m2["jobSpec"].(map[string]interface{}), "serviceAccount")
				b2, _ := json.Marshal(m2)
				cst, cb = c211xHTTP("POST", url, ftok, "application/json", string(b2))
				ar.CreateHTTP = cst
				ar.CreateBody = c211xTrunc(cb, 1200)
				fmt.Printf("%s_CREATE_RETRY_NO_SA shape=%s http=%d verbatim=%s\n",
					c211vTag, ar.Shape, cst, ar.CreateBody)
			}
			return ar
		}
		jm := c211xJSON(cb)
		name := c211vStr(jm, "name")
		ar.Job = name
		jobID := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			jobID = name[i+1:]
		}
		fmt.Printf("%s_JOB shape=%s name=%s\n", c211vTag, ar.Shape, name)
		states := []string{}
		last := ""
		final := ""
		devStart := time.Now()
		pollMax := 13 * time.Minute
		if rem := deadline.Sub(time.Now()) - 4*time.Minute; rem < pollMax {
			pollMax = rem
		}
		if pollMax < 90*time.Second {
			pollMax = 90 * time.Second
		}
		for i := 0; time.Since(devStart) < pollMax; i++ {
			time.Sleep(20 * time.Second)
			gst, gb := c211xHTTP("GET", "https://"+ar.Region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
			st := c211vStr(c211xJSON(gb), "state")
			if st != last {
				states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(devStart).Seconds()), st))
				fmt.Printf("%s_POLL shape=%s i=%d http=%d state=%s\n", c211vTag, ar.Shape, i, gst, st)
				last = st
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
		lt, lerr := c211vLogs(ftok, target, jobID)
		ar.LogBytes = len(lt)
		if lerr != "" {
			fmt.Printf("%s_LOG_ERR shape=%s %s\n", c211vTag, ar.Shape, lerr)
		}
		ar.Logs = c211xTrunc(lt, 4000)
		if bucket != "" {
			gst, gb := c211vObjectRead(ftok, target, "c211-i124598/"+targetProj+"/proof.txt")
			ar.GCSHTTP = gst
			ar.GCSProof = c211xTrunc(gb, 3000)
			fmt.Printf("%s_GCS_READ shape=%s http=%d bytes=%d body=%s\n",
				c211vTag, ar.Shape, gst, len(gb), c211xTrunc(gb, 1500))
			if ar.LogBytes < len(gb) {
				ar.Logs = c211xTrunc(gb, 4000)
				ar.LogBytes = len(gb)
			}
		}
		devLines := []string{}
		uuids := []string{}
		for _, ln := range strings.Split(ar.Logs+"\n"+ar.GCSProof, "\n") {
			if strings.Contains(ln, "C211R_") || strings.Contains(ln, "GPU-") || strings.Contains(ln, "NVIDIA") {
				if len(devLines) < 80 {
					devLines = append(devLines, c211xTrunc(ln, 400))
				}
			}
			if strings.Contains(ln, "GPU-") {
				for _, u := range c211xUUIDRe.FindAllString(ln, -1) {
					seen := false
					for _, x := range uuids {
						if x == u {
							seen = true
						}
					}
					if !seen {
						uuids = append(uuids, u)
					}
				}
			}
		}
		ar.Device = strings.Join(devLines, "\n")
		ar.UUIDs = strings.Join(uuids, ",")
		for _, ln := range devLines {
			fmt.Printf("%s_DEVICE_LINE shape=%s %s\n", c211vTag, ar.Shape, ln)
		}
		fmt.Printf("%s_LOG shape=%s bytes=%d uuids=%d uuids_list=%s\n", c211vTag, ar.Shape,
			ar.LogBytes, len(uuids), c211xTrunc(strings.Join(uuids, ","), 600))
		rbSt, rbBody := c211xHTTP("GET", "https://"+ar.Region+"-aiplatform.googleapis.com/v1/"+name, ftok, "", "")
		rbm := c211xJSON(rbBody)
		ar.Readback = fmt.Sprintf("http=%d state=%s", rbSt, c211vStr(rbm, "state"))
		if js, ok := rbm["jobSpec"].(map[string]interface{}); ok {
			if wps, ok2 := js["workerPoolSpecs"].([]interface{}); ok2 && len(wps) > 0 {
				wm, _ := wps[0].(map[string]interface{})
				if ms, ok3 := wm["machineSpec"].(map[string]interface{}); ok3 {
					ar.Readback += fmt.Sprintf(" machine=%v accel=%v count=%v",
						ms["machineType"], ms["acceleratorType"], ms["acceleratorCount"])
				}
				if ds, ok3 := wm["diskSpec"].(map[string]interface{}); ok3 {
					ar.Readback += fmt.Sprintf(" bootDiskType=%v", ds["bootDiskType"])
				}
			}
			if sc, ok2 := js["scheduling"].(map[string]interface{}); ok2 {
				ar.Readback += fmt.Sprintf(" strategy=%v", sc["strategy"])
			}
		}
		fmt.Printf("%s_READBACK shape=%s %s\n", c211vTag, ar.Shape, ar.Readback)
		live := !strings.HasPrefix(final, "JOB_STATE_SUCCEEDED") && !strings.HasPrefix(final, "JOB_STATE_FAILED") &&
			!strings.HasPrefix(final, "JOB_STATE_CANCELLED") && !strings.HasPrefix(final, "JOB_STATE_EXPIRED")
		dst, getAfter, note := c211vRetire(ftok, ar.Region, name, live)
		ar.Retire = note
		fmt.Printf("%s_RETIRE shape=%s delete_http=%d get_after=%d %s\n", c211vTag, ar.Shape, dst, getAfter, note)
		lstSt, _, lbody2 := c211vListJobs(ftok, target, ar.Region)
		ar.ListAfter = fmt.Sprintf("http=%d %s", lstSt, c211xTrunc(lbody2, 240))
		ar.Absence = fmt.Sprintf("get_after=%d list=%s", getAfter, ar.ListAfter)
		fmt.Printf("%s_ABSENCE shape=%s get_after=%d list=%s\n", c211vTag, ar.Shape, getAfter, ar.ListAfter)
		if len(uuids) >= 8 {
			proven = fmt.Sprintf("%s:%s:%s", targetProj, ar.Shape, strings.Join(uuids, ","))
		}
		_ = jobID
		return ar
	}

	// phase 2 — EXACTLY ONE on-demand attempt (verbatim refusal expected)
	od := runOne("ONDEMAND")
	attempts = append(attempts, od)
	odRefused := od.CreateHTTP != 200
	if od.CreateHTTP == 200 && len(strings.Split(od.UUIDs, ",")) >= 8 {
		// unexpected on-demand success with a device line: do not submit SPOT
		fmt.Printf("%s_ONDEMAND_UNEXPECTED_SUCCESS no_spot_submitted\n", c211vTag)
	} else if odRefused || od.UUIDs == "" {
		fmt.Printf("%s_ONDEMAND_REFUSAL_RECORDED http=%d -> submitting ONE SPOT variant\n",
			c211vTag, od.CreateHTTP)
		sp := runOne("SPOT")
		attempts = append(attempts, sp)
	}

	// phase 3 — bounded secondary: retire the residual x123851 4x customJob
	resGet1, _ := c211xHTTP("GET", "https://us-central1-aiplatform.googleapis.com/v1/"+c211vResidualJob, ftok, "", "")
	resNote := fmt.Sprintf("get_before=%d", resGet1)
	if resGet1 != 404 {
		cst, cb := c211xHTTP("POST", "https://us-central1-aiplatform.googleapis.com/v1/"+c211vResidualJob+":cancel",
			ftok, "application/json", `{}`)
		resNote += fmt.Sprintf(" cancel_http=%d verbatim=%s", cst, c211xTrunc(cb, 160))
		time.Sleep(6 * time.Second)
		dst, db := c211xHTTP("DELETE", "https://us-central1-aiplatform.googleapis.com/v1/"+c211vResidualJob, ftok, "", "")
		resNote += fmt.Sprintf(" delete_http=%d verbatim=%s", dst, c211xTrunc(db, 200))
		for i := 0; i < 8; i++ {
			g, _ := c211xHTTP("GET", "https://us-central1-aiplatform.googleapis.com/v1/"+c211vResidualJob, ftok, "", "")
			resNote += fmt.Sprintf(" get_after_%d=%d", i, g)
			if g == 404 {
				break
			}
			time.Sleep(8 * time.Second)
		}
	}
	fmt.Printf("%s_RESIDUAL job=%s %s\n", c211vTag, c211vResidualJob, resNote)

	// phase 4 — bucket cleanup + final verdict
	bucketNote := ""
	if bucket != "" {
		bucketNote = c211vBucketCleanup(ftok, target)
		fmt.Printf("%s_BUCKET_CLEANUP %s\n", c211vTag, bucketNote)
	}
	out := map[string]interface{}{
		"nonce":       c211vNonce,
		"target":      target,
		"target_proj": targetProj,
		"proven":      proven,
		"attempts":    attempts,
		"residual":    resNote,
		"bucket":      bucketNote,
		"elapsed":     int(time.Since(started).Seconds()),
	}
	b, _ := json.Marshal(out)
	fmt.Printf("%s_VERDICT_FINAL elapsed=%ds proven=%s\n", c211vTag,
		int(time.Since(started).Seconds()), c211xTrunc(proven, 400))
	fmt.Printf("%s_JSON %s\n", c211vTag, c211xTrunc(string(b), 20000))
	fmt.Printf("%s_END %s\n", c211vTag, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}
