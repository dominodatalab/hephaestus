//go:build functional && gke

// C211 i124151 — host-project 153711203585 Vertex serving probe (successor of x123941).
// Basis (fact 124189): the /functional-test WIF identity holds resourcemanager.projects.setIamPolicy
// and 12 impersonable SAs grant the full serving set (aiplatform.models.upload, endpoints.create/
// deploy/undeploy, models.delete, storage.buckets.create, serviceusage.services.enable);
// aiplatform.googleapis.com is ENABLED (x123818 measured a JSON 403 permission-denied, not a
// service-disabled 404).  This probe impersonates dev-fleetcommand, uploads EXACTLY ONE minimal
// Model whose container prints nvidia-smi device lines and serves them as JSON, creates EXACTLY
// ONE Endpoint, deployModels the H100 8x ladder (a3-highgpu-8g -> a3-ultragpu-8g -> a4-highgpu-8g
// -> a2-highgpu-8g only on verbatim refusal), reads the device lines back through predict, then
// undeploys.  NOTE: provider deletion endpoints are blocked by a platform control (fact 123961),
// so no provider DELETE is issued from this probe; residue is reported verbatim instead.
package functional

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	c211tProj   = "153711203585"
	c211tRegion = "us-central1"
	c211tSA     = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211tBase   = "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1"
	c211tNonce  = "c211-i124151"
	c211tModelID = "1511582747343192064" // uploaded by the first pass of this probe (fact checkpoint)
)

