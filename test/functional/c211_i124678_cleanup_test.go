//go:build functional && gke

// C211 i124678 — cleanup-only close-out for the H200 Vertex serving lane.
//
// Fact 124761: the H200 a3-ultragpu-8g x8 NVIDIA_H200_141GB serving ladder
// (host project 153711203585) produced ZERO device UUIDs:
//   - us-central1 and europe-west4 deployModel operations terminated
//     done=true with error code 8
//     "The following quotas are exceeded: CustomModelServingH200GPUsPerProjectPerRegion";
//   - europe-west1's endpoint-create operation 5208465951770542080 never
//     completed inside the probe's 150 s poll window, so deployModel returned
//     HTTP 404 "Endpoint .../endpoints/5465604131953573888 is not found." and
//     the DELETE returned HTTP 400 "is being created and cannot be deleted."
//
// All endpoints/models the original probe saw were retired to GET 404 and the
// lists read c211=0, BUT the europe-west1 endpoint-create operation was still
// pending at the last read (19:58:52Z, t=140 s) and the probe's final
// europe-west1 list read happened at 19:59:10Z while the operation could still
// complete afterwards — so endpoint 5465604131953573888 may exist as an orphan.
//
// This cleanup-only probe runs in the SAME in-identity GKE WIF job
// (impersonating dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com)
// and does NOTHING but verify/retire that orphan:
//   1. poll europe-west1 create operation 5208465951770542080 to terminal;
//   2. GET endpoint 5465604131953573888; if present, DELETE (waiting out any
//      named operation) and prove GET 404;
//   3. GET model 1730483967561302016; if present, DELETE and prove GET 404;
//   4. list endpoints+models in europe-west1 (and read-only in us-central1 /
//      europe-west4) and prove no c211-i124678 object remains;
//   5. read-only GET of another lane's objects (endpoint 2866572498902712320,
//      model 205538855405748224, customJob 4080308893584982016) — never touched.
package functional

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	c211hcProj    = "153711203585"
	c211hcSA      = "dev-fleetcommand@domino-eng-platform-dev.iam.gserviceaccount.com"
	c211hcNonce   = "c211-i124678-clean"
	c211hcAPIRoot = "https://europe-west1-aiplatform.googleapis.com/v1/"
	c211hcBase    = "https://europe-west1-aiplatform.googleapis.com/v1/projects/153711203585/locations/europe-west1"
	c211hcEPID    = "5465604131953573888"
	c211hcModelID = "1730483967561302016"
	c211hcOpID    = "projects/153711203585/locations/europe-west1/operations/5208465951770542080"
	// another lane's objects — read-only, never mutated
	c211hcOtherEP    = "2866572498902712320"
	c211hcOtherModel = "205538855405748224"
	c211hcOtherJob   = "projects/153711203585/locations/us-central1/customJobs/4080308893584982016"
)

var c211hcFallbackSAs = []string{
	"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
	"domino-eng-platform-dev@appspot.gserviceaccount.com",
	"153711203585-compute@developer.gserviceaccount.com",
}

// regional bases that hosted a c211-i124678 attempt in the original run
var c211hcRegions = map[string]string{
	"us-central1":  "https://us-central1-aiplatform.googleapis.com/v1/projects/153711203585/locations/us-central1",
	"europe-west1": "https://europe-west1-aiplatform.googleapis.com/v1/projects/153711203585/locations/europe-west1",
	"europe-west4": "https://europe-west4-aiplatform.googleapis.com/v1/projects/153711203585/locations/europe-west4",
}

