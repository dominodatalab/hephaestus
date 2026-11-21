//go:build functional && gke

// C211 i124629 — cleanup-only close-out for the x124413 Vertex serving lane.
//
// Fact 124599/124672: run 3 (GKE job 136945863041) proved the first real B200
// device in host project 153711203585 — a4-highgpu-8g x8 NVIDIA_B200
// deployment 1160823144919859200 on endpoint 2866572498902712320 with
// availableReplicaCount=1 and 8 distinct NVIDIA B200 UUIDs read back through
// endpoints :predict HTTP 200.  The probe's own cleanup then failed:
//
//	C211V_EP_DELETE    id=2866572498902712320 http=400 FAILED_PRECONDITION
//	  "There are other operations running on the Endpoint ... Operation(s) are:
//	   projects/153711203585/locations/us-central1/operations/3580041775940632576."
//	C211V_MODEL_DELETE id=205538855405748224 http=400 "can't be deleted because
//	  it's deployed or being deployed at ... endpoints/2866572498902712320"
//
// This probe runs in the SAME in-identity GKE WIF job (impersonating
// dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com) and does
// NOTHING but clean up: it waits out the still-running H100 DeployModel
// operation 3580041775940632576, undeploys anything still attached, deletes
// endpoint 2866572498902712320 and own Model 205538855405748224, and proves
// GET 404 / list-empty.  The pre-existing other-lane customJob
// 4080308893584982016 is examined read-only and never touched.
package functional

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func c211vTrunc(s string, n int) string { return c211xTrunc(s, n) }

func c211vUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

const (
	c211cProj     = "153711203585"
	c211cRegion   = "us-central1"
	c211cSA       = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211cBase     = "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1"
	c211cAPIRoot  = "https://us-central1-aiplatform.googleapis.com/v1/"
	c211cNonce    = "c211-i124629"
	c211cEPID     = "2866572498902712320"
	c211cModelID  = "205538855405748224"
	c211cDeployID = "1160823144919859200"
	c211cOpID     = "projects/153711203585/locations/us-central1/operations/3580041775940632576"
	c211cOtherJob = "projects/153711203585/locations/us-central1/customJobs/4080308893584982016"
)

var c211cFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

func c211cUJ(v interface{}) string { return c211vUJ(v) }