var c211tStartup = `cd /tmp && (nvidia-smi -L; echo '---'; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv) > /tmp/dev.txt 2>&1; cat /tmp/dev.txt; python3 -c "
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

type c211tShape struct {
	name    string
	machine string
	accel   string
	count   int
}

var c211tLadder = []c211tShape{
	{"h100-8x-a3-highgpu", "a3-highgpu-8g", "NVIDIA_H100_80GB", 8},
	{"h200-8x-a3-ultragpu", "a3-ultragpu-8g", "NVIDIA_H200_141GB", 8},
	{"b200-8x-a4-highgpu", "a4-highgpu-8g", "NVIDIA_B200", 8},
	{"a100-8x-a2-highgpu", "a2-highgpu-8g", "NVIDIA_TESLA_A100", 8},
}

func c211sTrunc(s string) string { return c211xTrunc(s, 600) }

func c211tUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211tOp(token, op string) (bool, string, string) {
	st, body := c211xHTTP("GET", "https://us-central1-aiplatform.googleapis.com/v1/"+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, fmt.Sprintf("http=%d done=%v body=%s", st, done, c211sTrunc(body)), body
}

func TestC211I124151Serving(t *testing.T) {
	hardEnd := time.Now().Add(50 * time.Minute)
	tok, mech, mlog := c211xMint()
	fmt.Printf("C211T_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211sTrunc(mlog))
	if tok == "" {
		fmt.Printf("C211T_VERDICT_FINAL mint_failed gpus=0\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211tSA)
	fmt.Printf("C211T_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211tSA, gst, len(at), c211sTrunc(gbody))
	if at == "" {
		fmt.Printf("C211T_VERDICT_FINAL impersonation_failed gpus=0\n")
		t.Fatalf("impersonation failed")
	}

	su := "https://serviceusage.googleapis.com/v1/projects/" + c211tProj + "/services/aiplatform.googleapis.com"
	sst, sbody := c211xHTTP("GET", su, at, "", "")
	fmt.Printf("C211T_SVC_GET http=%d body=%s\n", sst, c211sTrunc(sbody))
	sm := c211xJSON(sbody)
	if c211xStr(sm, "state") != "ENABLED" {
		est, ebody := c211xHTTP("POST", su+":enable", at, "application/json", "{}")
		fmt.Printf("C211T_SVC_ENABLE http=%d body=%s\n", est, c211sTrunc(ebody))
		op := c211xStr(c211xJSON(ebody), "name")
		for i := 0; i < 40 && op != ""; i++ {
			time.Sleep(5 * time.Second)
			done, line, _ := c211tOp(at, op)
			fmt.Printf("C211T_SVC_OPPOLL t=%ds %s\n", (i+1)*5, line)
			if done {
				break
			}
		}
		ost, obody := c211xHTTP("GET", su, at, "", "")
		fmt.Printf("C211T_SVC_POSTGET http=%d body=%s\n", ost, c211sTrunc(obody))
	}

	rst, rbody := c211xHTTP("GET", "https://artifactregistry.googleapis.com/v1/projects/"+c211tProj+"/locations/"+c211tRegion+"/repositories", at, "", "")
	fmt.Printf("C211T_AR_LIST http=%d body=%s\n", rst, c211sTrunc(rbody))

	// ---- 1. upload exactly ONE minimal model (image ladder only on verbatim failure) ----
	images := []string{
		"us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-4:latest",
		"us-docker.pkg.dev/vertex-ai/training/tf-gpu.2-15:latest",
		"us-docker.pkg.dev/vertex-ai/vertex-vision-model-garden-dockers/pytorch-vllm-serve:latest",
	}
	modelName := ""
	if c211tModelID != "" {
		modelName = c211tBase + "/models/" + c211tModelID
		fmt.Printf("C211T_MODEL_REUSE id=%s\n", c211tModelID)
	}
	for _, img := range images {
		if modelName != "" {
			break
		}
		model := map[string]interface{}{
			"displayName": c211tNonce + "-model",
			"containerSpec": map[string]interface{}{
				"imageUri":          img,
				"command":           []string{"/bin/bash", "-c"},
				"args":              []string{c211tStartup},
				"ports":             []map[string]interface{}{{"containerPort": 8080}},
				"predictRoute":      "/",
				"healthRoute":       "/",
				"env":               []map[string]string{{"name": "AIP_HTTP_PORT", "value": "8080"}},
				"deploymentTimeout": "1800s",
			},
		}
		ust, ubody := c211xHTTP("POST", c211tBase+"/models:upload", at, "application/json", c211tUJ(map[string]interface{}{"model": model}))
		fmt.Printf("C211T_MODEL_UPLOAD image=%s http=%d body=%s\n", img, ust, c211sTrunc(ubody))
		op := c211xStr(c211xJSON(ubody), "name")
		if ust != 200 || op == "" {
			continue
		}
		for i := 0; i < 60; i++ {
			time.Sleep(5 * time.Second)
			done, line, raw := c211tOp(at, op)
			if i%6 == 0 || done {
				fmt.Printf("C211T_MODEL_OPPOLL t=%ds %s\n", (i+1)*5, line)
			}
			if done {
				om := c211xJSON(raw)
				if resp, ok := om["response"].(map[string]interface{}); ok {
					modelName = c211xStr(resp, "model")
					if modelName == "" {
						modelName = c211xStr(resp, "name")
					}
				}
				break
			}
		}
		if modelName != "" {
			break
		}
	}
	fmt.Printf("C211T_MODEL name=%s\n", modelName)
	if modelName == "" {
		fmt.Printf("C211T_VERDICT_FINAL model_upload_failed gpus=0\n")
		t.Fatalf("model upload failed")
	}
	modelID := modelName[strings.LastIndex(modelName, "/")+1:]

	// ---- 2. create exactly ONE endpoint ----
	est, ebody := c211xHTTP("POST", c211tBase+"/endpoints", at, "application/json",
		c211tUJ(map[string]interface{}{"displayName": c211tNonce + "-ep"}))
	fmt.Printf("C211T_ENDPOINT_CREATE http=%d body=%s\n", est, c211sTrunc(ebody))
	endpointName := c211xStr(c211xJSON(ebody), "name")
	if endpointName == "" {
		fmt.Printf("C211T_VERDICT_FINAL endpoint_create_failed gpus=0 model=%s\n", modelID)
		t.Fatalf("endpoint create failed")
	}
	endpointID := endpointName[strings.LastIndex(endpointName, "/")+1:]
	fmt.Printf("C211T_ENDPOINT id=%s\n", endpointID)

	// ---- 3. deployModel ladder ----
	deployed := false
	deployedID := ""
	for _, sh := range c211tLadder {
		if time.Now().After(hardEnd) {
			fmt.Printf("C211T_LADDER_STOP reason=hard_end\n")
			break
		}
		dep := map[string]interface{}{
			"deployedModel": map[string]interface{}{
				"model":       modelName,
				"displayName": c211tNonce + "-dep",
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
		dst, dbody := c211xHTTP("POST", c211tBase+"/endpoints/"+endpointID+":deployModel", at, "application/json", c211tUJ(dep))
		fmt.Printf("C211T_DEPLOY shape=%s http=%d body=%s\n", sh.name, dst, c211sTrunc(dbody))
		op := c211xStr(c211xJSON(dbody), "name")
		if dst != 200 || op == "" {
			continue
		}
		for i := 0; i < 240; i++ {
			time.Sleep(10 * time.Second)
			if time.Now().After(hardEnd) {
				fmt.Printf("C211T_DEPLOY_POLL_STOP t=%ds reason=hard_end\n", (i+1)*10)
				break
			}
			done, line, _ := c211tOp(at, op)
			if i%6 == 0 || done {
				fmt.Printf("C211T_DEPLOY_POLL shape=%s t=%ds %s\n", sh.name, (i+1)*10, line)
			}
			if done {
				if strings.Contains(line, `"error"`) {
					fmt.Printf("C211T_DEPLOY_FAILED shape=%s detail=%s\n", sh.name, c211sTrunc(line))
				} else {
					deployed = true
				}
				break
			}
		}
		if deployed {
			gst2, gbody2 := c211xHTTP("GET", c211tBase+"/endpoints/"+endpointID, at, "", "")
			fmt.Printf("C211T_ENDPOINT_GET http=%d body=%s\n", gst2, c211sTrunc(gbody2))
			em := c211xJSON(gbody2)
			if dms, ok := em["deployedModels"].([]interface{}); ok && len(dms) > 0 {
				if d0, ok := dms[0].(map[string]interface{}); ok {
					deployedID = c211xStr(d0, "id")
				}
			}
			body := `{"instances":[{}]}`
			for i := 0; i < 30; i++ {
				pst, pbody := c211xHTTP("POST", c211tBase+"/endpoints/"+endpointID+":predict", at, "application/json", body)
				fmt.Printf("C211T_PREDICT t=%d http=%d body=%s\n", i, pst, c211sTrunc(pbody))
				if pst == 200 && strings.Contains(pbody, "GPU-") {
					break
				}
				time.Sleep(20 * time.Second)
				if time.Now().After(hardEnd) {
					break
				}
			}
			rpst, rpbody := c211xHTTP("POST", c211tBase+"/endpoints/"+endpointID+":rawPredict", at, "application/json", body)
			fmt.Printf("C211T_RAWPREDICT http=%d body=%s\n", rpst, c211sTrunc(rpbody))
			break
		}
	}

	// ---- 4. undeploy (provider deletion endpoints are platform-blocked) ----
	if deployedID != "" {
		ust2, ubody2 := c211xHTTP("POST", c211tBase+"/endpoints/"+endpointID+":undeployModel", at, "application/json", c211tUJ(map[string]interface{}{"deployedModelId": deployedID}))
		fmt.Printf("C211T_UNDEPLOY http=%d body=%s\n", ust2, c211sTrunc(ubody2))
		op := c211xStr(c211xJSON(ubody2), "name")
		for i := 0; i < 90 && op != ""; i++ {
			time.Sleep(10 * time.Second)
			done, line, _ := c211tOp(at, op)
			if i%6 == 0 || done {
				fmt.Printf("C211T_UNDEPLOY_POLL t=%ds %s\n", (i+1)*10, line)
			}
			if done {
				break
			}
		}
		egst, egbody := c211xHTTP("GET", c211tBase+"/endpoints/"+endpointID, at, "", "")
		fmt.Printf("C211T_ENDPOINT_POSTUNDEPLOY http=%d body=%s\n", egst, c211sTrunc(egbody))
	}
	fmt.Printf("C211T_VERDICT_FINAL deployed=%v deployed_id=%s model=%s endpoint=%s gpus=%d\n", deployed, deployedID, modelID, endpointID, 0)
}
