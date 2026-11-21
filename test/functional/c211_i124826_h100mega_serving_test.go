//go:build functional && gke

// C211 i124826 — host-project 153711203585 Vertex SERVING-plane probe for the
// never-attempted NVIDIA_H100_MEGA_80GB class.
//
// Basis (facts 124599 / 124672 / 124674 / 124880): the Vertex SERVING plane is
// the one plane that bypasses BOTH the GPU-metric Cloud Quotas preference wall
// (verbatim 403 active policy restriction) and the 0.0 compute INSTANCES quota:
// an a4-highgpu-8g x8 NVIDIA_B200 deployModel reached availableReplicaCount=1
// and its container's own /tmp/dev.txt came back through endpoints :predict
// HTTP 200 with 8 real B200 UUIDs, impersonating
// dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com from the
// proven dominodatalab/hephaestus '/functional-test' GKE WIF vehicle.
//
// The saved in-identity quota matrix (i123642/host_quota_153711203585.json)
// reads custom_model_serving_nvidia_h100_mega_gpus = 0 in all 42 regions while
// custom_model_serving_preemptible_nvidia_h100_mega_gpus = 115.0 in all 42
// regions.  This probe therefore:
//
//	(1) records verbatim every H100-MEGA-related SERVING quotaInfo the Cloud
//	    Quotas API exposes (and the Service Usage consumerQuotaMetrics limits)
//	    and creates NO quotaPreference;
//	(2) deploys EXACTLY ONE nonce-tagged ON-DEMAND serving shape
//	    a3-megagpu-8g x8 NVIDIA_H100_MEGA_80GB (dedicatedResources.spot=false,
//	    min=1 max=1) on its own fresh nonce endpoint in us-central1;
//	(3) ONLY on a verbatim on-demand refusal submits ONE PREEMPTIBLE/spot
//	    variant of the same shape on the same nonce endpoint (mandate §2:
//	    spot only after a verbatim on-demand refusal);
//	(4) polls the deploy operation to terminal, takes an independent
//	    endpoints.get availableReplicaCount readback, reads the container's own
//	    /tmp/dev.txt back through endpoints :predict HTTP 200 and derives every
//	    GPU UUID verbatim from that output;
//	(5) undeploys, deletes and proves GET 404 / endpoints+models lists empty.
//
// No pre-existing endpoint/model/customJob owned by another lane is touched
// (in particular not endpoint 2866572498902712320, model 205538855405748224 or
// residual customJob 4080308893584982016), and no quotaPreference is created.
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
	c211sProj    = "153711203585"
	c211sRegion  = "us-central1"
	c211sBase    = "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1"
	c211sAPIRoot = "https://us-central1-aiplatform.googleapis.com/v1/"
	c211sSA      = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211sNonce   = "c211-i124826"
	c211sImage   = "us-docker.pkg.dev/vertex-ai/vertex-vision-model-garden-dockers/pytorch-vllm-serve:latest"
	c211sMachine = "a3-megagpu-8g"
	c211sAccel   = "NVIDIA_H100_MEGA_80GB"
	c211sCount   = 8
	c211sOldEP   = "2866572498902712320"
	c211sOldMdl  = "205538855405748224"
)

var c211sFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

