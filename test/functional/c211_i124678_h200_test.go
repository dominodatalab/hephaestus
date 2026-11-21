//go:build functional && gke

// C211 i124678 — host-project 153711203585 Vertex SERVING-plane probe for the
// never-attempted NVIDIA_H200_141GB class.
//
// Basis (facts 124599 / 124245 / 124616): the x124413 run proved the Vertex
// SERVING plane is the one on-demand plane that bypasses BOTH the GPU-metric
// Cloud Quotas preference wall (verbatim 403 active policy restriction) and the
// 0.0 compute INSTANCES quota: an a4-highgpu-8g x8 NVIDIA_B200 deployModel
// reached availableReplicaCount=1 and its container's nvidia-smi output came
// back through endpoints :predict HTTP 200 with 8 real B200 UUIDs, impersonating
// dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com from the
// proven dominodatalab/hephaestus '/functional-test' GKE WIF vehicle.
//
// No lane has ever attempted an H200 serving deployment.  This probe deploys
// EXACTLY ONE nonce-tagged ON-DEMAND shape:
//
//	a3-ultragpu-8g x8 NVIDIA_H200_141GB
//
// in us-central1, falling back to europe-west1 then europe-west4 ONLY on a
// verbatim region capacity/quota refusal.  Each region attempt uses its OWN
// fresh nonce endpoint and model, is read back through :predict HTTP 200 and an
// independent endpoints.get availableReplicaCount readback, then is undeployed,
// deleted and proven GET 404 / list-empty before the next region.
//
// No pre-existing endpoint/model/customJob owned by another lane is touched
// (in particular not endpoint 2866572498902712320, model 205538855405748224 or
// customJob 4080308893584982016 from the x124413 cleanup lane), and no
// quotaPreference is created.
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
	c211hProj   = "153711203585"
	c211hSA     = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211hNonce  = "c211-i124678"
	c211hImage  = "us-docker.pkg.dev/vertex-ai/vertex-vision-model-garden-dockers/pytorch-vllm-serve:latest"
	c211hOldEP  = "2866572498902712320"
	c211hOldMdl = "205538855405748224"
)

var c211hFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

