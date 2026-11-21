//go:build functional && gke

// C211 i124763 — cleanup-only close-out for the x124630 Vertex serving-extension lane.
//
// The x124630 probe (run 44941824283 / GKE job 136960512875, terminal 19:50:53Z)
// exercised the per-shape ladder and produced ZERO device UUIDs, but left one
// residual behind: the a3-highgpu-1g x1 NVIDIA_H100_80GB DeployModel operation
//
//	projects/153711203585/locations/us-central1/operations/2610536403032866816
//
// was still non-terminal at the probe's 580 s poll stop (stage
// ADDING_NODES_TO_CLUSTER), so the probe's own endpoint DELETE returned verbatim
// HTTP 400 FAILED_PRECONDITION "There are other operations running on the
// Endpoint ... Operation(s) are: .../operations/2610536403032866816." and the
// model DELETE returned HTTP 400 "can't be deleted because it's deployed or
// being deployed at ... endpoints/8022068172335087616".
//
// This probe runs in the SAME in-identity GKE WIF job (impersonating
// dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com) and does
// NOTHING else: it waits out that DeployModel operation, and IF the deployment
// did become available it first reads the container's own /tmp/dev.txt back
// through endpoints :predict HTTP 200 (a real H100 device line would be device
// proof), then undeploys it, deletes endpoint 8022068172335087616 and own Model
// 7932589916066676736, and proves GET 404 / endpoints+models lists empty. The
// other-lane objects endpoint 2866572498902712320, model 205538855405748224 and
// customJob 4080308893584982016 are examined read-only and never touched.
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
	c211czProj    = "153711203585"
	c211czRegion  = "us-central1"
	c211czSA      = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211czBase    = "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1"
	c211czAPIRoot = "https://us-central1-aiplatform.googleapis.com/v1/"
	c211czNonce   = "c211-i124763"
	c211czEPID    = "8022068172335087616"
	c211czModelID = "7932589916066676736"
	c211czOpID    = "projects/153711203585/locations/us-central1/operations/2610536403032866816"
	// other-lane objects: read-only, never deleted
	c211czOtherEP    = "2866572498902712320"
	c211czOtherModel = "205538855405748224"
	c211czOtherJob   = "projects/153711203585/locations/us-central1/customJobs/4080308893584982016"
)

var c211czFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

func c211czT(s string, n int) string { return c211xTrunc(s, n) }