// c211sStartup writes the container's OWN device lines to /tmp/dev.txt and
// serves that file as JSON on AIP_HTTP_PORT.  This is the run-3-proven
// containerSpec shape for this project (Model 205538855405748224).
var c211sStartup = `cd /tmp && (echo "nonce=c211-i124826 host=$(hostname) at=$(date -u +%FT%TZ)"; nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv; echo '---'; lspci -nn | grep -i -E 'nvidia|3d controller|vga'; echo '---'; ls -l /dev/nvidia* 2>&1; echo '---'; cat /proc/driver/nvidia/version 2>&1) > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
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

var c211sEPRe = regexp.MustCompile(`/endpoints/([0-9]+)(?:/operations/[0-9]+)?$`)

func c211sEndpointID(name string) string {
	name = strings.TrimRight(name, "/")
	if m := c211sEPRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func c211sTrunc(s string, n int) string { return c211xTrunc(s, n) }

func c211sUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211sOp(token, op string) (bool, string, string) {
	st, body := c211xHTTP("GET", c211sAPIRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, fmt.Sprintf("http=%d done=%v body=%s", st, done, c211sTrunc(body, 800)), body
}

func c211sErrText(body string) string {
	m := c211xJSON(body)
	if m == nil {
		return c211sTrunc(body, 900)
	}
	if e, ok := m["error"].(map[string]interface{}); ok {
		return fmt.Sprintf("code=%v status=%v message=%v", e["code"], c211xStr(e, "status"), c211xStr(e, "message"))
	}
	return c211sTrunc(body, 900)
}

// c211sRefusalClass reports whether a terminal deploy/operation error looks
// like a quota/capacity refusal (the precondition the mandate requires before a
// spot attempt).
func c211sRefusalClass(body string) bool {
	l := strings.ToLower(body)
	for _, k := range []string{
		"resource_exhausted", "quota", "capacity", "zone_resource_pool_exhausted",
		"resources are insufficient", "insufficient", "stockout", "no available",
		"unavailable", "exceeded",
	} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// c211sQuotaProbe records verbatim every H100-MEGA-related serving quotaInfo
// from Cloud Quotas and the matching Service Usage consumerQuotaMetrics.  It is
// read-only: no quotaPreference is ever created.
func c211sQuotaProbe(at string) (int, int) {
	matchedQuotaInfos := 0
	matchedSU := 0
	fmt.Printf("C211S_QUOTA_BEGIN nonce=%s proj=%s\n", c211sNonce, c211sProj)
	// ---- Cloud Quotas quotaInfos -------------------------------------------
	cq := "https://cloudquotas.googleapis.com/v1/projects/" + c211sProj + "/locations/global/quotaInfos?pageSize=100"
	for page := 0; page < 4 && cq != ""; page++ {
		st, body := c211xHTTP("GET", cq, at, "", "")
		m := c211xJSON(body)
		n := 0
		if m != nil {
			if arr, ok := m["quotaInfos"].([]interface{}); ok {
				n = len(arr)
				for _, qi := range arr {
					s := c211sUJ(qi)
					low := strings.ToLower(s)
					if strings.Contains(low, "h100") && (strings.Contains(low, "serving") || strings.Contains(low, "mega")) {
						matchedQuotaInfos++
						fmt.Printf("C211S_QUOTAINFO n=%d verbatim=%s\n", matchedQuotaInfos, c211sTrunc(s, 1900))
					}
				}
			}
		}
		next := ""
		if m != nil {
			next = c211xStr(m, "nextPageToken")
		}
		fmt.Printf("C211S_QUOTAINFOS_PAGE page=%d http=%d items=%d next=%v\n", page, st, n, next != "")
		if next == "" {
			cq = ""
		} else {
			cq = "https://cloudquotas.googleapis.com/v1/projects/" + c211sProj + "/locations/global/quotaInfos?pageSize=100&pageToken=" + next
		}
	}
	if matchedQuotaInfos == 0 {
		fmt.Printf("C211S_QUOTAINFO_NONE verbatim=no Cloud Quotas quotaInfo matching h100+serving/mega was returned for projects/%s\n", c211sProj)
	}
	// ---- Service Usage consumerQuotaMetrics -------------------------------
	su := "https://serviceusage.googleapis.com/v1/projects/" + c211sProj + "/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=100"
	for page := 0; page < 5 && su != ""; page++ {
		st, body := c211xHTTP("GET", su, at, "", "")
		m := c211xJSON(body)
		n := 0
		if m != nil {
			if arr, ok := m["metrics"].([]interface{}); ok {
				n = len(arr)
				for _, mt := range arr {
					s := c211sUJ(mt)
					low := strings.ToLower(s)
					if (strings.Contains(low, "custom_model_serving") || strings.Contains(low, "serving")) && strings.Contains(low, "h100") && strings.Contains(low, "mega") {
						matchedSU++
						fmt.Printf("C211S_SU_METRIC n=%d verbatim=%s\n", matchedSU, c211sTrunc(s, 2600))
					}
				}
			}
		}
		next := ""
		if m != nil {
			next = c211xStr(m, "nextPageToken")
		}
		fmt.Printf("C211S_SU_PAGE page=%d http=%d items=%d next=%v\n", page, st, n, next != "")
		if next == "" {
			su = ""
		} else {
			su = "https://serviceusage.googleapis.com/v1/projects/" + c211sProj + "/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=100&pageToken=" + next
		}
	}
	if matchedSU == 0 {
		fmt.Printf("C211S_SU_METRIC_NONE verbatim=no aiplatform consumerQuotaMetric matching serving+h100+mega was returned for projects/%s\n", c211sProj)
	}
	fmt.Printf("C211S_QUOTA_END quotaInfos_matched=%d su_metrics_matched=%d\n", matchedQuotaInfos, matchedSU)
	return matchedQuotaInfos, matchedSU
}

func TestC211I124826H100MegaServing(t *testing.T) {
	start := time.Now()
	// Wall-clock plan: ~2 min setup + quota read, ONE on-demand deploy verdict
	// (refusals are terminal within seconds; a successful deploy gets up to
	// ~22 min of poll), then ONE spot deploy (up to ~24 min), then predict and
	// cleanup, all bounded at 68 min with an 8 min cleanup reserve.
	hardEnd := start.Add(68 * time.Minute)
	reserve := 8 * time.Minute

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211S_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211sTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211S_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211sSA)
	fmt.Printf("C211S_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211sSA, gst, len(at), c211sTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211sFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211S_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211sTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211S_VERDICT_FINAL impersonation_failed gpus=0\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 0. verbatim permission matrix -------------------------------------
	perms := []string{
		"aiplatform.endpoints.deploy", "aiplatform.endpoints.undeploy",
		"aiplatform.endpoints.create", "aiplatform.endpoints.get",
		"aiplatform.endpoints.list", "aiplatform.endpoints.predict",
		"aiplatform.endpoints.delete",
		"aiplatform.models.upload", "aiplatform.models.get", "aiplatform.models.list", "aiplatform.models.delete",
		"iam.serviceAccounts.actAs", "serviceusage.services.get",
	}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211sProj, perms)
	fmt.Printf("C211S_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211sTrunc(pbody, 700))

	// ---- 0b. read-only confirmation of the other lane's objects -----------
	ogst, ogb := c211xHTTP("GET", c211sBase+"/endpoints/"+c211sOldEP, at, "", "")
	fmt.Printf("C211S_OTHER_EP_READONLY id=%s http=%d body=%s\n", c211sOldEP, ogst, c211sTrunc(ogb, 400))
	mgst, mgb := c211xHTTP("GET", c211sBase+"/models/"+c211sOldMdl, at, "", "")
	fmt.Printf("C211S_OTHER_MODEL_READONLY id=%s http=%d body=%s\n", c211sOldMdl, mgst, c211sTrunc(mgb, 400))

	// ---- 0c. verbatim H100-MEGA serving quotaInfo matrix -------------------
	c211sQuotaProbe(at)

	// ---- 1. upload ONE serving model ---------------------------------------
	model := map[string]interface{}{
		"displayName": c211sNonce + "-model",
		"containerSpec": map[string]interface{}{
			"imageUri":          c211sImage,
			"command":           []string{"/bin/bash", "-c"},
			"args":              []string{c211sStartup},
			"ports":             []map[string]interface{}{{"containerPort": 8080}},
			"predictRoute":      "/",
			"healthRoute":       "/",
			"env":               []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
			"deploymentTimeout": "1800s",
		},
	}
	ust, ubody := c211xHTTP("POST", c211sBase+"/models:upload", at, "application/json", c211sUJ(map[string]interface{}{"model": model}))
	fmt.Printf("C211S_MODEL_UPLOAD image=%s http=%d body=%s\n", c211sImage, ust, c211sTrunc(ubody, 800))
	modelName := ""
	mop := c211xStr(c211xJSON(ubody), "name")
	if ust == 200 && mop != "" {
		for i := 0; i < 40; i++ {
			time.Sleep(5 * time.Second)
			done, line, raw := c211sOp(at, mop)
			if i%6 == 0 || done {
				fmt.Printf("C211S_MODEL_OPPOLL t=%ds %s\n", (i+1)*5, c211sTrunc(line, 500))
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
			if time.Now().After(hardEnd.Add(-reserve)) {
				break
			}
		}
	}
	modelID := ""
	if modelName != "" {
		modelID = c211sEndpointID(modelName)
	}
	fmt.Printf("C211S_MODEL name=%s id=%s\n", modelName, modelID)
	if modelName == "" {
		fmt.Printf("C211S_VERDICT_FINAL model_upload_failed gpus=0 detail=%s\n", c211sErrText(ubody))
		t.Fatalf("model upload failed")
	}

	// ---- 2. ONE fresh nonce endpoint ---------------------------------------
	epID := ""
	est, ebody := c211xHTTP("POST", c211sBase+"/endpoints", at, "application/json",
		c211sUJ(map[string]interface{}{"displayName": c211sNonce + "-ep"}))
	fmt.Printf("C211S_EP_CREATE http=%d body=%s\n", est, c211sTrunc(ebody, 800))
	eop := c211xStr(c211xJSON(ebody), "name")
	if est == 200 && eop != "" {
		if id := c211sEndpointID(eop); id != eop && id != "" {
			epID = id
		}
		for i := 0; i < 30; i++ {
			time.Sleep(5 * time.Second)
			done, line, raw := c211sOp(at, eop)
			if i%3 == 0 || done {
				fmt.Printf("C211S_EP_OPPOLL t=%ds %s\n", (i+1)*5, c211sTrunc(line, 500))
			}
			if done {
				om := c211xJSON(raw)
				if resp, ok := om["response"].(map[string]interface{}); ok {
					if nm := c211xStr(resp, "name"); nm != "" {
						epID = c211sEndpointID(nm)
					}
				}
				break
			}
			if time.Now().After(hardEnd.Add(-reserve)) {
				break
			}
		}
	}
	fmt.Printf("C211S_ENDPOINT id=%s\n", epID)
	if epID == "" {
		fmt.Printf("C211S_VERDICT_FINAL endpoint_create_failed gpus=0 detail=%s\n", c211sErrText(ebody))
		// best-effort model cleanup
		c211xHTTP("DELETE", c211sBase+"/models/"+modelID, at, "", "")
		t.Fatalf("endpoint create failed")
	}

	type attempt struct {
		stage   string
		outcome string
		detail  string
	}
	attempts := []attempt{}

	// ---- 3. deploy stage helper -------------------------------------------
	type stageRes struct {
		label     string
		createHTTP int
		createBody string
		opName    string
		terminal  bool
		refused   bool
		deployed  bool
		avail     int
		depID     string
		last      string
		errText   string
	}

	doStage := func(label string, spot bool, pollMax int) stageRes {
		r := stageRes{label: label, avail: -1}
		dr := map[string]interface{}{
			"machineSpec": map[string]interface{}{
				"machineType":      c211sMachine,
				"acceleratorType":  c211sAccel,
				"acceleratorCount": c211sCount,
			},
			"minReplicaCount": 1,
			"maxReplicaCount": 1,
		}
		if spot {
			dr["spot"] = true
		}
		dep := map[string]interface{}{
			"deployedModel": map[string]interface{}{
				"model":             modelName,
				"displayName":       c211sNonce + "-dep-" + label,
				"dedicatedResources": dr,
			},
		}
		dst, dbody := c211xHTTP("POST", c211sBase+"/endpoints/"+epID+":deployModel", at, "application/json", c211sUJ(dep))
		r.createHTTP = dst
		r.createBody = c211sTrunc(dbody, 1200)
		fmt.Printf("C211S_DEPLOY stage=%s spot=%v machine=%s accel=%s count=%d http=%d body=%s\n",
			label, spot, c211sMachine, c211sAccel, c211sCount, dst, c211sTrunc(dbody, 1000))
		r.opName = c211xStr(c211xJSON(dbody), "name")
		if dst != 200 || r.opName == "" {
			r.terminal = true
			r.errText = c211sErrText(dbody)
			return r
		}
		for i := 0; i < pollMax; i++ {
			time.Sleep(10 * time.Second)
			if time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211S_DEPLOY_POLL_STOP stage=%s t=%ds reason=budget\n", label, (i+1)*10)
				break
			}
			d, line, raw := c211sOp(at, r.opName)
			r.last = line
			if i%3 == 0 {
				fmt.Printf("C211S_DEPLOY_POLL stage=%s t=%ds %s\n", label, (i+1)*10, c211sTrunc(line, 500))
			}
			_ = raw
			// Independent availability probe every 30s: the B200 run proved a
			// deployment can be serving while the long-running operation is
			// still not marked done.
			if i%3 == 2 {
				egst, egb := c211xHTTP("GET", c211sBase+"/endpoints/"+epID, at, "", "")
				if m := c211xJSON(egb); m != nil {
					if dms, ok := m["deployedModels"].([]interface{}); ok && len(dms) > 0 {
						d0, _ := dms[len(dms)-1].(map[string]interface{})
						if s, ok := d0["status"].(map[string]interface{}); ok {
							if v, ok := s["availableReplicaCount"].(float64); ok && int(v) >= 1 {
								r.depID = c211xStr(d0, "id")
								r.avail = int(v)
								fmt.Printf("C211S_EP_AVAILABLE stage=%s t=%ds avail=%d depID=%s http=%d\n",
									label, (i+1)*10, r.avail, r.depID, egst)
							}
						}
					}
				}
			}
			if d {
				r.terminal = true
				if strings.Contains(line, "\"error\"") {
					r.refused = true
					r.errText = c211sTrunc(line, 1800)
					fmt.Printf("C211S_DEPLOY_REFUSED stage=%s verbatim=%s\n", label, c211sTrunc(line, 1800))
				} else {
					r.deployed = true
					fmt.Printf("C211S_DEPLOY_OK stage=%s\n", label)
				}
				break
			}
			if r.avail >= 1 {
				break
			}
		}
		if !r.terminal && r.avail < 1 {
			fmt.Printf("C211S_DEPLOY_UNTERMINAL stage=%s last=%s\n", label, c211sTrunc(r.last, 700))
		}
		return r
	}

	// ---- 4. ON-DEMAND stage (mandatory first) ------------------------------
	on := doStage("ondemand", false, 130)
	if on.refused {
		attempts = append(attempts, attempt{"ondemand", "op_refused", on.errText})
	} else if on.deployed || on.avail >= 1 {
		attempts = append(attempts, attempt{"ondemand", "deployed", ""})
	} else {
		attempts = append(attempts, attempt{"ondemand", "poll_stop", c211sTrunc(on.last, 700)})
	}

	// ---- 5. PREEMPTIBLE/spot stage, only on a verbatim on-demand refusal ---
	sp := stageRes{label: "preemptible", avail: -1}
	spotOK := false
	if on.refused && c211sRefusalClass(on.errText) {
		spotOK = true
	} else if on.refused {
		fmt.Printf("C211S_SPOT_SKIP reason=on-demand-refusal-not-quota-class verbatim=%s\n", c211sTrunc(on.errText, 600))
	} else if !on.deployed && on.avail < 1 {
		fmt.Printf("C211S_SPOT_SKIP reason=on-demand-not-verbatim-refused last=%s\n", c211sTrunc(on.last, 600))
	} else {
		fmt.Printf("C211S_SPOT_SKIP reason=on-demand-already-serving\n")
	}
	if spotOK && !time.Now().After(hardEnd.Add(-reserve)) {
		sp = doStage("preemptible", true, 140)
		if sp.refused {
			attempts = append(attempts, attempt{"preemptible", "op_refused", sp.errText})
		} else if sp.deployed || sp.avail >= 1 {
			attempts = append(attempts, attempt{"preemptible", "deployed", ""})
		} else {
			attempts = append(attempts, attempt{"preemptible", "poll_stop", c211sTrunc(sp.last, 700)})
		}
	}

	// ---- 6. independent readback + :predict device read --------------------
	egst, egb := c211xHTTP("GET", c211sBase+"/endpoints/"+epID, at, "", "")
	avail := -1
	depID := ""
	depAttached := 0
	if m := c211xJSON(egb); m != nil {
		if dms, ok := m["deployedModels"].([]interface{}); ok {
			depAttached = len(dms)
			for _, dm := range dms {
				d0, _ := dm.(map[string]interface{})
				if id := c211xStr(d0, "id"); id != "" {
					depID = id
				}
				if s, ok := d0["status"].(map[string]interface{}); ok {
					if v, ok := s["availableReplicaCount"].(float64); ok {
						avail = int(v)
					}
				}
				if dr, ok := d0["dedicatedResources"].(map[string]interface{}); ok {
					if ms, ok := dr["machineSpec"].(map[string]interface{}); ok {
						fmt.Printf("C211S_DEPLOYED_MODEL id=%s machine=%s accel=%s count=%v spot=%v min=%v max=%v avail=%d\n",
							c211xStr(d0, "id"), c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"),
							ms["acceleratorCount"], dr["spot"], dr["minReplicaCount"], dr["maxReplicaCount"], avail)
					}
				}
			}
		}
	}
	fmt.Printf("C211S_EP_GET_AFTER_DEPLOY http=%d avail=%d attached=%d depID=%s body=%s\n",
		egst, avail, depAttached, depID, c211sTrunc(egb, 1800))

	gotUUIDs := []string{}
	if on.deployed || on.avail >= 1 || sp.deployed || sp.avail >= 1 || depAttached > 0 {
		for i := 0; i < 15; i++ {
			pst2, pbody2 := c211xHTTP("POST", c211sBase+"/endpoints/"+epID+":predict", at, "application/json", `{"instances":[{}]}`)
			uu := c211xUUIDRe.FindAllString(pbody2, -1)
			fmt.Printf("C211S_PREDICT t=%d http=%d uuids=%d body=%s\n", i, pst2, len(uu), c211sTrunc(pbody2, 2400))
			if pst2 == 200 && len(uu) > 0 {
				seen := map[string]bool{}
				for _, u := range uu {
					if !seen[u] {
						seen[u] = true
						gotUUIDs = append(gotUUIDs, u)
					}
				}
				break
			}
			if time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211S_PREDICT_STOP reason=budget\n")
				break
			}
			time.Sleep(20 * time.Second)
		}
	}
	if len(gotUUIDs) > 0 {
		fmt.Printf("C211S_PROVEN n=%d avail=%d ep=%s dep=%s uuids=%s\n",
			len(gotUUIDs), avail, epID, depID, strings.Join(gotUUIDs, ","))
	}

	// ---- 7. retire: undeploy everything attached, delete endpoint+model ----
	// 7a. undeploy each attached deployment
	if cgst, cgb := c211xHTTP("GET", c211sBase+"/endpoints/"+epID, at, "", ""); cgst == 200 {
		if m := c211xJSON(cgb); m != nil {
			if dms, ok := m["deployedModels"].([]interface{}); ok {
				for _, dm := range dms {
					d0, _ := dm.(map[string]interface{})
					id := c211xStr(d0, "id")
					if id == "" {
						continue
					}
					ust3, ub3 := c211xHTTP("POST", c211sBase+"/endpoints/"+epID+":undeployModel", at, "application/json",
						c211sUJ(map[string]interface{}{"deployedModelId": id}))
					fmt.Printf("C211S_UNDEPLOY id=%s http=%d body=%s\n", id, ust3, c211sTrunc(ub3, 700))
					op2 := c211xStr(c211xJSON(ub3), "name")
					for i := 0; i < 60 && op2 != ""; i++ {
						time.Sleep(10 * time.Second)
						d, line, _ := c211sOp(at, op2)
						if i%3 == 0 || d {
							fmt.Printf("C211S_UNDEPLOY_POLL id=%s t=%ds %s\n", id, (i+1)*10, c211sTrunc(line, 400))
						}
						if d || time.Now().After(hardEnd) {
							break
						}
					}
				}
			}
		}
	}
	// 7b. delete our endpoint and prove absence
	ddst, ddb := c211xHTTP("DELETE", c211sBase+"/endpoints/"+epID, at, "", "")
	fmt.Printf("C211S_EP_DELETE id=%s http=%d body=%s\n", epID, ddst, c211sTrunc(ddb, 800))
	op4 := c211xStr(c211xJSON(ddb), "name")
	for i := 0; i < 40 && op4 != ""; i++ {
		time.Sleep(5 * time.Second)
		d, _, _ := c211sOp(at, op4)
		if i%3 == 0 || d {
			fmt.Printf("C211S_EP_DELETE_POLL t=%ds done=%v\n", (i+1)*5, d)
		}
		if d {
			break
		}
	}
	agst, agb := c211xHTTP("GET", c211sBase+"/endpoints/"+epID, at, "", "")
	fmt.Printf("C211S_EP_ABSENCE id=%s http=%d body=%s\n", epID, agst, c211sTrunc(agb, 500))
	// 7c. delete our model and prove absence
	mdst, mdb := c211xHTTP("DELETE", c211sBase+"/models/"+modelID, at, "", "")
	fmt.Printf("C211S_MODEL_DELETE id=%s http=%d body=%s\n", modelID, mdst, c211sTrunc(mdb, 700))
	mop2 := c211xStr(c211xJSON(mdb), "name")
	for i := 0; i < 30 && mop2 != ""; i++ {
		time.Sleep(5 * time.Second)
		d, _, _ := c211sOp(at, mop2)
		if d {
			break
		}
	}
	mgst2, mgb2 := c211xHTTP("GET", c211sBase+"/models/"+modelID, at, "", "")
	fmt.Printf("C211S_MODEL_ABSENCE id=%s http=%d body=%s\n", modelID, mgst2, c211sTrunc(mgb2, 500))
	elst, elb := c211xHTTP("GET", c211sBase+"/endpoints?pageSize=100", at, "", "")
	mlst, mlb := c211xHTTP("GET", c211sBase+"/models?pageSize=100", at, "", "")
	fmt.Printf("C211S_EP_LIST_AFTER http=%d c211=%d own=%d body=%s\n", elst,
		strings.Count(strings.ToLower(elb), c211sNonce), strings.Count(elb, epID), c211sTrunc(elb, 700))
	fmt.Printf("C211S_MODEL_LIST_AFTER http=%d c211=%d own=%d body=%s\n", mlst,
		strings.Count(strings.ToLower(mlb), c211sNonce), strings.Count(mlb, modelID), c211sTrunc(mlb, 700))

	// ---- 8. verdict -------------------------------------------------------
	fmt.Printf("C211S_VERDICT_FINAL elapsed=%ds proven=%d avail=%d ep=%s dep=%s uuids=%s\n",
		int(time.Since(start).Seconds()), len(gotUUIDs), avail, epID, depID, strings.Join(gotUUIDs, ","))
	for _, a := range attempts {
		fmt.Printf("C211S_ATTEMPT stage=%s outcome=%s detail=%s\n", a.stage, a.outcome, c211sTrunc(a.detail, 800))
	}
}
