//go:build functional && gke

// C211 i124677 — extend the PROVEN host Vertex serving plane to the four Domino
// sibling projects.
//
// Basis:
//   - fact 124599: in host project 153711203585 the a4-highgpu-8g x8
//     NVIDIA_B200 deployModel reached availableReplicaCount=1 and the model
//     container's /tmp/dev.txt came back through endpoints :predict HTTP 200
//     carrying 8 real NVIDIA B200 UUIDs (183359 MiB, driver 580.178.04).
//   - fact 124650: the exact cross-project identity
//     fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com
//     (len 76, sha256 9736b01aaa62f06b) now operates in all four siblings with
//     serviceusage.services.enable allowed (cloudquotas enabled + roles
//     granted in every sibling by that run).
//
// This probe, running inside the proven '/functional-test' GKE WIF vehicle:
//  1. mints the WIF token, impersonates the exact fleet SA (never a substring
//     match — dev-fleetcommand@ sorts first and only reaches the host);
//  2. per sibling project, in order 210628133505 / 1072394045651 /
//     463640210929 / 1074386228880:
//     a. reads aiplatform.googleapis.com state and enables it additively if
//     DISABLED (recording the state transition verbatim);
//     b. testIamPermissions for aiplatform.endpoints.create/get/delete,
//     aiplatform.models.upload/get/delete, the deploy/undeploy
//     permissions behind deployModel/undeployModel, and the literal
//     deployModel/undeployModel spellings (recorded verbatim);
//     c. uploads ONE nonce model (pytorch-vllm-serve, the run-3-proven
//     image) whose container writes nvidia-smi -L,
//     --query-gpu=name,uuid,memory.total,driver_version and lspci -nn to
//     /tmp/dev.txt and serves it on AIP_HTTP_PORT;
//     d. walks the mandated ON-DEMAND serving ladder, ONE shape at a time,
//     each on its own fresh nonce endpoint:
//     a4-highgpu-8g      x8 NVIDIA_B200
//     a3-highgpu-8g      x8 NVIDIA_H100_80GB
//     a2-ultragpu-1g     x1 NVIDIA_A100_80GB
//     polling the deployModel operation AND endpoints.get; on a live
//     deployment it reads the device file back through endpoints
//     :predict HTTP 200 and derives every UUID verbatim from that
//     per-deployment file (unique UUIDs + GPU lines counted separately,
//     never a probe-side summary), and takes an independent
//     endpoints.get availableReplicaCount readback;
//     e. undeploys, deletes the endpoint and proves GET 404 before the next
//     shape (a non-terminal deploy operation is bounded: cancel attempt
//     then delete retry; a residual is recorded verbatim and the ladder
//     for that project stops);
//     f. deletes the model and reads the endpoint/model lists back.
//
// No quotaPreference is created. No other lane's endpoint/model/customJob is
// touched (in particular host residual endpoint 2866572498902712320, model
// 205538855405748224 and customJob 4080308893584982016 are never addressed
// here — this probe only ever addresses sibling-project resources it created).
package functional

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	c211sNonce     = "c211-i124677"
	c211sRegion    = "us-central1"
	c211sHostProj  = "153711203585"
	c211sFleetSA   = "fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211sAPIRoot   = "https://us-central1-aiplatform.googleapis.com/v1/"
	c211sLocalDate = "2026-11-21"
)

type c211sProject struct{ Name, Num string }