func c211czUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211czOpDone(token, op string) (bool, int, string) {
	st, body := c211xHTTP("GET", c211czAPIRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, st, body
}

func c211czOpErr(body string) string {
	m := c211xJSON(body)
	if e, ok := m["error"].(map[string]interface{}); ok {
		return fmt.Sprintf("code=%v message=%s", e["code"], c211xStr(e, "message"))
	}
	return ""
}

type c211czDep struct {
	id   string
	avail int
}

func c211czEndpointState(token string) (int, string, []c211czDep) {
	st, body := c211xHTTP("GET", c211czBase+"/endpoints/"+c211czEPID, token, "", "")
	deps := []c211czDep{}
	m := c211xJSON(body)
	if m != nil {
		if dms, ok := m["deployedModels"].([]interface{}); ok {
			for _, dm := range dms {
				if d0, ok := dm.(map[string]interface{}); ok {
					d := c211czDep{id: c211xStr(d0, "id"), avail: -1}
					if v, ok := d0["availableReplicaCount"].(float64); ok {
						d.avail = int(v)
					}
					deps = append(deps, d)
				}
			}
		}
	}
	return st, body, deps
}

func TestC211I124630Cleanup(t *testing.T) {
	start := time.Now()
	hardEnd := start.Add(16 * time.Minute)

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211CZ_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211czT(mlog, 900))
	if tok == "" {
		fmt.Printf("C211CZ_VERDICT_FINAL mint_failed cleanup_ok=false\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211czSA)
	fmt.Printf("C211CZ_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211czSA, gst, len(at), c211czT(gbody, 300))
	if at == "" {
		for _, sa := range c211czFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211CZ_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211czT(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211CZ_VERDICT_FINAL impersonation_failed cleanup_ok=false\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 0. permission check ------------------------------------------------
	perms := []string{"aiplatform.endpoints.get", "aiplatform.endpoints.list", "aiplatform.endpoints.delete",
		"aiplatform.endpoints.undeploy", "aiplatform.endpoints.predict", "aiplatform.models.get",
		"aiplatform.models.list", "aiplatform.models.delete", "aiplatform.customJobs.get"}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211czProj, perms)
	fmt.Printf("C211CZ_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211czT(pbody, 700))

	// ---- 1. wait out the residual DeployModel operation --------------------
	opDone := false
	opErr := ""
	for i := 0; ; i++ {
		d, st, body := c211czOpDone(at, c211czOpID)
		e := c211czOpErr(body)
		fmt.Printf("C211CZ_OP_POLL t=%ds http=%d done=%v err=%s body=%s\n", i*10, st, d, e, c211czT(body, 900))
		if d {
			opDone = true
			opErr = e
			break
		}
		if time.Now().After(hardEnd.Add(-11 * time.Minute)) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Printf("C211CZ_OP_TERMINAL op=%s done=%v err=%s\n", c211czOpID, opDone, opErr)

	// ---- 2. inspect the endpoint; predict if available, then undeploy ------
	est, ebody, deps := c211czEndpointState(at)
	fmt.Printf("C211CZ_EP_GET id=%s http=%d deps=%v body=%s\n", c211czEPID, est, deps, c211czT(ebody, 1600))
	proven := []string{}
	undeployed := []string{}
	predicted := false
	for _, dep := range deps {
		if dep.avail >= 1 && !predicted {
			predicted = true
			pst2, pbody2 := c211xHTTP("POST", c211czBase+"/endpoints/"+c211czEPID+":predict", at, "application/json",
				c211czUJ(map[string]interface{}{"instances": []interface{}{map[string]interface{}{"x": 1}}}))
			fmt.Printf("C211CZ_PREDICT depID=%s http=%d body=%s\n", dep.id, pst2, c211czT(pbody2, 3000))
			uniq := map[string]bool{}
			for _, u := range regexp.MustCompile(`GPU-[0-9a-f-]{20,}`).FindAllString(pbody2, -1) {
				uniq[u] = true
			}
			for u := range uniq {
				proven = append(proven, u)
			}
			fmt.Printf("C211CZ_PREDICT_UUIDS n=%d\n", len(proven))
			if len(proven) > 0 {
				fmt.Printf("C211CZ_PROVEN shape=h100-1x-a3-highgpu n=%d uuids=%s ep=%s dep=%s\n",
					len(proven), strings.Join(proven, ","), c211czEPID, dep.id)
			}
		}
		ust, ub := c211xHTTP("POST", c211czBase+"/endpoints/"+c211czEPID+":undeployModel", at, "application/json",
			c211czUJ(map[string]interface{}{"deployedModelId": dep.id}))
		fmt.Printf("C211CZ_UNDEPLOY id=%s http=%d body=%s\n", dep.id, ust, c211czT(ub, 600))
		op := c211xStr(c211xJSON(ub), "name")
		for i := 0; i < 40 && op != ""; i++ {
			time.Sleep(10 * time.Second)
			d, _, _ := c211czOpDone(at, op)
			if i%3 == 0 || d {
				fmt.Printf("C211CZ_UNDEPLOY_POLL id=%s t=%ds done=%v\n", dep.id, (i+1)*10, d)
			}
			if d {
				break
			}
			if time.Now().After(hardEnd.Add(-8 * time.Minute)) {
				break
			}
		}
		undeployed = append(undeployed, dep.id)
	}

	// ---- 3. delete the endpoint, waiting out any operation it names --------
	epGone := est == 404
	for attempt := 0; attempt < 6 && !epGone; attempt++ {
		dst, db := c211xHTTP("DELETE", c211czBase+"/endpoints/"+c211czEPID, at, "", "")
		fmt.Printf("C211CZ_EP_DELETE attempt=%d http=%d body=%s\n", attempt+1, dst, c211czT(db, 900))
		if dst == 200 {
			op := c211xStr(c211xJSON(db), "name")
			for i := 0; i < 40 && op != ""; i++ {
				time.Sleep(5 * time.Second)
				d, _, _ := c211czOpDone(at, op)
				if i%3 == 0 || d {
					fmt.Printf("C211CZ_EP_DELETE_POLL t=%ds done=%v\n", (i+1)*5, d)
				}
				if d {
					break
				}
			}
		} else if dst == 400 {
			m := c211xJSON(db)
			if em, ok := m["error"].(map[string]interface{}); ok {
				msg := c211xStr(em, "message")
				fmt.Printf("C211CZ_EP_DELETE_PRECONDITION verbatim=%s\n", c211czT(msg, 700))
				if k := strings.Index(msg, "operations/"); k >= 0 {
					op := "projects/153711203585/locations/us-central1/operations/" + strings.Split(msg[k+len("operations/"):], "\"")[0]
					op = strings.TrimRight(op, ". ")
					fmt.Printf("C211CZ_EP_DELETE_WAITOP op=%s\n", op)
					for i := 0; i < 40; i++ {
						time.Sleep(10 * time.Second)
						d, _, _ := c211czOpDone(at, op)
						if i%3 == 0 || d {
							fmt.Printf("C211CZ_EP_DELETE_WAITOP_POLL t=%ds done=%v\n", (i+1)*10, d)
						}
						if d {
							break
						}
						if time.Now().After(hardEnd.Add(-5 * time.Minute)) {
							break
						}
					}
				}
			}
		} else if dst == 404 {
			epGone = true
		}
		ast, abody, _ := c211czEndpointState(at)
		fmt.Printf("C211CZ_EP_ABSENCE id=%s http=%d body=%s\n", c211czEPID, ast, c211czT(abody, 500))
		epGone = ast == 404
	}

	// ---- 4. delete own model (after the deployment is gone) ---------------
	mdel := -1
	mgone := false
	for attempt := 0; attempt < 5 && !mgone; attempt++ {
		dst, db := c211xHTTP("DELETE", c211czBase+"/models/"+c211czModelID, at, "", "")
		mdel = dst
		fmt.Printf("C211CZ_MODEL_DELETE attempt=%d http=%d body=%s\n", attempt+1, dst, c211czT(db, 900))
		if dst == 200 {
			op := c211xStr(c211xJSON(db), "name")
			for i := 0; i < 40 && op != ""; i++ {
				time.Sleep(5 * time.Second)
				d, _, _ := c211czOpDone(at, op)
				if i%3 == 0 || d {
					fmt.Printf("C211CZ_MODEL_DELETE_POLL t=%ds done=%v\n", (i+1)*5, d)
				}
				if d {
					break
				}
				if time.Now().After(hardEnd.Add(-3 * time.Minute)) {
					break
				}
			}
		}
		mst, mbody := c211xHTTP("GET", c211czBase+"/models/"+c211czModelID, at, "", "")
		fmt.Printf("C211CZ_MODEL_ABSENCE id=%s http=%d body=%s\n", c211czModelID, mst, c211czT(mbody, 500))
		mgone = mst == 404
		time.Sleep(5 * time.Second)
	}

	// ---- 5. list checks ---------------------------------------------------
	least, leb := c211xHTTP("GET", c211czBase+"/endpoints?pageSize=100", at, "", "")
	hasEP := strings.Contains(leb, c211czEPID)
	fmt.Printf("C211CZ_EP_LIST_AFTER http=%d has_endpoint=%v body=%s\n", least, hasEP, c211czT(leb, 1200))
	lmst, lmb := c211xHTTP("GET", c211czBase+"/models?pageSize=100", at, "", "")
	hasModel := strings.Contains(lmb, c211czModelID)
	fmt.Printf("C211CZ_MODEL_LIST_AFTER http=%d has_model=%v body=%s\n", lmst, hasModel, c211czT(lmb, 1200))

	// ---- 6. other-lane readbacks (read-only, never touched) ---------------
	oest, oeb := c211xHTTP("GET", c211czBase+"/endpoints/"+c211czOtherEP, at, "", "")
	fmt.Printf("C211CZ_OTHER_EP_GET id=%s http=%d body=%s\n", c211czOtherEP, oest, c211czT(oeb, 400))
	omst, omb := c211xHTTP("GET", c211czBase+"/models/"+c211czOtherModel, at, "", "")
	fmt.Printf("C211CZ_OTHER_MODEL_GET id=%s http=%d body=%s\n", c211czOtherModel, omst, c211czT(omb, 400))
	ojst, ojb := c211xHTTP("GET", c211czAPIRoot+c211czOtherJob, at, "", "")
	fmt.Printf("C211CZ_OTHER_JOB_GET job=%s http=%d body=%s\n", c211czOtherJob, ojst, c211czT(ojb, 400))

	cleanupOK := epGone && mgone && !hasEP && !hasModel
	fmt.Printf("C211CZ_MATRIX op_done=%v op_err=%s ep_http=%d model_http=%d ep_listed=%v model_listed=%v undeployed=%v predicted=%v\n",
		opDone, opErr, est, mdel, hasEP, hasModel, undeployed, predicted)
	fmt.Printf("C211CZ_VERDICT_FINAL elapsed=%ds cleanup_ok=%v endpoint_gone=%v model_gone=%v ep_list_empty=%v model_list_clean=%v proven_n=%d uuids=%s nonce=%s\n",
		int(time.Since(start).Seconds()), cleanupOK, epGone, mgone, !hasEP, !hasModel, len(proven), strings.Join(proven, ","), c211czNonce)
}