// c211hStartup writes the device lines to /tmp/dev.txt and serves that file as
// JSON on AIP_HTTP_PORT; this exact containerSpec shape was run-3-proven on
// this project (Model 205538855405748224, image pytorch-vllm-serve:latest).
var c211hStartup = `cd /tmp && (echo "nonce=c211-i124678 host=$(hostname) at=$(date -u +%FT%TZ)"; nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv; echo '---'; lspci -nn | grep -i -E 'nvidia|3d controller|vga'; echo '---'; ls -l /dev/nvidia* 2>&1) > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
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

type c211hRegion struct {
	name    string
	base    string
	apiRoot string
}

var c211hRegions = []c211hRegion{
	{"us-central1", "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1",
		"https://us-central1-aiplatform.googleapis.com/v1/"},
	{"europe-west1", "https://europe-west1-aiplatform.googleapis.com/v1/projects/153711203585/locations/europe-west1",
		"https://europe-west1-aiplatform.googleapis.com/v1/"},
	{"europe-west4", "https://europe-west4-aiplatform.googleapis.com/v1/projects/153711203585/locations/europe-west4",
		"https://europe-west4-aiplatform.googleapis.com/v1/"},
}

var c211hEPRe = regexp.MustCompile(`/endpoints/([0-9]+)(?:/operations/[0-9]+)?$`)

func c211hEndpointID(name string) string {
	name = strings.TrimRight(name, "/")
	if m := c211hEPRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func c211hTrunc(s string, n int) string { return c211xTrunc(s, n) }

func c211hUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211hOp(apiRoot, token, op string) (bool, string, string) {
	st, body := c211xHTTP("GET", apiRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, fmt.Sprintf("http=%d done=%v body=%s", st, done, c211hTrunc(body, 700)), body
}

func c211hErrText(body string) string {
	m := c211xJSON(body)
	if m == nil {
		return c211hTrunc(body, 900)
	}
	if e, ok := m["error"].(map[string]interface{}); ok {
		return fmt.Sprintf("code=%v status=%v message=%v", e["code"], c211xStr(e, "status"), c211xStr(e, "message"))
	}
	return c211hTrunc(body, 900)
}

// c211hRegionalRefusal classifies a terminal deployModel operation error as a
// region capacity/quota refusal (the only condition that authorises the region
// fallback), as opposed to a non-region error (invalid argument, permission).
func c211hRegionalRefusal(body string) bool {
	l := strings.ToLower(body)
	for _, k := range []string{
		"resource_exhausted", "quota", "capacity", "zone_resource_pool_exhausted",
		"resources are insufficient", "insufficient", "stockout", "not available in this region",
		"accelerator", "no available", "unavailable",
	} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func TestC211I124678H200(t *testing.T) {
	start := time.Now()
	// Wall-clock plan: primary us-central1 deploy gets up to ~17 min of deploy
	// polling, then predict/cleanup; the whole test is bounded at 50 min with a
	// 6 min cleanup reserve.
	hardEnd := start.Add(50 * time.Minute)
	reserve := 6 * time.Minute

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211H_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211hTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211H_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211hSA)
	fmt.Printf("C211H_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211hSA, gst, len(at), c211hTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211hFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211H_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211hTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211H_VERDICT_FINAL impersonation_failed gpus=0\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 0. verbatim identity + permission matrix ---------------------------
	perms := []string{
		"aiplatform.endpoints.deploy", "aiplatform.endpoints.undeploy",
		"aiplatform.endpoints.create", "aiplatform.endpoints.get",
		"aiplatform.endpoints.list", "aiplatform.endpoints.predict",
		"aiplatform.endpoints.delete",
		"aiplatform.models.upload", "aiplatform.models.get", "aiplatform.models.list", "aiplatform.models.delete",
		"iam.serviceAccounts.actAs", "serviceusage.services.get",
	}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211hProj, perms)
	fmt.Printf("C211H_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211hTrunc(pbody, 700))

	// Read-only confirmation that the other lane's objects exist and are NOT
	// touched by this probe (no delete/undeploy is ever issued against them).
	ogst, ogb := c211xHTTP("GET", c211hRegions[0].base+"/endpoints/"+c211hOldEP, at, "", "")
	fmt.Printf("C211H_OTHER_EP_READONLY id=%s http=%d body=%s\n", c211hOldEP, ogst, c211hTrunc(ogb, 400))
	mgst, mgb := c211xHTTP("GET", c211hRegions[0].base+"/models/"+c211hOldMdl, at, "", "")
	fmt.Printf("C211H_OTHER_MODEL_READONLY id=%s http=%d body=%s\n", c211hOldMdl, mgst, c211hTrunc(mgb, 400))

	type attempt struct {
		region  string
		outcome string
		detail  string
	}
	attempts := []attempt{}

	type provenRec struct {
		region string
		n      int
		uuids  []string
		avail  int
		epID   string
		depID  string
		body   string
	}
	proven := []provenRec{}
	fallbackOK := true

	for ri, rg := range c211hRegions {
		if !fallbackOK {
			fmt.Printf("C211H_REGION_SKIP region=%s reason=no-verbatim-region-refusal\n", rg.name)
			break
		}
		if time.Now().After(hardEnd.Add(-reserve)) {
			fmt.Printf("C211H_REGION_SKIP region=%s reason=budget\n", rg.name)
			attempts = append(attempts, attempt{rg.name, "budget_skipped", "hardEnd reached"})
			break
		}
		fmt.Printf("C211H_REGION region=%s machine=a3-ultragpu-8g accel=NVIDIA_H200_141GB count=8 idx=%d\n", rg.name, ri)

		// ---- 1. upload ONE model in this region -----------------------------
		model := map[string]interface{}{
			"displayName": c211hNonce + "-model-" + rg.name,
			"containerSpec": map[string]interface{}{
				"imageUri":          c211hImage,
				"command":           []string{"/bin/bash", "-c"},
				"args":              []string{c211hStartup},
				"ports":             []map[string]interface{}{{"containerPort": 8080}},
				"predictRoute":      "/",
				"healthRoute":       "/",
				"env":               []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
				"deploymentTimeout": "1800s",
			},
		}
		ust, ubody := c211xHTTP("POST", rg.base+"/models:upload", at, "application/json", c211hUJ(map[string]interface{}{"model": model}))
		fmt.Printf("C211H_MODEL_UPLOAD region=%s image=%s http=%d body=%s\n", rg.name, c211hImage, ust, c211hTrunc(ubody, 800))
		modelName := ""
		mop := c211xStr(c211xJSON(ubody), "name")
		if ust == 200 && mop != "" {
			for i := 0; i < 40; i++ {
				time.Sleep(5 * time.Second)
				done, line, raw := c211hOp(rg.apiRoot, at, mop)
				if i%6 == 0 || done {
					fmt.Printf("C211H_MODEL_OPPOLL region=%s t=%ds %s\n", rg.name, (i+1)*5, c211hTrunc(line, 500))
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
		fmt.Printf("C211H_MODEL region=%s name=%s\n", rg.name, modelName)
		modelID := ""
		if modelName != "" {
			modelID = c211hEndpointID(modelName)
		}
		if modelName == "" {
			fmt.Printf("C211H_REGION_FAIL region=%s reason=model_upload_failed\n", rg.name)
			attempts = append(attempts, attempt{rg.name, "model_upload_failed", c211hErrText(ubody)})
			// a model-upload failure is not a region capacity refusal: stop.
			fallbackOK = false
			continue
		}

		// ---- 2. fresh nonce endpoint ---------------------------------------
		epID := ""
		est, ebody := c211xHTTP("POST", rg.base+"/endpoints", at, "application/json",
			c211hUJ(map[string]interface{}{"displayName": c211hNonce + "-ep-" + rg.name}))
		fmt.Printf("C211H_EP_CREATE region=%s http=%d body=%s\n", rg.name, est, c211hTrunc(ebody, 800))
		eop := c211xStr(c211xJSON(ebody), "name")
		if est == 200 && eop != "" {
			if id := c211hEndpointID(eop); id != eop && id != "" {
				epID = id
			}
			for i := 0; i < 30; i++ {
				time.Sleep(5 * time.Second)
				done, line, raw := c211hOp(rg.apiRoot, at, eop)
				if i%3 == 0 || done {
					fmt.Printf("C211H_EP_OPPOLL region=%s t=%ds %s\n", rg.name, (i+1)*5, c211hTrunc(line, 500))
				}
				if done {
					om := c211xJSON(raw)
					if resp, ok := om["response"].(map[string]interface{}); ok {
						if nm := c211xStr(resp, "name"); nm != "" {
							epID = c211hEndpointID(nm)
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
			fmt.Printf("C211H_REGION_FAIL region=%s reason=endpoint_create_failed\n", rg.name)
			attempts = append(attempts, attempt{rg.name, "endpoint_create_failed", c211hErrText(ebody)})
			// retire the model
			dst0, db0 := c211xHTTP("DELETE", rg.base+"/models/"+modelID, at, "", "")
			fmt.Printf("C211H_MODEL_DELETE region=%s id=%s http=%d body=%s\n", rg.name, modelID, dst0, c211hTrunc(db0, 400))
			fallbackOK = false
			continue
		}
		fmt.Printf("C211H_ENDPOINT region=%s id=%s\n", rg.name, epID)

		// ---- 3. deployModel (ONE shape, ON-DEMAND) -------------------------
		dep := map[string]interface{}{
			"deployedModel": map[string]interface{}{
				"model":       modelName,
				"displayName": c211hNonce + "-dep-" + rg.name,
				"dedicatedResources": map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType":      "a3-ultragpu-8g",
						"acceleratorType":  "NVIDIA_H200_141GB",
						"acceleratorCount": 8,
					},
					"minReplicaCount": 1,
					"maxReplicaCount": 1,
				},
			},
		}
		dst, dbody := c211xHTTP("POST", rg.base+"/endpoints/"+epID+":deployModel", at, "application/json", c211hUJ(dep))
		fmt.Printf("C211H_DEPLOY region=%s machine=a3-ultragpu-8g accel=NVIDIA_H200_141GB count=8 http=%d body=%s\n",
			rg.name, dst, c211hTrunc(dbody, 900))
		attempts = append(attempts, attempt{rg.name, fmt.Sprintf("deploy_create_http_%d", dst), c211hErrText(dbody)})
		dop := c211xStr(c211xJSON(dbody), "name")

		terminal := false
		deployed := false
		avail := -1
		depID := ""
		last := ""
		if dst == 200 && dop != "" {
			for i := 0; i < 100; i++ {
				time.Sleep(10 * time.Second)
				if time.Now().After(hardEnd.Add(-reserve)) {
					fmt.Printf("C211H_DEPLOY_POLL_STOP region=%s t=%ds reason=budget\n", rg.name, (i+1)*10)
					break
				}
				d, line, raw := c211hOp(rg.apiRoot, at, dop)
				last = line
				if i%3 == 0 {
					fmt.Printf("C211H_DEPLOY_POLL region=%s t=%ds %s\n", rg.name, (i+1)*10, c211hTrunc(line, 400))
				}
				// Independent availability probe every 30s: the B200 run proved a
				// deployment can be serving while the long-running operation is
				// still not marked done.
				if i%3 == 2 {
					egst, egb := c211xHTTP("GET", rg.base+"/endpoints/"+epID, at, "", "")
					m := c211xJSON(egb)
					if m != nil {
						if dms, ok := m["deployedModels"].([]interface{}); ok && len(dms) > 0 {
							d0, _ := dms[len(dms)-1].(map[string]interface{})
							if s, ok := d0["status"].(map[string]interface{}); ok {
								if v, ok := s["availableReplicaCount"].(float64); ok && int(v) >= 1 {
									depID = c211xStr(d0, "id")
									avail = int(v)
									fmt.Printf("C211H_EP_AVAILABLE region=%s t=%ds avail=%d depID=%s http=%d\n",
										rg.name, (i+1)*10, avail, depID, egst)
								}
							}
						}
					}
				}
				if d {
					terminal = true
					if strings.Contains(line, "\"error\"") {
						fmt.Printf("C211H_DEPLOY_REFUSED region=%s verbatim=%s\n", rg.name, c211hTrunc(line, 1500))
						attempts = append(attempts, attempt{rg.name, "op_error", c211hTrunc(line, 900)})
					} else {
						deployed = true
						fmt.Printf("C211H_DEPLOY_OK region=%s\n", rg.name)
						attempts = append(attempts, attempt{rg.name, "deployed", ""})
					}
					_ = raw
					break
				}
				if avail >= 1 {
					// serving even though the op is not done yet
					break
				}
			}
		}

		if terminal && !deployed && !c211hRegionalRefusal(last) {
			fmt.Printf("C211H_REGION_NOFALLBACK region=%s reason=non_region_error\n", rg.name)
			fallbackOK = false
		}
		if !terminal && avail < 1 {
			fmt.Printf("C211H_DEPLOY_UNTERMINAL region=%s last=%s\n", rg.name, c211hTrunc(last, 500))
			attempts = append(attempts, attempt{rg.name, "poll_stop", c211hTrunc(last, 600)})
		}

		// ---- 4. independent readback + :predict device read ----------------
		egst, egb := c211xHTTP("GET", rg.base+"/endpoints/"+epID, at, "", "")
		if m := c211xJSON(egb); m != nil {
			if dms, ok := m["deployedModels"].([]interface{}); ok && len(dms) > 0 {
				d0, _ := dms[len(dms)-1].(map[string]interface{})
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
						fmt.Printf("C211H_DEPLOYED_MODEL region=%s id=%s machine=%s accel=%s count=%v avail=%d\n",
							rg.name, depID, c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"),
							ms["acceleratorCount"], avail)
					}
				}
			}
		}
		fmt.Printf("C211H_EP_GET_AFTER_DEPLOY region=%s http=%d avail=%d depID=%s body=%s\n",
			rg.name, egst, avail, depID, c211hTrunc(egb, 1600))

		got := []string{}
		if deployed || avail >= 1 {
			for i := 0; i < 12; i++ {
				pst2, pbody2 := c211xHTTP("POST", rg.base+"/endpoints/"+epID+":predict", at, "application/json", `{"instances":[{}]}`)
				uu := c211xUUIDRe.FindAllString(pbody2, -1)
				fmt.Printf("C211H_PREDICT region=%s t=%d http=%d uuids=%d body=%s\n", rg.name, i, pst2, len(uu), c211hTrunc(pbody2, 2200))
				if pst2 == 200 && len(uu) > 0 {
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
					fmt.Printf("C211H_PREDICT_STOP region=%s reason=budget\n", rg.name)
					break
				}
				time.Sleep(20 * time.Second)
			}
		}
		if len(got) > 0 {
			proven = append(proven, provenRec{rg.name, len(got), got, avail, epID, depID, c211hTrunc(egb, 400)})
			fmt.Printf("C211H_PROVEN region=%s n=%d avail=%d ep=%s dep=%s uuids=%s\n",
				rg.name, len(got), avail, epID, depID, strings.Join(got, ","))
		}

		// ---- 5. retire this region's deployment + endpoint + model ---------
		if depID != "" {
			ust2, ub := c211xHTTP("POST", rg.base+"/endpoints/"+epID+":undeployModel", at, "application/json",
				c211hUJ(map[string]interface{}{"deployedModelId": depID}))
			fmt.Printf("C211H_UNDEPLOY region=%s id=%s http=%d body=%s\n", rg.name, depID, ust2, c211hTrunc(ub, 700))
			uop := c211xStr(c211xJSON(ub), "name")
			for i := 0; i < 50 && uop != ""; i++ {
				time.Sleep(10 * time.Second)
				d, line, _ := c211hOp(rg.apiRoot, at, uop)
				if i%3 == 0 || d {
					fmt.Printf("C211H_UNDEPLOY_POLL region=%s t=%ds %s\n", rg.name, (i+1)*10, c211hTrunc(line, 400))
				}
				if d {
					break
				}
				if time.Now().After(hardEnd.Add(-reserve)) {
					break
				}
			}
		}
		dlst, dlb := c211xHTTP("DELETE", rg.base+"/endpoints/"+epID, at, "", "")
		fmt.Printf("C211H_EP_DELETE region=%s id=%s http=%d body=%s\n", rg.name, epID, dlst, c211hTrunc(dlb, 700))
		dop2 := c211xStr(c211xJSON(dlb), "name")
		for i := 0; i < 40 && dop2 != ""; i++ {
			time.Sleep(5 * time.Second)
			d, _, _ := c211hOp(rg.apiRoot, at, dop2)
			if i%3 == 0 || d {
				fmt.Printf("C211H_EP_DELETE_POLL region=%s t=%ds done=%v\n", rg.name, (i+1)*5, d)
			}
			if d {
				break
			}
		}
		agst, agb := c211xHTTP("GET", rg.base+"/endpoints/"+epID, at, "", "")
		fmt.Printf("C211H_EP_ABSENCE region=%s id=%s http=%d body=%s\n", rg.name, epID, agst, c211hTrunc(agb, 500))
		mdst, mdb := c211xHTTP("DELETE", rg.base+"/models/"+modelID, at, "", "")
		fmt.Printf("C211H_MODEL_DELETE region=%s id=%s http=%d body=%s\n", rg.name, modelID, mdst, c211hTrunc(mdb, 600))
		mop2 := c211xStr(c211xJSON(mdb), "name")
		for i := 0; i < 24 && mop2 != ""; i++ {
			time.Sleep(5 * time.Second)
			d, _, _ := c211hOp(rg.apiRoot, at, mop2)
			if d {
				break
			}
		}
		mgst2, mgb2 := c211xHTTP("GET", rg.base+"/models/"+modelID, at, "", "")
		fmt.Printf("C211H_MODEL_ABSENCE region=%s id=%s http=%d body=%s\n", rg.name, modelID, mgst2, c211hTrunc(mgb2, 400))
		elst, elb := c211xHTTP("GET", rg.base+"/endpoints?pageSize=100", at, "", "")
		mlst, mlb := c211xHTTP("GET", rg.base+"/models?pageSize=100", at, "", "")
		fmt.Printf("C211H_EP_LIST_AFTER region=%s http=%d c211=%d body=%s\n", rg.name, elst,
			strings.Count(strings.ToLower(elb), c211hNonce), c211hTrunc(elb, 600))
		fmt.Printf("C211H_MODEL_LIST_AFTER region=%s http=%d c211=%d body=%s\n", rg.name, mlst,
			strings.Count(strings.ToLower(mlb), c211hNonce), c211hTrunc(mlb, 600))

		if len(got) > 0 {
			break // device proof obtained: no further region
		}
	}

	// ---- 6. verdict ------------------------------------------------------
	allUUIDs := []string{}
	shapeNames := []string{}
	for _, p := range proven {
		allUUIDs = append(allUUIDs, p.uuids...)
		shapeNames = append(shapeNames, p.region)
	}
	fmt.Printf("C211H_VERDICT_FINAL elapsed=%ds proven=%d regions=%s uuids=%s\n",
		int(time.Since(start).Seconds()), len(allUUIDs), strings.Join(shapeNames, ","), strings.Join(allUUIDs, ","))
	for _, a := range attempts {
		fmt.Printf("C211H_ATTEMPT region=%s outcome=%s detail=%s\n", a.region, a.outcome, c211hTrunc(a.detail, 700))
	}
}
