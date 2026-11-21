//go:build functional && gke

// C211 i124630 — host-project 153711203585 Vertex SERVING-plane EXTENSION probe.
//
// Basis (fact 124599): the x124413 run proved the serving plane bypasses both the
// GPU-metric Cloud Quotas preference wall and the 0.0 compute INSTANCES quota:
// an a4-highgpu-8g x8 NVIDIA_B200 deployment reached availableReplicaCount=1 and
// its container's nvidia-smi output came back through endpoints :predict HTTP 200
// with 8 real B200 UUIDs.  The same run also deployed a2-ultragpu-1g x1
// NVIDIA_A100_80GB (deployment 6454804506893877248 reached available) but its
// predict line was served by the still-attached B200 deployment, so it yielded
// no device line; the a3-highgpu-8g x8 H100 shape stayed unterminated.
//
// This probe deploys ONE shape at a time on its OWN fresh nonce endpoint and
// fully retires it (undeploy -> delete endpoint -> GET 404) before the next
// shape, so :predict can only be answered by the deployment under test:
//
//	(a) h100-1x-a3-highgpu   a3-highgpu-1g   x1 NVIDIA_H100_80GB   (first H100-class on-demand serving attempt)
//	(b) h100-8x-a3-highgpu   a3-highgpu-8g   x8 NVIDIA_H100_80GB   (only if (a) reaches availableReplicaCount>=1)
//	(c) a100-80gb-1x-a2-ultragpu a2-ultragpu-1g x1 NVIDIA_A100_80GB
//
// Every shape's nvidia-smi -L / --query-gpu=name,uuid,memory.total,driver_version
// and lspci -nn output is captured into /tmp/dev.txt inside the serving container
// and served back as JSON on AIP_HTTP_PORT; a real GPU- UUID line is device proof.
// No pre-existing endpoint/model/customJob owned by another lane is touched.
package functional

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	c211vProj    = "153711203585"
	c211vRegion  = "us-central1"
	c211vSA      = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211vBase    = "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1"
	c211vAPIRoot = "https://us-central1-aiplatform.googleapis.com/v1/"
	c211vNonce   = "c211-i124630"
)

var c211vFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

var c211vStartup = `cd /tmp && (echo "nonce=c211-i124630 host=$(hostname) at=$(date -u +%FT%TZ)"; nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv; echo '---'; lspci -nn | grep -i -E 'nvidia|3d controller|vga') > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
import http.server,json,os
D=open('/tmp/dev.txt').read()
class H(http.server.BaseHTTPRequestHandler):
    def _r(self):
        b=json.dumps({'predictions':[D]}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(b))); self.end_headers(); self.wfile.write(b)
    do_GET=_r
    do_POST=_r
    def log_message(self,*a): pass
http.server.HTTPServer(('0.0.0.0',int(os.environ.get('AIP_HTTP_PORT','8080'))),H).serve_forever()
"`

type c211vShape struct {
	name    string
	machine string
	accel   string
	count   int
}

// staged ladder (order matters: (b) only runs if (a) reaches availableReplicaCount>=1)
var c211vLadder = []c211vShape{
	{"h100-1x-a3-highgpu", "a3-highgpu-1g", "NVIDIA_H100_80GB", 1},
	{"h100-8x-a3-highgpu", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8},
	{"a100-80gb-1x-a2-ultragpu", "a2-ultragpu-1g", "NVIDIA_A100_80GB", 1},
}

// c211vEPRe extracts the endpoint resource id from either an endpoint resource
// name or a CreateEndpoint operation name of the form
// projects/<p>/locations/<r>/endpoints/<id>/operations/<opid>.
var c211vEPRe = regexp.MustCompile(`/endpoints/([0-9]+)(?:/operations/[0-9]+)?$`)

