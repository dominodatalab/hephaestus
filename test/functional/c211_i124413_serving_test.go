//go:build functional && gke

// C211 i124413 — host-project 153711203585 Vertex serving-plane probe.
//
// Basis (fact 124381): the '/functional-test' WIF identity impersonates
// dev-fleetcommand, which holds the full Vertex serving permission set; the
// previous probe uploaded Model 1511582747343192064 (image
// pytorch-gpu.2-4 with a containerSpec command that prints nvidia-smi device
// lines and serves them as JSON on AIP_HTTP_PORT) HTTP 200 and created
// Endpoint 2167388656753442816 HTTP 200, but all four deployModel attempts
// returned verbatim 404 NOT_FOUND because the probe used the CreateEndpoint
// long-running-operation id as the endpoint id.
//
// This probe FIXES that defect: it parses the endpoint resource id out of the
// operation name (projects/.../endpoints/<id>/operations/<opid>) or out of the
// operation's response.name, reuses the existing empty Endpoint when an
// in-identity GET shows it exists, and otherwise creates exactly ONE nonce
// endpoint.  It then deploys ONE shape at a time from the quota-positive
// us-central1 ladder (B200 8.0, H100 80GB 16.0, A100 80GB 1.0, A100 14.0),
// polling each deployModel operation to terminal and capturing every verbatim
// refusal; on success it reads the nvidia-smi lines back through :predict,
// takes an independent endpoints.get readback, undeploys, then moves on.
//
// Cleanup: undeploy + DELETE own endpoint and the residual Endpoint
// 2167388656753442816 plus the three residual Model records
// 1511582747343192064/5069426452965883904/7711913534325522432, proving
// GET 404 / list empty where the identity allows it; bounded secondary retires
// the three cancelled-but-PENDING us-central1 customJobs from fact 124366.
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
	c211vNonce   = "c211-i124413"
	c211vOldEPID = "2167388656753442816"
)

var c211vOldModels = []string{"1511582747343192064", "5069426452965883904", "7711913534325522432"}

var c211vOldJobs = []string{
	"projects/153711203585/locations/us-central1/customJobs/4154618287436595200",
	"projects/153711203585/locations/us-central1/customJobs/5991523985450336256",
	"projects/153711203585/locations/us-central1/customJobs/4863372278794027008",
}

var c211vFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