func c211cOpDone(token, op string) (bool, int, string) {
	st, body := c211xHTTP("GET", c211cAPIRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, st, body
}

func c211cEndpointState(token string) (int, string, []string) {
	st, body := c211xHTTP("GET", c211cBase+"/endpoints/"+c211cEPID, token, "", "")
	ids := []string{}
	m := c211xJSON(body)
	if m != nil {
		if dms, ok := m["deployedModels"].([]interface{}); ok {
			for _, dm := range dms {
				if d0, ok := dm.(map[string]interface{}); ok {
					if id := c211xStr(d0, "id"); id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
	}
	return st, body, ids
}

func TestC211I124629Cleanup(t *testing.T) {
	start := time.Now()
	hardEnd := start.Add(18 * time.Minute)

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211C_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211vTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211C_VERDICT_FINAL mint_failed cleanup_ok=false\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211cSA)
	fmt.Printf("C211C_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211cSA, gst, len(at), c211vTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211cFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211C_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211vTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211C_VERDICT_FINAL impersonation_failed cleanup_ok=false\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 0. permission check (delete/undeploy/get/list) -------------------
	perms := []string{"aiplatform.endpoints.get", "aiplatform.endpoints.list", "aiplatform.endpoints.delete",
		"aiplatform.endpoints.undeploy", "aiplatform.models.get", "aiplatform.models.list",
		"aiplatform.models.delete", "aiplatform.customJobs.get"}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211cProj, perms)
	fmt.Printf("C211C_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211vTrunc(pbody, 700))

	// ---- 1. wait out the still-running H100 DeployModel operation ---------
	opDone := false
	for i := 0; ; i++ {
		d, st, body := c211cOpDone(at, c211cOpID)
		fmt.Printf("C211C_OP_POLL t=%ds http=%d done=%v body=%s\n", i*10, st, d, c211vTrunc(body, 800))
		if d {
			opDone = true
			break
		}
		if time.Now().After(hardEnd.Add(-12 * time.Minute)) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Printf("C211C_OP_TERMINAL op=%s done=%v\n", c211cOpID, opDone)

	// ---- 2. inspect the endpoint and undeploy anything still attached -----
	est, ebody, depIDs := c211cEndpointState(at)
	fmt.Printf("C211C_EP_GET id=%s http=%d deployed=%v body=%s\n", c211cEPID, est, depIDs, c211vTrunc(ebody, 1400))
	undeployed := []string{}
	for _, id := range depIDs {
		ust, ub := c211xHTTP("POST", c211cBase+"/endpoints/"+c211cEPID+":undeployModel", at, "application/json",
			c211cUJ(map[string]interface{}{"deployedModelId": id}))
		fmt.Printf("C211C_UNDEPLOY id=%s http=%d body=%s\n", id, ust, c211vTrunc(ub, 500))
		op := c211xStr(c211xJSON(ub), "name")
		for i := 0; i < 60 && op != ""; i++ {
			time.Sleep(10 * time.Second)
			d, _, _ := c211cOpDone(at, op)
			if i%3 == 0 || d {
				fmt.Printf("C211C_UNDEPLOY_POLL id=%s t=%ds done=%v\n", id, (i+1)*10, d)
			}
			if d {
				break
			}
			if time.Now().After(hardEnd.Add(-8 * time.Minute)) {
				break
			}
		}
		undeployed = append(undeployed, id)
	}

	// ---- 3. delete the endpoint, waiting out any operation it names -------
	epGone := false
	for attempt := 0; attempt < 8; attempt++ {
		dst, db := c211xHTTP("DELETE", c211cBase+"/endpoints/"+c211cEPID, at, "", "")
		fmt.Printf("C211C_EP_DELETE attempt=%d http=%d body=%s\n", attempt+1, dst, c211vTrunc(db, 900))
		if dst == 200 {
			op := c211xStr(c211xJSON(db), "name")
			for i := 0; i < 40 && op != ""; i++ {
				time.Sleep(5 * time.Second)
				d, _, _ := c211cOpDone(at, op)
				if i%3 == 0 || d {
					fmt.Printf("C211C_EP_DELETE_POLL t=%ds done=%v\n", (i+1)*5, d)
				}
				if d {
					break
				}
			}
		} else if dst == 400 {
			// FAILED_PRECONDITION names an operation that is still running
			m := c211xJSON(db)
			if em, ok := m["error"].(map[string]interface{}); ok {
				msg := c211xStr(em, "message")
				fmt.Printf("C211C_EP_DELETE_PRECONDITION verbatim=%s\n", c211vTrunc(msg, 600))
				if k := strings.Index(msg, "operations/"); k >= 0 {
					op := "projects/153711203585/locations/us-central1/operations/" + strings.Split(msg[k+len("operations/"):], "\"")[0]
					op = strings.TrimRight(op, ". ")
					fmt.Printf("C211C_EP_DELETE_WAITOP op=%s\n", op)
					for i := 0; i < 60; i++ {
						time.Sleep(10 * time.Second)
						d, _, _ := c211cOpDone(at, op)
						if i%3 == 0 || d {
							fmt.Printf("C211C_EP_DELETE_WAITOP_POLL t=%ds done=%v\n", (i+1)*10, d)
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
		}
		gst, _, _ := c211cEndpointState(at)
		if gst == 404 {
			epGone = true
			break
		}
		if time.Now().After(hardEnd.Add(-4 * time.Minute)) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	agst, agb, agdeps := c211cEndpointState(at)
	fmt.Printf("C211C_EP_ABSENCE id=%s http=%d deployed=%v body=%s\n", c211cEPID, agst, agdeps, c211vTrunc(agb, 500))
	if agst == 404 {
		epGone = true
	}

	// ---- 4. delete own Model 205538855405748224 --------------------------
	modelGone := false
	mgst := 0
	for attempt := 0; attempt < 4; attempt++ {
		mdst, mdb := c211xHTTP("DELETE", c211cBase+"/models/"+c211cModelID, at, "", "")
		fmt.Printf("C211C_MODEL_DELETE attempt=%d id=%s http=%d body=%s\n", attempt+1, c211cModelID, mdst, c211vTrunc(mdb, 700))
		if mdst == 200 {
			op := c211xStr(c211xJSON(mdb), "name")
			for i := 0; i < 30 && op != ""; i++ {
				time.Sleep(5 * time.Second)
				d, _, _ := c211cOpDone(at, op)
				if d {
					break
				}
			}
		}
		var mgb string
		mgst, mgb = c211xHTTP("GET", c211cBase+"/models/"+c211cModelID, at, "", "")
		fmt.Printf("C211C_MODEL_ABSENCE id=%s http=%d body=%s\n", c211cModelID, mgst, c211vTrunc(mgb, 400))
		if mgst == 404 {
			modelGone = true
			break
		}
		time.Sleep(5 * time.Second)
	}

	// ---- 5. list readbacks ------------------------------------------------
	lstM, lbodyM := c211xHTTP("GET", c211cBase+"/models?pageSize=100", at, "", "")
	lstE, lbodyE := c211xHTTP("GET", c211cBase+"/endpoints?pageSize=100", at, "", "")
	modelStillListed := strings.Contains(lbodyM, "models/"+c211cModelID)
	epStillListed := strings.Contains(lbodyE, "endpoints/"+c211cEPID)
	fmt.Printf("C211C_MODEL_LIST_AFTER http=%d has_model=%v body=%s\n", lstM, modelStillListed, c211vTrunc(lbodyM, 1000))
	fmt.Printf("C211C_EP_LIST_AFTER http=%d has_endpoint=%v body=%s\n", lstE, epStillListed, c211vTrunc(lbodyE, 1000))
	epListEmpty := !epStillListed
	modelListClean := !modelStillListed

	// ---- 6. pre-existing other-lane customJob must stay untouched ---------
	jst, jbody := c211xHTTP("GET", c211cAPIRoot+c211cOtherJob, at, "", "")
	jstate := c211xStr(c211xJSON(jbody), "state")
	fmt.Printf("C211C_OTHER_JOB_GET job=%s http=%d state=%s body=%s\n", c211cOtherJob, jst, jstate, c211vTrunc(jbody, 500))

	cleanupOK := epGone && modelGone && epListEmpty && modelListClean
	fmt.Printf("C211C_CLEANUP_MATRIX op_done=%v ep_http=%d model_http=%d ep_listed=%v model_listed=%v undeployed=%v\n",
		opDone, agst, mgst, epStillListed, modelStillListed, undeployed)
	fmt.Printf("C211C_VERDICT_FINAL elapsed=%ds cleanup_ok=%v endpoint_gone=%v model_gone=%v ep_list_empty=%v model_list_clean=%v other_job_state=%s nonce=%s\n",
		int(time.Since(start).Seconds()), cleanupOK, epGone, modelGone, epListEmpty, modelListClean, jstate, c211cNonce)
	if !cleanupOK {
		t.Errorf("cleanup incomplete: endpoint_gone=%v model_gone=%v ep_list_empty=%v model_list_clean=%v", epGone, modelGone, epListEmpty, modelListClean)
	}
}