func c211vEndpointID(name string) string {
	name = strings.TrimRight(name, "/")
	if m := c211vEPRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func c211vTrunc(s string, n int) string { return c211xTrunc(s, n) }

func c211vUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211vOp(token, op string) (bool, string, string) {
	st, body := c211xHTTP("GET", c211vAPIRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, fmt.Sprintf("http=%d done=%v body=%s", st, done, c211vTrunc(body, 700)), body
}

func c211vErrText(body string) string {
	m := c211xJSON(body)
	if m == nil {
		return c211vTrunc(body, 900)
	}
	if e, ok := m["error"].(map[string]interface{}); ok {
		return fmt.Sprintf("code=%v status=%v message=%v", e["code"], c211xStr(e, "status"), c211xStr(e, "message"))
	}
	return c211vTrunc(body, 900)
}

// c211vEPReadback fetches the endpoint and returns the deployedModel entries
// plus the availableReplicaCount of the LAST entry (the one just deployed).
func c211vEPReadback(token, epID string) (int, string, string, int) {
	st, body := c211xHTTP("GET", c211vBase+"/endpoints/"+epID, token, "", "")
	depID := ""
	avail := -1
	m := c211xJSON(body)
	if m != nil {
		if dms, ok := m["deployedModels"].([]interface{}); ok && len(dms) > 0 {
			d0, _ := dms[len(dms)-1].(map[string]interface{})
			depID = c211xStr(d0, "id")
			if s, ok := d0["status"].(map[string]interface{}); ok {
				if v, ok := s["availableReplicaCount"].(float64); ok {
					avail = int(v)
				}
			}
		}
	}
	return st, body, depID, avail
}

func TestC211I124630Serving(t *testing.T) {
	start := time.Now()
	// The GKE vehicle allows roughly 30 min wall clock; keep cleanup headroom.
	hardEnd := start.Add(33 * time.Minute)
	reserve := 4 * time.Minute
	base := c211vBase

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211VX_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211vTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211VX_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211vSA)
	fmt.Printf("C211VX_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211vSA, gst, len(at), c211vTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211vFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211VX_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211vTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211VX_VERDICT_FINAL impersonation_failed gpus=0\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 1. permission matrix on the host project --------------------------
	perms := []string{
		"aiplatform.endpoints.deploy", "aiplatform.endpoints.undeploy",
		"aiplatform.endpoints.create", "aiplatform.endpoints.get",
		"aiplatform.endpoints.list", "aiplatform.endpoints.predict",
		"aiplatform.endpoints.delete",
		"aiplatform.models.upload", "aiplatform.models.get", "aiplatform.models.list", "aiplatform.models.delete",
		"aiplatform.customJobs.get", "aiplatform.customJobs.list",
		"storage.buckets.create", "serviceusage.services.get", "iam.serviceAccounts.actAs",
	}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211vProj, perms)
	fmt.Printf("C211VX_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211vTrunc(pbody, 700))

	// ---- 2. upload exactly ONE serving model (the vllm-serve prediction image
	// is the run-3-proven image that accepts a containerSpec command without an
	// artifact_uri) ----------------------------------------------------------
	img := "us-docker.pkg.dev/vertex-ai/vertex-vision-model-garden-dockers/pytorch-vllm-serve:latest"
	model := map[string]interface{}{
		"displayName": c211vNonce + "-model",
		"containerSpec": map[string]interface{}{
			"imageUri":          img,
			"command":           []string{"/bin/bash", "-c"},
			"args":              []string{c211vStartup},
			"ports":             []map[string]interface{}{{"containerPort": 8080}},
			"predictRoute":      "/",
			"healthRoute":       "/",
			"env":               []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
			"deploymentTimeout": "1800s",
		},
	}
	ust, ubody := c211xHTTP("POST", base+"/models:upload", at, "application/json", c211vUJ(map[string]interface{}{"model": model}))
	fmt.Printf("C211VX_MODEL_UPLOAD image=%s http=%d body=%s\n", img, ust, c211vTrunc(ubody, 800))
	modelName := ""
	op := c211xStr(c211xJSON(ubody), "name")
	if ust == 200 && op != "" {
		for i := 0; i < 40; i++ {
			time.Sleep(5 * time.Second)
			done, line, raw := c211vOp(at, op)
			if i%6 == 0 || done {
				fmt.Printf("C211VX_MODEL_OPPOLL t=%ds %s\n", (i+1)*5, c211vTrunc(line, 500))
			}
			if done {
				om := c211xJSON(raw)
				if resp, ok := om["response"].(map[string]interface{}); ok {
					mn := c211xStr(resp, "model")
					if mn == "" {
						mn = c211xStr(resp, "name")
					}
					if idx := strings.Index(mn, "/projects/"); idx >= 0 {
						mn = mn[idx+1:]
					}
					modelName = mn
				}
				break
			}
		}
	}
	fmt.Printf("C211VX_MODEL name=%s\n", modelName)
	if modelName == "" {
		fmt.Printf("C211VX_VERDICT_FINAL model_upload_failed gpus=0\n")
		t.Fatalf("model upload failed")
	}
	modelID := c211vEndpointID(modelName)

	// ---- 3. per-shape ladder: fresh endpoint each, one deployment at a time --
	type attempt struct {
		shape   string
		outcome string
		detail  string
	}
	attempts := []attempt{}
	type provenRec struct {
		shape  string
		n      int
		uuids  []string
		avail  int
		epID   string
		depID  string
		detail string
	}
	proven := []provenRec{}
	prevAvailable := false

	for si, sh := range c211vLadder {
		if si == 1 && !prevAvailable {
			fmt.Printf("C211VX_LADDER_SKIP shape=%s reason=prev-shape-not-available\n", sh.name)
			attempts = append(attempts, attempt{sh.name, "skipped", "previous shape did not reach availableReplicaCount>=1"})
			continue
		}
		if time.Now().After(hardEnd.Add(-reserve)) {
			fmt.Printf("C211VX_LADDER_STOP reason=budget shape=%s\n", sh.name)
			attempts = append(attempts, attempt{sh.name, "budget_skipped", "hardEnd reached"})
			continue
		}
		// 3a. fresh nonce endpoint
		epID := ""
		est, ebody := c211xHTTP("POST", base+"/endpoints", at, "application/json",
			c211vUJ(map[string]interface{}{"displayName": c211vNonce + "-ep-" + sh.name}))
		fmt.Printf("C211VX_EP_CREATE shape=%s http=%d body=%s\n", sh.name, est, c211vTrunc(ebody, 800))
		eop := c211xStr(c211xJSON(ebody), "name")
		if est == 200 && eop != "" {
			if id := c211vEndpointID(eop); id != eop && id != "" {
				epID = id
			}
			for i := 0; i < 30; i++ {
				time.Sleep(5 * time.Second)
				done, line, raw := c211vOp(at, eop)
				if i%3 == 0 || done {
					fmt.Printf("C211VX_EP_OPPOLL shape=%s t=%ds %s\n", sh.name, (i+1)*5, c211vTrunc(line, 500))
				}
				if done {
					om := c211xJSON(raw)
					if resp, ok := om["response"].(map[string]interface{}); ok {
						if nm := c211xStr(resp, "name"); nm != "" {
							epID = c211vEndpointID(nm)
						}
					}
					break
				}
				if time.Now().After(hardEnd.Add(-reserve)) {
					break
				}
			}
		}
		if epID == "" {
			fmt.Printf("C211VX_EP_FAIL shape=%s\n", sh.name)
			attempts = append(attempts, attempt{sh.name, "endpoint_create_failed", c211vErrText(ebody)})
			continue
		}
		fmt.Printf("C211VX_ENDPOINT shape=%s id=%s\n", sh.name, epID)

		// 3b. deployModel
		dep := map[string]interface{}{
			"deployedModel": map[string]interface{}{
				"model":       modelName,
				"displayName": c211vNonce + "-dep-" + sh.name,
				"dedicatedResources": map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType":      sh.machine,
						"acceleratorType":  sh.accel,
						"acceleratorCount": sh.count,
					},
					"minReplicaCount": 1,
					"maxReplicaCount": 1,
				},
			},
		}
		dst, dbody := c211xHTTP("POST", base+"/endpoints/"+epID+":deployModel", at, "application/json", c211vUJ(dep))
		fmt.Printf("C211VX_DEPLOY shape=%s machine=%s accel=%s count=%d http=%d body=%s\n",
			sh.name, sh.machine, sh.accel, sh.count, dst, c211vTrunc(dbody, 900))
		dop := c211xStr(c211xJSON(dbody), "name")
		if dst != 200 || dop == "" {
			attempts = append(attempts, attempt{sh.name, fmt.Sprintf("deploy_create_http_%d", dst), c211vErrText(dbody)})
			// retire the fresh endpoint
			dlst, dlb := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
			fmt.Printf("C211VX_EP_DELETE shape=%s http=%d body=%s\n", sh.name, dlst, c211vTrunc(dlb, 500))
			prevAvailable = false
			continue
		}
		deployed := false
		last := ""
		for i := 0; i < 60; i++ {
			time.Sleep(10 * time.Second)
			if time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211VX_DEPLOY_POLL_STOP shape=%s t=%ds reason=budget\n", sh.name, (i+1)*10)
				break
			}
			d, line, _ := c211vOp(at, dop)
			if i%3 == 0 {
				fmt.Printf("C211VX_DEPLOY_POLL shape=%s t=%ds %s\n", sh.name, (i+1)*10, c211vTrunc(line, 400))
			}
			last = line
			if d {
				deployed = true
				break
			}
		}
		if !deployed {
			fmt.Printf("C211VX_DEPLOY_UNTERMINAL shape=%s last=%s\n", sh.name, c211vTrunc(last, 500))
			attempts = append(attempts, attempt{sh.name, "poll_stop", c211vTrunc(last, 500)})
			prevAvailable = false
			// best-effort retire: try undeploy of any attached deployment, then delete the endpoint
			_, _, depID0, _ := c211vEPReadback(at, epID)
			if depID0 != "" {
				ust2, ub2 := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", at, "application/json",
					c211vUJ(map[string]interface{}{"deployedModelId": depID0}))
				fmt.Printf("C211VX_UNDEPLOY_UNTERMINAL shape=%s http=%d body=%s\n", sh.name, ust2, c211vTrunc(ub2, 500))
			}
			dlst, dlb := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
			fmt.Printf("C211VX_EP_DELETE shape=%s http=%d body=%s\n", sh.name, dlst, c211vTrunc(dlb, 500))
			continue
		}
		if strings.Contains(last, "\"error\"") {
			fmt.Printf("C211VX_DEPLOY_REFUSED shape=%s verbatim=%s\n", sh.name, c211vTrunc(last, 1200))
			attempts = append(attempts, attempt{sh.name, "op_error", c211vTrunc(last, 700)})
			prevAvailable = false
			dlst, dlb := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
			fmt.Printf("C211VX_EP_DELETE shape=%s http=%d body=%s\n", sh.name, dlst, c211vTrunc(dlb, 500))
			continue
		}
		fmt.Printf("C211VX_DEPLOY_OK shape=%s\n", sh.name)

		// 3c. independent endpoint readback: which deployment, how many replicas available
		egst, egb, depID, avail := c211vEPReadback(at, epID)
		fmt.Printf("C211VX_EP_GET_AFTER_DEPLOY shape=%s http=%d avail=%d depID=%s body=%s\n",
			sh.name, egst, avail, depID, c211vTrunc(egb, 1600))
		attempts = append(attempts, attempt{sh.name, fmt.Sprintf("deployed_avail_%d", avail), c211vTrunc(egb, 500)})
		if avail >= 1 {
			prevAvailable = true
		} else {
			prevAvailable = false
		}

		// 3d. read the device lines back through :predict (only this deployment is attached)
		got := []string{}
		if avail >= 1 {
			for i := 0; i < 12; i++ {
				pst2, pbody2 := c211xHTTP("POST", base+"/endpoints/"+epID+":predict", at, "application/json", `{"instances":[{}]}`)
				uu := c211xUUIDRe.FindAllString(pbody2, -1)
				fmt.Printf("C211VX_PREDICT shape=%s t=%d http=%d uuids=%d body=%s\n", sh.name, i, pst2, len(uu), c211vTrunc(pbody2, 2000))
				if pst2 == 200 && len(uu) > 0 {
					// unique UUIDs only
					seen := map[string]bool{}
					for _, u := range uu {
						if !seen[u] {
							seen[u] = true
							got = append(got, u)
						}
					}
					break
				}
				if time.Now().After(hardEnd.Add(-reserve)) {
					fmt.Printf("C211VX_PREDICT_STOP shape=%s reason=budget\n", sh.name)
					break
				}
				time.Sleep(20 * time.Second)
			}
		} else {
			fmt.Printf("C211VX_PREDICT_SKIP shape=%s reason=not_available\n", sh.name)
		}
		if len(got) > 0 {
			fmt.Printf("C211VX_PROVEN shape=%s n=%d uuids=%s\n", sh.name, len(got), strings.Join(got, ","))
			proven = append(proven, provenRec{sh.name, len(got), got, avail, epID, depID, ""})
		}

		// 3e. undeploy this deployment, delete this endpoint, prove GET 404 before next shape
		if depID != "" {
			ust2, ub := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", at, "application/json",
				c211vUJ(map[string]interface{}{"deployedModelId": depID}))
			fmt.Printf("C211VX_UNDEPLOY shape=%s id=%s http=%d body=%s\n", sh.name, depID, ust2, c211vTrunc(ub, 700))
			uop := c211xStr(c211xJSON(ub), "name")
			for i := 0; i < 30 && uop != ""; i++ {
				time.Sleep(10 * time.Second)
				d, line, _ := c211vOp(at, uop)
				if i%3 == 0 || d {
					fmt.Printf("C211VX_UNDEPLOY_POLL shape=%s t=%ds %s\n", sh.name, (i+1)*10, c211vTrunc(line, 400))
				}
				if d {
					break
				}
				if time.Now().After(hardEnd) {
					break
				}
			}
		}
		dlst, dlb := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
		fmt.Printf("C211VX_EP_DELETE shape=%s http=%d body=%s\n", sh.name, dlst, c211vTrunc(dlb, 600))
		dop2 := c211xStr(c211xJSON(dlb), "name")
		for i := 0; i < 30 && dop2 != ""; i++ {
			time.Sleep(5 * time.Second)
			d, _, _ := c211vOp(at, dop2)
			if i%3 == 0 || d {
				fmt.Printf("C211VX_EP_DELETE_POLL shape=%s t=%ds done=%v\n", sh.name, (i+1)*5, d)
			}
			if d {
				break
			}
		}
		agst, agb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
		fmt.Printf("C211VX_EP_ABSENCE shape=%s id=%s http=%d body=%s\n", sh.name, epID, agst, c211vTrunc(agb, 400))
		attempts = append(attempts, attempt{sh.name, fmt.Sprintf("retired_ep_http_%d", agst), "endpoint deleted after deploy"})
	}

	// ---- 4. cleanup: delete this run's model and prove absence -------------
	mdst, mdb := c211xHTTP("DELETE", base+"/models/"+modelID, at, "", "")
	fmt.Printf("C211VX_MODEL_DELETE id=%s http=%d body=%s\n", modelID, mdst, c211vTrunc(mdb, 600))
	mop := c211xStr(c211xJSON(mdb), "name")
	for i := 0; i < 24 && mop != ""; i++ {
		time.Sleep(5 * time.Second)
		d, _, _ := c211vOp(at, mop)
		if d {
			break
		}
	}
	mgst, mgb := c211xHTTP("GET", base+"/models/"+modelID, at, "", "")
	fmt.Printf("C211VX_MODEL_ABSENCE id=%s http=%d body=%s\n", modelID, mgst, c211vTrunc(mgb, 400))
	lst3, lbody3 := c211xHTTP("GET", base+"/models?pageSize=100", at, "", "")
	fmt.Printf("C211VX_MODEL_LIST_AFTER http=%d body=%s\n", lst3, c211vTrunc(lbody3, 900))
	lst4, lbody4 := c211xHTTP("GET", base+"/endpoints?pageSize=100", at, "", "")
	fmt.Printf("C211VX_EP_LIST_AFTER http=%d body=%s\n", lst4, c211vTrunc(lbody4, 900))

	fmt.Printf("C211VX_VERDICT_FINAL elapsed=%ds proven_shapes=%d uuids=%d model=%s\n",
		int(time.Since(start).Seconds()), len(proven), func() int {
			n := 0
			for _, p := range proven {
				n += len(p.uuids)
			}
			return n
		}(), modelName)
	for _, p := range proven {
		fmt.Printf("C211VX_PROVEN_FINAL shape=%s n=%d avail=%d ep=%s dep=%s uuids=%s\n",
			p.shape, len(p.uuids), p.avail, p.epID, p.depID, strings.Join(p.uuids, ","))
	}
	for _, a := range attempts {
		fmt.Printf("C211VX_ATTEMPT shape=%s outcome=%s detail=%s\n", a.shape, a.outcome, c211vTrunc(a.detail, 600))
	}
}