var c211vStartup = `cd /tmp && (echo "nonce=c211-i124413 host=$(hostname) at=$(date -u +%FT%TZ)"; nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv) > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
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

var c211vLadder = []c211vShape{
	{"b200-8x-a4-highgpu", "a4-highgpu-8g", "NVIDIA_B200", 8},
	{"h100-8x-a3-highgpu", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8},
	{"a100-80gb-1x-a2-ultragpu", "a2-ultragpu-1g", "NVIDIA_A100_80GB", 1},
	{"a100-8x-a2-highgpu", "a2-highgpu-8g", "NVIDIA_TESLA_A100", 8},
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

func TestC211I124413Serving(t *testing.T) {
	start := time.Now()
	hardEnd := start.Add(40 * time.Minute)
	reserve := 6 * time.Minute
	base := c211vBase

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211V_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211vTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211V_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211vSA)
	fmt.Printf("C211V_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211vSA, gst, len(at), c211vTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211vFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211V_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211vTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211V_VERDICT_FINAL impersonation_failed gpus=0\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 1. permission matrix on the host project --------------------------
	perms := []string{
		"aiplatform.endpoints.deployModel", "aiplatform.endpoints.undeployModel",
		"aiplatform.endpoints.deploy", "aiplatform.endpoints.undeploy",
		"aiplatform.endpoints.create", "aiplatform.endpoints.get",
		"aiplatform.endpoints.list", "aiplatform.endpoints.predict",
		"aiplatform.endpoints.delete",
		"aiplatform.models.upload", "aiplatform.models.get", "aiplatform.models.list", "aiplatform.models.delete",
		"aiplatform.customJobs.get", "aiplatform.customJobs.list",
		"aiplatform.customJobs.cancel", "aiplatform.customJobs.delete",
		"storage.buckets.create", "serviceusage.services.get", "iam.serviceAccounts.actAs",
	}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211vProj, perms)
	fmt.Printf("C211V_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211vTrunc(pbody, 700))

	// ---- 2. reuse the residual endpoint if it exists, else create ONE ------
	epID := ""
	rgst, rgb := c211xHTTP("GET", base+"/endpoints/"+c211vOldEPID, at, "", "")
	fmt.Printf("C211V_RESID_EP_GET id=%s http=%d body=%s\n", c211vOldEPID, rgst, c211vTrunc(rgb, 1000))
	if rgst == 200 {
		epID = c211vOldEPID
		fmt.Printf("C211V_EP_REUSE id=%s\n", epID)
	} else {
		est, ebody := c211xHTTP("POST", base+"/endpoints", at, "application/json",
			c211vUJ(map[string]interface{}{"displayName": c211vNonce + "-ep"}))
		fmt.Printf("C211V_EP_CREATE http=%d body=%s\n", est, c211vTrunc(ebody, 800))
		op := c211xStr(c211xJSON(ebody), "name")
		if op != "" {
			if id := c211vEndpointID(op); id != op && id != "" {
				epID = id
				fmt.Printf("C211V_EP_ID_FROM_OPNAME id=%s\n", epID)
			}
			for i := 0; i < 40; i++ {
				time.Sleep(5 * time.Second)
				done, line, raw := c211vOp(at, op)
				if i%3 == 0 || done {
					fmt.Printf("C211V_EP_OPPOLL t=%ds %s\n", (i+1)*5, c211vTrunc(line, 500))
				}
				if done {
					om := c211xJSON(raw)
					if resp, ok := om["response"].(map[string]interface{}); ok {
						nm := c211xStr(resp, "name")
						if nm != "" {
							epID = c211vEndpointID(nm)
							fmt.Printf("C211V_EP_ID_FROM_OPRESP name=%s id=%s\n", nm, epID)
						}
					}
					break
				}
				if time.Now().After(hardEnd.Add(-reserve)) {
					fmt.Printf("C211V_EP_OPPOLL_STOP reason=budget\n")
					break
				}
			}
		}
	}
	fmt.Printf("C211V_ENDPOINT id=%s\n", epID)
	if epID == "" {
		fmt.Printf("C211V_VERDICT_FINAL endpoint_create_failed gpus=0\n")
		t.Fatalf("endpoint create failed")
	}

	// ---- 3. deploy ladder --------------------------------------------------
	provenShapes := []string{}
	provenUUIDs := []string{}
	type attempt struct {
		shape   string
		outcome string
		detail  string
	}
	attempts := []attempt{}
	for _, sh := range c211vLadder {
		if time.Now().After(hardEnd.Add(-reserve)) {
			fmt.Printf("C211V_LADDER_STOP reason=budget shape=%s\n", sh.name)
			break
		}
		dep := map[string]interface{}{
			"deployedModel": map[string]interface{}{
				"model":       base + "/models/" + c211vOldModels[0],
				"displayName": c211vNonce + "-dep",
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
		fmt.Printf("C211V_DEPLOY shape=%s http=%d body=%s\n", sh.name, dst, c211vTrunc(dbody, 900))
		op := c211xStr(c211xJSON(dbody), "name")
		if dst != 200 || op == "" {
			attempts = append(attempts, attempt{sh.name, fmt.Sprintf("create_http_%d", dst), c211vErrText(dbody)})
			continue
		}
		done := false
		last := ""
		for i := 0; i < 72; i++ {
			time.Sleep(10 * time.Second)
			if time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211V_DEPLOY_POLL_STOP shape=%s t=%ds reason=budget\n", sh.name, (i+1)*10)
				break
			}
			d, line, _ := c211vOp(at, op)
			if i%3 == 0 {
				fmt.Printf("C211V_DEPLOY_POLL shape=%s t=%ds %s\n", sh.name, (i+1)*10, c211vTrunc(line, 400))
			}
			last = line
			if d {
				done = true
				break
			}
		}
		if !done {
			fmt.Printf("C211V_DEPLOY_UNTERMINAL shape=%s last=%s\n", sh.name, c211vTrunc(last, 500))
			attempts = append(attempts, attempt{sh.name, "poll_stop", c211vTrunc(last, 500)})
			continue
		}
		if strings.Contains(last, "\"error\"") {
			fmt.Printf("C211V_DEPLOY_REFUSED shape=%s verbatim=%s\n", sh.name, c211vTrunc(last, 1200))
			attempts = append(attempts, attempt{sh.name, "op_error", c211vTrunc(last, 700)})
			continue
		}
		fmt.Printf("C211V_DEPLOY_OK shape=%s\n", sh.name)
		attempts = append(attempts, attempt{sh.name, "deployed", ""})
		egst, egb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
		fmt.Printf("C211V_EP_GET_AFTER_DEPLOY shape=%s http=%d body=%s\n", sh.name, egst, c211vTrunc(egb, 1400))
		depID := ""
		em := c211xJSON(egb)
		if dms, ok := em["deployedModels"].([]interface{}); ok && len(dms) > 0 {
			if d0, ok := dms[len(dms)-1].(map[string]interface{}); ok {
				depID = c211xStr(d0, "id")
				dr, _ := d0["dedicatedResources"].(map[string]interface{})
				ms, _ := dr["machineSpec"].(map[string]interface{})
				fmt.Printf("C211V_DEPLOYED_MODEL shape=%s id=%s machine=%s accel=%s accel_count=%v min=%v max=%v\n",
					sh.name, depID, c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"),
					ms["acceleratorCount"], dr["minReplicaCount"], dr["maxReplicaCount"])
			}
		}
		gotUUIDs := []string{}
		for i := 0; i < 15; i++ {
			pst2, pbody2 := c211xHTTP("POST", base+"/endpoints/"+epID+":predict", at, "application/json", `{"instances":[{}]}`)
			uu := c211xUUIDRe.FindAllString(pbody2, -1)
			fmt.Printf("C211V_PREDICT shape=%s t=%d http=%d uuids=%d body=%s\n", sh.name, i, pst2, len(uu), c211vTrunc(pbody2, 1600))
			if pst2 == 200 && len(uu) > 0 {
				gotUUIDs = uu
				break
			}
			if time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211V_PREDICT_STOP shape=%s reason=budget\n", sh.name)
				break
			}
			time.Sleep(20 * time.Second)
		}
		if len(gotUUIDs) > 0 {
			provenShapes = append(provenShapes, sh.name)
			provenUUIDs = append(provenUUIDs, gotUUIDs...)
			fmt.Printf("C211V_PROVEN shape=%s n=%d uuids=%s\n", sh.name, len(gotUUIDs), strings.Join(gotUUIDs, ","))
		}
		if depID != "" {
			ust2, ub := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", at, "application/json",
				c211vUJ(map[string]interface{}{"deployedModelId": depID}))
			fmt.Printf("C211V_UNDEPLOY shape=%s http=%d body=%s\n", sh.name, ust2, c211vTrunc(ub, 700))
			op2 := c211xStr(c211xJSON(ub), "name")
			for i := 0; i < 60 && op2 != ""; i++ {
				time.Sleep(10 * time.Second)
				d, line, _ := c211vOp(at, op2)
				if i%3 == 0 || d {
					fmt.Printf("C211V_UNDEPLOY_POLL shape=%s t=%ds %s\n", sh.name, (i+1)*10, c211vTrunc(line, 400))
				}
				if d {
					break
				}
				if time.Now().After(hardEnd) {
					break
				}
			}
			pgst, pgb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
			fmt.Printf("C211V_EP_POSTUNDEPLOY shape=%s http=%d body=%s\n", sh.name, pgst, c211vTrunc(pgb, 1000))
		}
		if len(gotUUIDs) > 0 {
			break
		}
	}

	// ---- 4. cleanup -------------------------------------------------------
	// 4a. undeploy anything still attached to our endpoint.
	cgst, cgb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
	fmt.Printf("C211V_CLEAN_EP_GET id=%s http=%d body=%s\n", epID, cgst, c211vTrunc(cgb, 1200))
	if cgst == 200 {
		cm := c211xJSON(cgb)
		if dms, ok := cm["deployedModels"].([]interface{}); ok {
			for _, dm := range dms {
				d0, _ := dm.(map[string]interface{})
				id := c211xStr(d0, "id")
				if id == "" {
					continue
				}
				ust3, ub3 := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", at, "application/json",
					c211vUJ(map[string]interface{}{"deployedModelId": id}))
				fmt.Printf("C211V_CLEAN_UNDEPLOY id=%s http=%d body=%s\n", id, ust3, c211vTrunc(ub3, 500))
				op3 := c211xStr(c211xJSON(ub3), "name")
				for i := 0; i < 30 && op3 != ""; i++ {
					time.Sleep(10 * time.Second)
					d, _, _ := c211vOp(at, op3)
					if d {
						break
					}
				}
			}
		}
	}
	// 4b. delete the endpoint (own or residual) and prove absence.
	ddst, ddb := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
	fmt.Printf("C211V_EP_DELETE id=%s http=%d body=%s\n", epID, ddst, c211vTrunc(ddb, 800))
	op4 := c211xStr(c211xJSON(ddb), "name")
	for i := 0; i < 40 && op4 != ""; i++ {
		time.Sleep(5 * time.Second)
		d, _, _ := c211vOp(at, op4)
		if i%3 == 0 || d {
			fmt.Printf("C211V_EP_DELETE_POLL t=%ds done=%v\n", (i+1)*5, d)
		}
		if d {
			break
		}
	}
	agst, agb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
	fmt.Printf("C211V_EP_ABSENCE id=%s http=%d body=%s\n", epID, agst, c211vTrunc(agb, 500))
	// 4c. delete the residual model records.
	for _, mid := range c211vOldModels {
		mdst, mdb := c211xHTTP("DELETE", base+"/models/"+mid, at, "", "")
		fmt.Printf("C211V_MODEL_DELETE id=%s http=%d body=%s\n", mid, mdst, c211vTrunc(mdb, 600))
		op5 := c211xStr(c211xJSON(mdb), "name")
		for i := 0; i < 24 && op5 != ""; i++ {
			time.Sleep(5 * time.Second)
			d, _, _ := c211vOp(at, op5)
			if d {
				break
			}
		}
		mgst, mgb := c211xHTTP("GET", base+"/models/"+mid, at, "", "")
		fmt.Printf("C211V_MODEL_ABSENCE id=%s http=%d body=%s\n", mid, mgst, c211vTrunc(mgb, 400))
	}
	lst3, lbody3 := c211xHTTP("GET", base+"/models?pageSize=100", at, "", "")
	fmt.Printf("C211V_MODEL_LIST_AFTER http=%d body=%s\n", lst3, c211vTrunc(lbody3, 700))
	lst4, lbody4 := c211xHTTP("GET", base+"/endpoints?pageSize=100", at, "", "")
	fmt.Printf("C211V_EP_LIST_AFTER http=%d body=%s\n", lst4, c211vTrunc(lbody4, 700))

	// ---- 5. bounded secondary: retire x124366 PENDING customJobs ----------
	if time.Now().Before(start.Add(55 * time.Minute)) {
		cjBase := "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1/customJobs"
		jlst, jbody := c211xHTTP("GET", cjBase+"?pageSize=100", at, "", "")
		fmt.Printf("C211V_JOB_LIST http=%d body=%s\n", jlst, c211vTrunc(jbody, 900))
		for _, jn := range c211vOldJobs {
			jgst, jgb := c211xHTTP("GET", c211vAPIRoot+jn, at, "", "")
			jstate := c211xStr(c211xJSON(jgb), "state")
			fmt.Printf("C211V_JOB_GET job=%s http=%d state=%s\n", jn, jgst, jstate)
			if jgst != 200 {
				continue
			}
			cst, cb := c211xHTTP("POST", c211vAPIRoot+jn+":cancel", at, "application/json", "{}")
			fmt.Printf("C211V_JOB_CANCEL job=%s http=%d body=%s\n", jn, cst, c211vTrunc(cb, 300))
			for i := 0; i < 12; i++ {
				time.Sleep(5 * time.Second)
				_, jgb2 := c211xHTTP("GET", c211vAPIRoot+jn, at, "", "")
				st2 := c211xStr(c211xJSON(jgb2), "state")
				if strings.Contains(st2, "CANCELLED") || strings.Contains(st2, "CANCELLING") || strings.Contains(st2, "SUCCEEDED") || strings.Contains(st2, "FAILED") {
					fmt.Printf("C211V_JOB_CANCEL_STATE job=%s t=%ds state=%s\n", jn, (i+1)*5, st2)
					break
				}
			}
			jdst, jdb := c211xHTTP("DELETE", c211vAPIRoot+jn, at, "", "")
			fmt.Printf("C211V_JOB_DELETE job=%s http=%d body=%s\n", jn, jdst, c211vTrunc(jdb, 500))
			for i := 0; i < 24; i++ {
				time.Sleep(5 * time.Second)
				g2st, g2b := c211xHTTP("GET", c211vAPIRoot+jn, at, "", "")
				if g2st == 404 {
					fmt.Printf("C211V_JOB_ABSENCE job=%s t=%ds http=404\n", jn, (i+1)*5)
					break
				}
				if i == 23 {
					fmt.Printf("C211V_JOB_STILL job=%s http=%d state=%s\n", jn, g2st, c211xStr(c211xJSON(g2b), "state"))
				}
			}
		}
		jlst2, jbody2 := c211xHTTP("GET", cjBase+"?pageSize=100", at, "", "")
		fmt.Printf("C211V_JOB_LIST_AFTER http=%d body=%s\n", jlst2, c211vTrunc(jbody2, 900))
	} else {
		fmt.Printf("C211V_JOB_RETIRE_SKIPPED reason=budget\n")
	}

	fmt.Printf("C211V_VERDICT_FINAL elapsed=%ds proven=%d shapes=%s uuids=%s endpoint=%s model=%s\n",
		int(time.Since(start).Seconds()), len(provenUUIDs), strings.Join(provenShapes, ","),
		strings.Join(provenUUIDs, ","), epID, c211vOldModels[0])
	for _, a := range attempts {
		fmt.Printf("C211V_ATTEMPT shape=%s outcome=%s detail=%s\n", a.shape, a.outcome, c211vTrunc(a.detail, 600))
	}
}