var c211sProjects = []c211sProject{
	{"team-eng-infra-testing", "210628133505"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

type c211sShape struct {
	name    string
	machine string
	accel   string
	count   int
	wait    time.Duration
}

var c211sLadder = []c211sShape{
	{"b200-8x-a4-highgpu", "a4-highgpu-8g", "NVIDIA_B200", 8, 390 * time.Second},
	{"h100-8x-a3-highgpu", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8, 200 * time.Second},
	{"a100-80gb-1x-a2-ultragpu", "a2-ultragpu-1g", "NVIDIA_A100_80GB", 1, 210 * time.Second},
}

// c211sPerms are the IAM permission identifiers behind the mandated actions.
var c211sPerms = []string{
	"aiplatform.endpoints.create",
	"aiplatform.endpoints.get",
	"aiplatform.endpoints.delete",
	"aiplatform.endpoints.list",
	"aiplatform.endpoints.deploy",
	"aiplatform.endpoints.undeploy",
	"aiplatform.endpoints.predict",
	"aiplatform.models.upload",
	"aiplatform.models.get",
	"aiplatform.models.delete",
	"aiplatform.models.list",
	"iam.serviceAccounts.actAs",
	"serviceusage.services.get",
	"serviceusage.services.enable",
}

// c211sPermsLiteral records the verbatim API treatment of the mandate's
// deployModel/undeployModel spellings (real IAM ids are deploy/undeploy).
var c211sPermsLiteral = []string{
	"aiplatform.endpoints.deployModel",
	"aiplatform.endpoints.undeployModel",
}

type c211sShapeResult struct {
	Shape            string      `json:"shape"`
	Machine          string      `json:"machine"`
	Accel            string      `json:"accel"`
	Count            int         `json:"count"`
	DeployHTTP       int         `json:"deploy_http"`
	DeployBody       string      `json:"deploy_body"`
	OpName           string      `json:"op_name"`
	OpDone           bool        `json:"op_done"`
	OpError          string      `json:"op_error"`
	DeployedID       string      `json:"deployed_model_id"`
	MachineBack      string      `json:"machine_readback"`
	AccelBack        string      `json:"accel_readback"`
	CountBack        interface{} `json:"accel_count_readback"`
	AvailableReplica interface{} `json:"available_replica_count"`
	PredictHTTP      int         `json:"predict_http"`
	PredictBody      string      `json:"predict_body"`
	GPULines         int         `json:"gpu_lines"`
	UUIDs            []string    `json:"uuids"`
	Proven           bool        `json:"proven"`
	UndeployHTTP     int         `json:"undeploy_http"`
	EndpointDelHTTP  int         `json:"endpoint_delete_http"`
	EndpointAbsence  int         `json:"endpoint_absence_http"`
	Residual         string      `json:"residual"`
	ElapsedS         int         `json:"elapsed_s"`
}

type c211sProjResult struct {
	Proj          string             `json:"proj"`
	Num           string             `json:"num"`
	ProjectState  string             `json:"project_state"`
	ProjectHTTP   int                `json:"project_http"`
	ServiceBefore string             `json:"service_before"`
	ServiceHTTP   int                `json:"service_http"`
	EnableHTTP    int                `json:"enable_http"`
	EnableDetail  string             `json:"enable_detail"`
	ServiceAfter  string             `json:"service_after"`
	Perms         []string           `json:"perms_allowed"`
	PermsHTTP     int                `json:"perms_http"`
	PermsRaw      string             `json:"perms_raw"`
	PermsLit      []string           `json:"perms_literal_allowed"`
	PermsLitHTTP  int                `json:"perms_literal_http"`
	PermsLitRaw   string             `json:"perms_literal_raw"`
	ModelHTTP     int                `json:"model_upload_http"`
	ModelBody     string             `json:"model_upload_body"`
	ModelName     string             `json:"model_name"`
	Shapes        []c211sShapeResult `json:"shapes"`
	EndpointList  string             `json:"endpoints_list_after"`
	ModelDelHTTP  int                `json:"model_delete_http"`
	ModelAbsence  int                `json:"model_absence_http"`
	ModelList     string             `json:"models_list_after"`
	Verdict       string             `json:"verdict"`
}

var c211sEPRe = regexp.MustCompile(`/endpoints/([0-9]+)(?:/operations/[0-9]+)?$`)
var c211sGPULineRe = regexp.MustCompile(`(?m)^GPU [0-9]+: .*\(UUID: (GPU-[0-9a-fA-F-]+)\)`)

func c211sEndpointID(name string) string {
	name = strings.TrimRight(name, "/")
	if m := c211sEPRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func c211sBase(num string) string {
	return "https://" + c211sRegion + "-aiplatform.googleapis.com/v1/projects/" + num + "/locations/" + c211sRegion
}

func c211sUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211sErrText(body string) string {
	m := c211xJSON(body)
	if m == nil {
		return c211xTrunc(body, 900)
	}
	if e, ok := m["error"].(map[string]interface{}); ok {
		return fmt.Sprintf("code=%v status=%v message=%v", e["code"], c211xStr(e, "status"), c211xStr(e, "message"))
	}
	return c211xTrunc(body, 900)
}

// c211sOp polls a long-running operation by its resource name.
func c211sOp(at, op string) (bool, string, string) {
	st, body := c211xHTTP("GET", c211sAPIRoot+op, at, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, fmt.Sprintf("http=%d done=%v body=%s", st, done, c211xTrunc(body, 700)), body
}

// c211sWaitOp polls until done or until the wall-clock deadline.
func c211sWaitOp(at, op string, deadline time.Time, step time.Duration) (bool, string, string) {
	var last, raw string
	for {
		done, line, body := c211sOp(at, op)
		last, raw = line, body
		if done || time.Now().After(deadline) {
			return done, last, raw
		}
		time.Sleep(step)
	}
}

// c211sDevFile parses the container's /tmp/dev.txt (served as predictions[0])
// and returns the ordered unique UUIDs and the nvidia-smi -L GPU line count.
func c211sDevFile(predictBody string) ([]string, int, string) {
	m := c211xJSON(predictBody)
	dev := ""
	if m != nil {
		if preds, ok := m["predictions"].([]interface{}); ok && len(preds) > 0 {
			if s, ok := preds[0].(string); ok {
				dev = s
			}
		}
	}
	if dev == "" {
		dev = predictBody
	}
	seen := map[string]bool{}
	uuids := []string{}
	for _, u := range c211xUUIDRe.FindAllString(dev, -1) {
		if !seen[u] {
			seen[u] = true
			uuids = append(uuids, u)
		}
	}
	gl := len(c211sGPULineRe.FindAllStringSubmatch(dev, -1))
	return uuids, gl, dev
}

func c211sStartup(num string) string {
	return `cd /tmp && (echo "nonce=` + c211sNonce + ` proj=` + num + ` host=$(hostname) at=$(date -u +%FT%TZ)"; nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv; echo '---lspci---'; lspci -nn 2>/dev/null | grep -i -E 'nvidia|3d controller|vga') > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
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
}

// c211sRetireShape undeploys anything attached to our endpoint, deletes the
// endpoint, and proves GET 404. A non-terminal deploy operation is bounded by
// one operation cancel attempt and a delete retry.
func c211sRetireShape(at, base, epID string, res *c211sShapeResult) {
	if epID == "" {
		return
	}
	gst, gbody := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
	if gst == 200 {
		em := c211xJSON(gbody)
		if dms, ok := em["deployedModels"].([]interface{}); ok {
			for _, dm := range dms {
				d0, _ := dm.(map[string]interface{})
				id := c211xStr(d0, "id")
				if id == "" {
					continue
				}
				ust, ub := c211xHTTP("POST", base+"/endpoints/"+epID+":undeployModel", at, "application/json",
					c211sUJ(map[string]interface{}{"deployedModelId": id}))
				res.UndeployHTTP = ust
				fmt.Printf("C211S_UNDEPLOY proj_ep=%s id=%s http=%d body=%s\n", epID, id, ust, c211xTrunc(ub, 500))
				op := c211xStr(c211xJSON(ub), "name")
				if op != "" {
					c211sWaitOp(at, op, time.Now().Add(3*time.Minute), 10*time.Second)
				}
			}
		}
	}
	dst, db := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
	res.EndpointDelHTTP = dst
	fmt.Printf("C211S_EP_DELETE id=%s http=%d body=%s\n", epID, dst, c211xTrunc(db, 600))
	op := c211xStr(c211xJSON(db), "name")
	if op != "" {
		c211sWaitOp(at, op, time.Now().Add(2*time.Minute), 5*time.Second)
	}
	// blocked by a running operation? capture its name, cancel once, retry.
	if dst != 200 {
		if mm := regexp.MustCompile(`operations/([0-9]+)`).FindStringSubmatch(db); mm != nil {
			opName := "projects/" + strings.Split(base, "/projects/")[1] + "/operations/" + mm[1]
			cst, cb := c211xHTTP("POST", c211sAPIRoot+opName+":cancel", at, "application/json", "{}")
			fmt.Printf("C211S_OP_CANCEL op=%s http=%d body=%s\n", opName, cst, c211xTrunc(cb, 500))
			for i := 0; i < 12; i++ {
				time.Sleep(10 * time.Second)
				done, _, _ := c211sOp(at, opName)
				if done {
					break
				}
			}
			dst2, db2 := c211xHTTP("DELETE", base+"/endpoints/"+epID, at, "", "")
			res.EndpointDelHTTP = dst2
			fmt.Printf("C211S_EP_DELETE_RETRY id=%s http=%d body=%s\n", epID, dst2, c211xTrunc(db2, 600))
			op2 := c211xStr(c211xJSON(db2), "name")
			if op2 != "" {
				c211sWaitOp(at, op2, time.Now().Add(2*time.Minute), 5*time.Second)
			}
		}
	}
	agst, agb := c211xHTTP("GET", base+"/endpoints/"+epID, at, "", "")
	res.EndpointAbsence = agst
	fmt.Printf("C211S_EP_ABSENCE id=%s http=%d body=%s\n", epID, agst, c211xTrunc(agb, 400))
	if agst != 404 {
		res.Residual = "endpoint " + epID + " still present http=" + fmt.Sprint(agst) + " body=" + c211xTrunc(agb, 400)
	}
}

func TestC211I124677SiblingServing(t *testing.T) {
	start := time.Now()
	hardEnd := start.Add(56 * time.Minute)
	reserve := 3 * time.Minute

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211S_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211xTrunc(strings.ReplaceAll(mlog, "\n", " | "), 700))
	if tok == "" {
		fmt.Printf("C211S_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}

	// ---- exact cross-project fleet SA (never a substring match) -------------
	sas, sst, sbody := c211xListServiceAccounts(tok, c211sHostProj)
	fmt.Printf("C211S_SA_LIST host_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := c211xStr(a, "email")
		if strings.Contains(strings.ToLower(em), "fleet") {
			sum := sha256Sum(em)
			fmt.Printf("C211S_SA_CAND email_len=%d email_sha256=%s\n", len(em), sum[:16])
		}
		if em == c211sFleetSA {
			fleet = em
			break
		}
	}
	if fleet == "" {
		fmt.Printf("C211S_VERDICT_FINAL fleet_sa_absent verbatim=%s\n", c211xTrunc(sbody, 400))
		t.Fatalf("fleet SA absent")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211S_FLEET email_len=%d email_sha256=%s http=%d token_len=%d verbatim=%s\n",
		len(fleet), sha256Sum(fleet)[:16], fst, len(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		fmt.Printf("C211S_VERDICT_FINAL fleet_impersonation_failed gpus=0\n")
		t.Fatalf("fleet impersonation failed")
	}

	results := []*c211sProjResult{}
	provenTotal := 0
	for pi, p := range c211sProjects {
		if time.Now().After(hardEnd.Add(-reserve)) {
			fmt.Printf("C211S_PROJECTS_STOP reason=budget at=%s\n", p.Name)
			break
		}
		projStart := time.Now()
		projDeadline := projStart.Add(13 * time.Minute)
		if projDeadline.After(hardEnd.Add(-reserve)) {
			projDeadline = hardEnd.Add(-reserve)
		}
		res := &c211sProjResult{Proj: p.Name, Num: p.Num, Shapes: []c211sShapeResult{}}
		results = append(results, res)
		fmt.Printf("C211S_PROJ_BEGIN idx=%d proj=%s num=%s deadline=%s\n", pi, p.Name, p.Num, projDeadline.UTC().Format(time.RFC3339))

		// ---- a. project + aiplatform service state (enable if DISABLED) ----
		projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + p.Num
		pst, pbody := c211xHTTP("GET", projURL, ftok, "", "")
		res.ProjectHTTP = pst
		res.ProjectState = c211xStr(c211xJSON(pbody), "state")
		fmt.Printf("C211S_PROJECT proj=%s http=%d state=%s verbatim=%s\n", p.Name, pst, res.ProjectState, c211xTrunc(pbody, 300))
		if pst != 200 {
			res.Verdict = "project_get_refused"
			continue
		}
		svcURL := "https://serviceusage.googleapis.com/v1/projects/" + p.Num + "/services/aiplatform.googleapis.com"
		sst2, sb2 := c211xHTTP("GET", svcURL, ftok, "", "")
		res.ServiceHTTP = sst2
		res.ServiceBefore = c211xStr(c211xJSON(sb2), "state")
		fmt.Printf("C211S_SVC_BEFORE proj=%s http=%d state=%s verbatim=%s\n", p.Name, sst2, res.ServiceBefore, c211xTrunc(sb2, 300))
		res.ServiceAfter = res.ServiceBefore
		if res.ServiceBefore == "DISABLED" {
			est, eb := c211xHTTP("POST", svcURL+":enable", ftok, "application/json", "{}")
			res.EnableHTTP = est
			res.EnableDetail = c211xTrunc(eb, 700)
			fmt.Printf("C211S_ENABLE proj=%s http=%d verbatim=%s\n", p.Name, est, res.EnableDetail)
			op := c211xStr(c211xJSON(eb), "name")
			if op != "" {
				done, line, raw := c211sWaitOp(ftok, op, time.Now().Add(2*time.Minute), 5*time.Second)
				fmt.Printf("C211S_ENABLE_POLL proj=%s done=%v line=%s\n", p.Name, done, c211xTrunc(line, 400))
				_ = raw
			}
			sst3, sb3 := c211xHTTP("GET", svcURL, ftok, "", "")
			res.ServiceAfter = c211xStr(c211xJSON(sb3), "state")
			fmt.Printf("C211S_SVC_AFTER proj=%s http=%d state=%s verbatim=%s\n", p.Name, sst3, res.ServiceAfter, c211xTrunc(sb3, 300))
		}
		if res.ServiceAfter != "ENABLED" {
			res.Verdict = "aiplatform_service_not_enabled"
			continue
		}

		// ---- b. permission matrix ------------------------------------------
		al, pal, pab := c211xTestIamBatch(ftok, projURL, c211sPerms)
		res.Perms = al
		res.PermsHTTP = pal
		res.PermsRaw = c211xTrunc(pab, 700)
		fmt.Printf("C211S_PERMS proj=%s http=%d allowed=%v raw=%s\n", p.Name, pal, al, res.PermsRaw)
		all, lal, lab := c211xTestIamBatch(ftok, projURL, c211sPermsLiteral)
		res.PermsLit = all
		res.PermsLitHTTP = lal
		res.PermsLitRaw = c211xTrunc(lab, 700)
		fmt.Printf("C211S_PERMS_LITERAL proj=%s http=%d allowed=%v raw=%s\n", p.Name, lal, all, res.PermsLitRaw)

		permSet := map[string]bool{}
		for _, a := range al {
			permSet[a] = true
		}
		if !permSet["aiplatform.models.upload"] || !permSet["aiplatform.endpoints.create"] ||
			!permSet["aiplatform.endpoints.deploy"] {
			res.Verdict = "serving_permissions_missing"
			continue
		}

		base := c211sBase(p.Num)

		// ---- c. ONE model upload (run-3-proven image) ----------------------
		model := map[string]interface{}{
			"displayName": c211sNonce + "-model",
			"containerSpec": map[string]interface{}{
				"imageUri":          "us-docker.pkg.dev/vertex-ai/vertex-vision-model-garden-dockers/pytorch-vllm-serve:latest",
				"command":           []string{"/bin/bash", "-c"},
				"args":              []string{c211sStartup(p.Num)},
				"ports":             []map[string]interface{}{{"containerPort": 8080}},
				"predictRoute":      "/",
				"healthRoute":       "/",
				"env":               []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
				"deploymentTimeout": "1800s",
			},
		}
		ust, ub := c211xHTTP("POST", base+"/models:upload", ftok, "application/json", c211sUJ(map[string]interface{}{"model": model}))
		res.ModelHTTP = ust
		res.ModelBody = c211xTrunc(ub, 800)
		fmt.Printf("C211S_MODEL_UPLOAD proj=%s http=%d body=%s\n", p.Name, ust, res.ModelBody)
		modelName := ""
		if ust == 200 {
			op := c211xStr(c211xJSON(ub), "name")
			if op != "" {
				done, _, raw := c211sWaitOp(ftok, op, time.Now().Add(2*time.Minute), 5*time.Second)
				if done {
					om := c211xJSON(raw)
					if resp, ok := om["response"].(map[string]interface{}); ok {
						mn := c211xStr(resp, "model")
						if mn == "" {
							mn = c211xStr(resp, "name")
						}
						if i := strings.Index(mn, "/projects/"); i >= 0 {
							mn = mn[i+1:]
						}
						modelName = mn
					}
				}
			}
		}
		res.ModelName = modelName
		fmt.Printf("C211S_MODEL proj=%s name=%s\n", p.Name, modelName)
		if modelName == "" {
			res.Verdict = "model_upload_failed"
			continue
		}

		// ---- d. mandated ladder, one shape at a time ----------------------
		for _, sh := range c211sLadder {
			if time.Now().After(projDeadline) || time.Now().After(hardEnd.Add(-reserve)) {
				fmt.Printf("C211S_LADDER_STOP proj=%s shape=%s reason=budget\n", p.Name, sh.name)
				break
			}
			sr := c211sShapeResult{Shape: sh.name, Machine: sh.machine, Accel: sh.accel, Count: sh.count}
			shapeStart := time.Now()
			shapeDeadline := shapeStart.Add(sh.wait)
			if shapeDeadline.After(projDeadline) {
				shapeDeadline = projDeadline
			}

			est, eb := c211xHTTP("POST", base+"/endpoints", ftok, "application/json",
				c211sUJ(map[string]interface{}{"displayName": c211sNonce + "-" + sanitizeTag(sh.name) + "-ep"}))
			fmt.Printf("C211S_EP_CREATE proj=%s shape=%s http=%d body=%s\n", p.Name, sh.name, est, c211xTrunc(eb, 700))
			epID := ""
			if est == 200 {
				op := c211xStr(c211xJSON(eb), "name")
				if strings.Contains(op, "/endpoints/") {
					if id := c211sEndpointID(op); id != "" && id != op {
						epID = id
					}
				}
				if op != "" {
					done, _, raw := c211sWaitOp(ftok, op, time.Now().Add(90*time.Second), 5*time.Second)
					if done {
						if resp, ok := c211xJSON(raw)["response"].(map[string]interface{}); ok {
							if nm := c211xStr(resp, "name"); nm != "" {
								epID = c211sEndpointID(nm)
							}
						}
					}
				}
			}
			fmt.Printf("C211S_ENDPOINT proj=%s shape=%s id=%s\n", p.Name, sh.name, epID)
			if epID == "" {
				sr.Residual = "endpoint_create_failed"
				res.Shapes = append(res.Shapes, sr)
				continue
			}
			dep := map[string]interface{}{
				"deployedModel": map[string]interface{}{
					"model":       modelName,
					"displayName": c211sNonce + "-" + sanitizeTag(sh.name) + "-dep",
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
			dst, db := c211xHTTP("POST", base+"/endpoints/"+epID+":deployModel", ftok, "application/json", c211sUJ(dep))
			sr.DeployHTTP = dst
			sr.DeployBody = c211xTrunc(db, 900)
			fmt.Printf("C211S_DEPLOY proj=%s shape=%s http=%d body=%s\n", p.Name, sh.name, dst, sr.DeployBody)
			op := c211xStr(c211xJSON(db), "name")
			sr.OpName = op
			if dst == 200 && op != "" {
				attached := ""
				lastOpLine := ""
				for time.Now().Before(shapeDeadline) && time.Now().Before(hardEnd.Add(-reserve)) {
					time.Sleep(15 * time.Second)
					done, line, raw := c211sOp(ftok, op)
					lastOpLine = line
					if done {
						sr.OpDone = true
						if om := c211xJSON(raw); om != nil {
							if _, ok := om["error"]; ok {
								sr.OpError = c211sErrText(raw)
							}
						}
					}
					egst, egb := c211xHTTP("GET", base+"/endpoints/"+epID, ftok, "", "")
					attached = ""
					if egst == 200 {
						em := c211xJSON(egb)
						if dms, ok := em["deployedModels"].([]interface{}); ok && len(dms) > 0 {
							if d0, ok := dms[len(dms)-1].(map[string]interface{}); ok {
								attached = c211xStr(d0, "id")
								sr.DeployedID = attached
								if dr, ok := d0["dedicatedResources"].(map[string]interface{}); ok {
									if ms, ok := dr["machineSpec"].(map[string]interface{}); ok {
										sr.MachineBack = c211xStr(ms, "machineType")
										sr.AccelBack = c211xStr(ms, "acceleratorType")
										sr.CountBack = ms["acceleratorCount"]
									}
								}
								if stt, ok := d0["status"].(map[string]interface{}); ok {
									sr.AvailableReplica = stt["availableReplicaCount"]
								}
							}
						}
					}
					if attached != "" {
						pst, pb := c211xHTTP("POST", base+"/endpoints/"+epID+":predict", ftok, "application/json", `{"instances":[{}]}`)
						sr.PredictHTTP = pst
						uuids, gl, dev := c211sDevFile(pb)
						sr.PredictBody = c211xTrunc(pb, 2400)
						fmt.Printf("C211S_PREDICT proj=%s shape=%s http=%d attached=%s uuids=%d gpu_lines=%d body=%s\n",
							p.Name, sh.name, pst, attached, len(uuids), gl, c211xTrunc(pb, 3000))
						if pst == 200 && len(uuids) > 0 {
							sr.UUIDs = uuids
							sr.GPULines = gl
							sr.Proven = true
							fmt.Printf("C211S_DEVFILE proj=%s shape=%s gpus=%d uuids=%s dev=%s\n",
								p.Name, sh.name, len(uuids), strings.Join(uuids, ","), c211xTrunc(dev, 4000))
							fmt.Printf("C211S_READBACK proj=%s shape=%s deployed_model_id=%s machine=%s accel=%s count=%v availableReplicaCount=%v\n",
								p.Name, sh.name, sr.DeployedID, sr.MachineBack, sr.AccelBack, sr.CountBack, sr.AvailableReplica)
							break
						}
					}
					if sr.OpDone && sr.OpError != "" {
						fmt.Printf("C211S_DEPLOY_REFUSED proj=%s shape=%s verbatim=%s\n", p.Name, sh.name, sr.OpError)
						break
					}
				}
				if !sr.Proven && !sr.OpDone {
					fmt.Printf("C211S_DEPLOY_UNTERMINAL proj=%s shape=%s last=%s\n", p.Name, sh.name, c211xTrunc(lastOpLine, 400))
				}
			}
			sr.ElapsedS = int(time.Since(shapeStart).Seconds())
			c211sRetireShape(ftok, base, epID, &sr)
			res.Shapes = append(res.Shapes, sr)
			fmt.Printf("C211S_SHAPE_DONE proj=%s shape=%s proven=%v uuids=%d elapsed=%ds residual=%q\n",
				p.Name, sh.name, sr.Proven, len(sr.UUIDs), sr.ElapsedS, c211xTrunc(sr.Residual, 200))
			if sr.Proven {
				provenTotal++
				break // one proven shape per project is the deliverable
			}
			if sr.Residual != "" {
				// cannot prove list-empty before the next shape -> stop the ladder
				fmt.Printf("C211S_LADDER_STOP proj=%s shape=%s reason=residual\n", p.Name, sh.name)
				break
			}
		}

		// ---- e/f. cleanup + lists ------------------------------------------
		mdst, mdb := c211xHTTP("DELETE", base+"/models/"+c211sEndpointID(modelName), ftok, "", "")
		res.ModelDelHTTP = mdst
		op := c211xStr(c211xJSON(mdb), "name")
		if op != "" {
			c211sWaitOp(ftok, op, time.Now().Add(60*time.Second), 5*time.Second)
		}
		mast, _ := c211xHTTP("GET", base+"/models/"+c211sEndpointID(modelName), ftok, "", "")
		res.ModelAbsence = mast
		fmt.Printf("C211S_MODEL_DELETE proj=%s id=%s http=%d body=%s absence_http=%d\n",
			p.Name, c211sEndpointID(modelName), mdst, c211xTrunc(mdb, 400), mast)
		lst, lb := c211xHTTP("GET", base+"/endpoints?pageSize=100", ftok, "", "")
		res.EndpointList = c211xTrunc(lb, 900)
		fmt.Printf("C211S_EP_LIST_AFTER proj=%s http=%d body=%s\n", p.Name, lst, res.EndpointList)
		lst2, lb2 := c211xHTTP("GET", base+"/models?pageSize=100", ftok, "", "")
		res.ModelList = c211xTrunc(lb2, 900)
		fmt.Printf("C211S_MODEL_LIST_AFTER proj=%s http=%d body=%s\n", p.Name, lst2, res.ModelList)
		res.Verdict = "exercised"
	}

	// ---- final matrix -------------------------------------------------------
	summary := map[string]interface{}{}
	for _, r := range results {
		summary[r.Proj] = map[string]interface{}{
			"num": r.Num, "project": r.ProjectState, "service_before": r.ServiceBefore,
			"enable_http": r.EnableHTTP, "service_after": r.ServiceAfter,
			"perms": r.Perms, "perms_literal": r.PermsLit,
			"model_http": r.ModelHTTP, "shapes": r.Shapes, "verdict": r.Verdict,
			"model_delete_http": r.ModelDelHTTP, "model_absence": r.ModelAbsence,
		}
	}
	mb, _ := json.Marshal(summary)
	fmt.Printf("C211S_MATRIX %s\n", c211xTrunc(string(mb), 16000))
	fmt.Printf("C211S_VERDICT_FINAL elapsed=%ds proven_projects=%d candidates=%d\n",
		int(time.Since(start).Seconds()), provenTotal, len(results))
}

func sanitizeTag(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return strings.Trim(string(out), "-")
}

func sha256Sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}