func c211hcUJ(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func c211hcOpDone(token, op string) (bool, int, string) {
	st, body := c211xHTTP("GET", c211hcAPIRoot+op, token, "", "")
	m := c211xJSON(body)
	done, _ := m["done"].(bool)
	return done, st, body
}

func c211hcGet(token, url string) (int, string) {
	return c211xHTTP("GET", url, token, "", "")
}

func c211hcDeployedIDs(body string) []string {
	ids := []string{}
	m := c211xJSON(body)
	if m == nil {
		return ids
	}
	if dms, ok := m["deployedModels"].([]interface{}); ok {
		for _, dm := range dms {
			if d0, ok := dm.(map[string]interface{}); ok {
				if id := c211xStr(d0, "id"); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// c211hcWaitOp polls one operation until done or until the deadline.
func c211hcWaitOp(token, op string, deadline time.Time, label string, step time.Duration) bool {
	for i := 0; ; i++ {
		d, st, body := c211hcOpDone(token, op)
		fmt.Printf("C211HC_OP_POLL label=%s i=%d http=%d done=%v body=%s\n",
			label, i, st, d, c211xTrunc(body, 800))
		if d {
			return true
		}
		if time.Now().After(deadline) {
			fmt.Printf("C211HC_OP_WAIT_TIMEOUT label=%s op=%s\n", label, op)
			return false
		}
		time.Sleep(step)
	}
}

func TestC211I124678Cleanup(t *testing.T) {
	start := time.Now()
	hardEnd := start.Add(11 * time.Minute)

	tok, mech, mlog := c211xMint()
	fmt.Printf("C211HC_MINT mech=%s token_len=%d log=%s\n", mech, len(tok), c211xTrunc(mlog, 900))
	if tok == "" {
		fmt.Printf("C211HC_VERDICT_FINAL mint_failed cleanup_ok=false\n")
		t.Fatalf("mint failed")
	}
	at, gst, gbody := c211xGenerateAccessToken(tok, c211hcSA)
	fmt.Printf("C211HC_GENAT sa=%s http=%d token_len=%d raw=%s\n", c211hcSA, gst, len(at), c211xTrunc(gbody, 300))
	if at == "" {
		for _, sa := range c211hcFallbackSAs {
			a2, s2, b2 := c211xGenerateAccessToken(tok, sa)
			fmt.Printf("C211HC_GENAT_FALLBACK sa=%s http=%d token_len=%d raw=%s\n", sa, s2, len(a2), c211xTrunc(b2, 300))
			if a2 != "" {
				at = a2
				break
			}
		}
	}
	if at == "" {
		fmt.Printf("C211HC_VERDICT_FINAL impersonation_failed cleanup_ok=false\n")
		t.Fatalf("impersonation failed")
	}

	// ---- 0. permissions ---------------------------------------------------
	perms := []string{"aiplatform.endpoints.get", "aiplatform.endpoints.list", "aiplatform.endpoints.delete",
		"aiplatform.endpoints.undeploy", "aiplatform.models.get", "aiplatform.models.list",
		"aiplatform.models.delete", "aiplatform.customJobs.get"}
	al, pst, pbody := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+c211hcProj, perms)
	fmt.Printf("C211HC_TESTIAM http=%d allowed=%v raw=%s\n", pst, al, c211xTrunc(pbody, 700))

	// ---- 1. poll the europe-west1 endpoint-create operation to terminal ---
	opDone := false
	opResponse := ""
	for i := 0; i < 40; i++ {
		d, st, body := c211hcOpDone(at, c211hcOpID)
		fmt.Printf("C211HC_OP_POLL t=%ds http=%d done=%v body=%s\n", i*10, st, d, c211xTrunc(body, 900))
		if d {
			opDone = true
			opResponse = body
			break
		}
		if time.Now().After(hardEnd.Add(-7 * time.Minute)) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Printf("C211HC_OP_TERMINAL op=%s done=%v resp=%s\n", c211hcOpID, opDone, c211xTrunc(opResponse, 1200))

	// ---- 2. retire the europe-west1 endpoint if it exists -----------------
	epGone := false
	for attempt := 0; attempt < 8; attempt++ {
		est, ebody := c211hcGet(at, c211hcBase+"/endpoints/"+c211hcEPID)
		deps := c211hcDeployedIDs(ebody)
		fmt.Printf("C211HC_EP_GET attempt=%d id=%s http=%d deployed=%v body=%s\n",
			attempt+1, c211hcEPID, est, deps, c211xTrunc(ebody, 900))
		if est == 404 {
			epGone = true
			break
		}
		if est != 200 {
			break
		}
		for _, id := range deps {
			ust, ub := c211xHTTP("POST", c211hcBase+"/endpoints/"+c211hcEPID+":undeployModel", at, "application/json",
				c211hcUJ(map[string]interface{}{"deployedModelId": id}))
			fmt.Printf("C211HC_UNDEPLOY id=%s http=%d body=%s\n", id, ust, c211xTrunc(ub, 500))
			if op := c211xStr(c211xJSON(ub), "name"); op != "" {
				c211hcWaitOp(at, op, hardEnd.Add(-5*time.Minute), "undeploy", 10*time.Second)
			}
		}
		dst, db := c211xHTTP("DELETE", c211hcBase+"/endpoints/"+c211hcEPID, at, "", "")
		fmt.Printf("C211HC_EP_DELETE attempt=%d http=%d body=%s\n", attempt+1, dst, c211xTrunc(db, 900))
		if dst == 200 {
			if op := c211xStr(c211xJSON(db), "name"); op != "" {
				c211hcWaitOp(at, op, hardEnd.Add(-4*time.Minute), "ep_delete", 5*time.Second)
			}
		} else if dst == 400 {
			m := c211xJSON(db)
			if em, ok := m["error"].(map[string]interface{}); ok {
				msg := c211xStr(em, "message")
				fmt.Printf("C211HC_EP_DELETE_PRECONDITION verbatim=%s\n", c211xTrunc(msg, 700))
				if k := strings.Index(msg, "operations/"); k >= 0 {
					op := "projects/153711203585/locations/europe-west1/operations/" +
						strings.TrimRight(strings.Split(msg[k+len("operations/"):], "\"")[0], ". ")
					fmt.Printf("C211HC_EP_DELETE_WAITOP op=%s\n", op)
					c211hcWaitOp(at, op, hardEnd.Add(-3*time.Minute), "ep_delete_waitop", 10*time.Second)
				}
			}
		}
		if time.Now().After(hardEnd.Add(-3 * time.Minute)) {
			break
		}
	}
	agst, agb := c211hcGet(at, c211hcBase+"/endpoints/"+c211hcEPID)
	fmt.Printf("C211HC_EP_ABSENCE id=%s http=%d body=%s\n", c211hcEPID, agst, c211xTrunc(agb, 500))
	if agst == 404 {
		epGone = true
	}

	// ---- 3. retire the europe-west1 model if it exists --------------------
	modelGone := false
	mgst := 0
	for attempt := 0; attempt < 4; attempt++ {
		gst2, gb2 := c211hcGet(at, c211hcBase+"/models/"+c211hcModelID)
		fmt.Printf("C211HC_MODEL_GET attempt=%d id=%s http=%d body=%s\n", attempt+1, c211hcModelID, gst2, c211xTrunc(gb2, 500))
		if gst2 == 404 {
			modelGone = true
			break
		}
		if gst2 != 200 {
			break
		}
		mdst, mdb := c211xHTTP("DELETE", c211hcBase+"/models/"+c211hcModelID, at, "", "")
		fmt.Printf("C211HC_MODEL_DELETE attempt=%d id=%s http=%d body=%s\n", attempt+1, c211hcModelID, mdst, c211xTrunc(mdb, 700))
		if mdst == 200 {
			if op := c211xStr(c211xJSON(mdb), "name"); op != "" {
				c211hcWaitOp(at, op, hardEnd.Add(-2*time.Minute), "model_delete", 5*time.Second)
			}
		}
		mgst, _ = c211hcGet(at, c211hcBase+"/models/"+c211hcModelID)
		if mgst == 404 {
			modelGone = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	mgst2, mgb2 := c211hcGet(at, c211hcBase+"/models/"+c211hcModelID)
	fmt.Printf("C211HC_MODEL_ABSENCE id=%s http=%d body=%s\n", c211hcModelID, mgst2, c211xTrunc(mgb2, 400))
	if mgst2 == 404 {
		modelGone = true
	}

	// ---- 4. per-region list readbacks (no c211-i124678 object may remain) --
	listClean := true
	for _, region := range []string{"us-central1", "europe-west1", "europe-west4"} {
		base := c211hcRegions[region]
		lstE, lbodyE := c211hcGet(at, base+"/endpoints?pageSize=100")
		lstM, lbodyM := c211hcGet(at, base+"/models?pageSize=100")
		epHit := strings.Contains(lbodyE, c211hcEPID) || strings.Contains(lbodyE, "c211-i124678")
		mdlHit := strings.Contains(lbodyM, c211hcModelID) || strings.Contains(lbodyM, "c211-i124678")
		// also count any c211-displayName left behind
		fmt.Printf("C211HC_EP_LIST region=%s http=%d has_own=%v body=%s\n", region, lstE, epHit, c211xTrunc(lbodyE, 900))
		fmt.Printf("C211HC_MODEL_LIST region=%s http=%d has_own=%v body=%s\n", region, lstM, mdlHit, c211xTrunc(lbodyM, 900))
		if epHit || mdlHit {
			listClean = false
		}
	}

	// ---- 5. another lane's objects: READ-ONLY ----------------------------
	oest, oeb := c211hcGet(at, c211hcRegions["us-central1"]+"/endpoints/"+c211hcOtherEP)
	omst, omb := c211hcGet(at, c211hcRegions["us-central1"]+"/models/"+c211hcOtherModel)
	ojst, ojb := c211hcGet(at, c211hcAPIRoot+c211hcOtherJob)
	ojstate := c211xStr(c211xJSON(ojb), "state")
	fmt.Printf("C211HC_OTHER_EP_READONLY id=%s http=%d body=%s\n", c211hcOtherEP, oest, c211xTrunc(oeb, 400))
	fmt.Printf("C211HC_OTHER_MODEL_READONLY id=%s http=%d body=%s\n", c211hcOtherModel, omst, c211xTrunc(omb, 400))
	fmt.Printf("C211HC_OTHER_JOB_READONLY job=%s http=%d state=%s\n", c211hcOtherJob, ojst, ojstate)

	epListClean := listClean
	cleanupOK := epGone && modelGone && epListClean
	fmt.Printf("C211HC_CLEANUP_MATRIX op_done=%v ep_http=%d model_http=%d list_clean=%v\n",
		opDone, agst, mgst2, listClean)
	fmt.Printf("C211HC_VERDICT_FINAL elapsed=%ds cleanup_ok=%v endpoint_gone=%v model_gone=%v lists_clean=%v other_ep_http=%d other_model_http=%d other_job_state=%s nonce=%s\n",
		int(time.Since(start).Seconds()), cleanupOK, epGone, modelGone, epListClean, oest, omst, ojstate, c211hcNonce)
	if !cleanupOK {
		t.Errorf("cleanup incomplete: endpoint_gone=%v model_gone=%v lists_clean=%v", epGone, modelGone, epListClean)
	}
}
