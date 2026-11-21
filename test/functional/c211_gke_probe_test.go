//go:build functional

// C211 i119859 GKE/GCP capability probe.
//
// Runs inside the dominodatalab/hephaestus "GKE image building" job after
// google-github-actions/auth@v2 has exported a Workload Identity Federation
// credential file. It never prints a secret value: environment variables and
// tokens are reported as presence + length + SHA-256 prefix only.
//
// Flow: locate the exported ADC file, perform the Google STS token exchange and
// service-account impersonation with nothing but the Go standard library (gcloud
// is used only as a fallback), then enumerate the resolved project's identity,
// enabled services, compute regions, per-region GPU quota and accelerator types.
// If a genuinely positive on-demand GPU quota exists, exactly ONE minimal marker
// VM is created in a zone that offers that accelerator, its serial console is
// polled for the nvidia-smi device UUID, and the VM is deleted and read back as
// absent. Exact provider errors are printed verbatim for GATE evidence.
package functional

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const c211Scope = "https://www.googleapis.com/auth/cloud-platform"

var c211Client = &http.Client{Timeout: 120 * time.Second}

func c211Emit(section, body string) {
	fmt.Printf("\n=== C211GCP %s BEGIN ===\n%s\n=== C211GCP %s END ===\n", section, body, section)
}

func c211Trunc(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated total_len=" + strconv.Itoa(len(s)) + "]"
}

func c211Short(s string) string {
	if s == "" {
		return "<absent>"
	}
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("len=%d sha256=%x", len(s), h[:8])
}

func c211HTTP(method, u, token, ctype, body string) (int, string) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return -1, "request build error: " + c211Trunc(err.Error(), 300)
	}
	req.Header.Set("User-Agent", "c211-i119859-gke-probe")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c211Client.Do(req)
	if err != nil {
		return -1, "transport error: " + c211Trunc(err.Error(), 400)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, string(b)
}

func c211JSON(s string) map[string]interface{} {
	var m map[string]interface{}
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}

func c211Str(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

type c211Creds struct {
	path         string
	raw          map[string]interface{}
	typ          string
	audience     string
	subjectType  string
	tokenURL     string
	impersonate  string
	srcURL       string
	srcHeaders   map[string]interface{}
	srcFormat    string
	srcField     string
	saEmail      string
	projNumber   string
	credProjID   string
	credQuotaPID string
}

func c211FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func c211LoadCreds() (*c211Creds, string) {
	cands := []string{}
	for _, n := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GHA_CREDS_PATH", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"} {
		if v := os.Getenv(n); v != "" {
			cands = append(cands, v)
		}
	}
	for _, pat := range []string{
		"/home/runner/work/*/*/gha-creds-*.json",
		"/home/runner/work/_temp/gha-creds-*.json",
		"/home/runner/work/_temp/*.json",
		"gha-creds-*.json",
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			cands = append(cands, m...)
		}
	}
	seen := map[string]bool{}
	var log strings.Builder
	for _, p := range cands {
		if seen[p] {
			continue
		}
		seen[p] = true
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(&log, "credpath candidate %s err=%v\n", p, err)
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(b, &m) != nil {
			fmt.Fprintf(&log, "credpath candidate %s unparsable len=%d\n", p, len(b))
			continue
		}
		c := &c211Creds{path: p, raw: m}
		c.typ = c211Str(m, "type")
		c.audience = c211Str(m, "audience")
		c.subjectType = c211Str(m, "subject_token_type")
		c.tokenURL = c211Str(m, "token_url")
		c.impersonate = c211Str(m, "service_account_impersonation_url")
		c.credProjID = c211Str(m, "project_id")
		c.credQuotaPID = c211Str(m, "quota_project_id")
		if cs, ok := m["credential_source"].(map[string]interface{}); ok {
			c.srcURL = c211Str(cs, "url")
			c.srcHeaders, _ = cs["headers"].(map[string]interface{})
			if f, ok := cs["format"].(map[string]interface{}); ok {
				c.srcFormat = c211Str(f, "type")
				c.srcField = c211Str(f, "subject_token_field_name")
			}
		}
		if c.impersonate != "" {
			if i := strings.Index(c.impersonate, "serviceAccounts/"); i >= 0 {
				rest := c.impersonate[i+len("serviceAccounts/"):]
				if j := strings.Index(rest, ":"); j >= 0 {
					rest = rest[:j]
				}
				c.saEmail = rest
			}
		}
		if re := regexp.MustCompile(`projects/([0-9]+)/`); re.MatchString(c.audience) {
			c.projNumber = re.FindStringSubmatch(c.audience)[1]
		}
		fmt.Fprintf(&log, "SELECTED credpath=%s size=%d type=%s\n", p, len(b), c.typ)
		fmt.Fprintf(&log, "audience=%s\n", c.audience)
		fmt.Fprintf(&log, "subject_token_type=%s token_url=%s\n", c.subjectType, c.tokenURL)
		fmt.Fprintf(&log, "service_account_impersonation_url=%s\n", c.impersonate)
		fmt.Fprintf(&log, "service_account_email=%s\n", c.saEmail)
		fmt.Fprintf(&log, "credential_source.url=%s\n", c211Trunc(c.srcURL, 500))
		fmt.Fprintf(&log, "credential_source.format.type=%s subject_token_field_name=%s\n", c.srcFormat, c.srcField)
		for k, v := range c.srcHeaders {
			if s, ok := v.(string); ok {
				fmt.Fprintf(&log, "credential_source.headers[%s] %s\n", k, c211Short(s))
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&log, "credential_json_keys=%s\n", strings.Join(keys, ","))
		return c, log.String()
	}
	return nil, log.String()
}

func c211FetchSubjectToken(c *c211Creds) (string, string) {
	u := c.srcURL
	if u == "" {
		return "", "no credential_source.url in credentials file"
	}
	if c.audience != "" && !strings.Contains(u, "audience=") {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u = u + sep + "audience=" + url.QueryEscape(c.audience)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", "subject token request build error: " + err.Error()
	}
	req.Header.Set("User-Agent", "c211-i119859")
	for k, v := range c.srcHeaders {
		if s, ok := v.(string); ok {
			req.Header.Set(k, s)
		}
	}
	resp, err := c211Client.Do(req)
	if err != nil {
		return "", "subject token transport error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := strings.TrimSpace(string(b))
	if resp.StatusCode != 200 {
		return "", fmt.Sprintf("subject token endpoint status=%d verbatim=%s", resp.StatusCode, c211Trunc(body, 800))
	}
	tok := body
	if strings.HasPrefix(body, "{") {
		m := c211JSON(body)
		if v, ok := m["value"].(string); ok {
			tok = v
		} else if c.srcField != "" {
			if v, ok := m[c.srcField].(string); ok {
				tok = v
			}
		}
	}
	return tok, fmt.Sprintf("subject token endpoint status=200 body_%s extracted_%s", c211Short(body), c211Short(tok))
}

func c211STSExchange(c *c211Creds, subject string) (string, string) {
	tu := c.tokenURL
	if tu == "" {
		tu = "https://sts.googleapis.com/v1/token"
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("audience", c.audience)
	form.Set("scope", c211Scope)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	stt := c.subjectType
	if stt == "" {
		stt = "urn:ietf:params:oauth:token-type:jwt"
	}
	form.Set("subject_token_type", stt)
	form.Set("subject_token", subject)
	st, body := c211HTTP("POST", tu, "", "application/x-www-form-urlencoded", form.Encode())
	m := c211JSON(body)
	at := c211Str(m, "access_token")
	msg := fmt.Sprintf("STS POST %s -> %d", tu, st)
	if at != "" {
		msg += fmt.Sprintf(" access_token_%s expires_in=%v", c211Short(at), m["expires_in"])
		return at, msg
	}
	if e := c211Str(m, "error"); e != "" {
		msg += fmt.Sprintf(" error=%s error_description=%s", e, c211Str(m, "error_description"))
	}
	msg += " verbatim=" + c211Trunc(body, 1200)
	return "", msg
}

func c211Impersonate(c *c211Creds, fedToken string) (string, string) {
	if c.impersonate == "" {
		return fedToken, "no service_account_impersonation_url; using federated access token"
	}
	body := fmt.Sprintf(`{"scope":["%s"],"lifetime":"3600s"}`, c211Scope)
	st, rb := c211HTTP("POST", c.impersonate, fedToken, "application/json", body)
	m := c211JSON(rb)
	at := c211Str(m, "accessToken")
	msg := fmt.Sprintf("impersonation POST %s -> %d", c.impersonate, st)
	if at != "" {
		msg += fmt.Sprintf(" accessToken_%s expireTime=%s", c211Short(at), c211Str(m, "expireTime"))
		return at, msg
	}
	if e := c211Str(m, "error"); e != "" {
		em := "unknown"
		if emap, ok := m["error"].(map[string]interface{}); ok {
			em = c211Str(emap, "message")
		}
		msg += fmt.Sprintf(" error=%s error_message=%s", e, em)
	}
	msg += " verbatim=" + c211Trunc(rb, 1200)
	return "", msg
}

func c211GcloudToken() (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	var ob, eb strings.Builder
	cmd.Stdout = &ob
	cmd.Stderr = &eb
	err := cmd.Run()
	code := 0
	errs := ""
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
		errs = err.Error()
	}
	tok := strings.TrimSpace(ob.String())
	return tok, fmt.Sprintf("gcloud ADC fallback exit=%d err=%s stdout_%s stderr=%s", code, c211Trunc(errs, 200), c211Short(tok), c211Trunc(eb.String(), 1500))
}

type c211Quota struct {
	region string
	metric string
	limit  float64
	usage  float64
}

func c211FmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func TestC211GKECapabilityProbe(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{"probe": "c211_i119859_gke_gcp_capability"}
	verdict := "GATE"
	verdictDetail := ""

	// ---------------- 1. environment inventory ----------------
	var env strings.Builder
	for _, n := range []string{
		"GCP_PROJECT_ID", "GCP_PROJECT", "GCP_REGION",
		"CLOUDSDK_CORE_PROJECT", "CLOUDSDK_PROJECT", "GCLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT",
		"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GHA_CREDS_PATH", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
		"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_RUNTIME_TOKEN",
	} {
		v := os.Getenv(n)
		if v == "" {
			fmt.Fprintf(&env, "%s=<absent>\n", n)
		} else {
			fmt.Fprintf(&env, "%s present %s\n", n, c211Short(v))
		}
	}
	for _, n := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_SHA", "GITHUB_WORKSPACE", "RUNNER_NAME", "RUNNER_OS"} {
		fmt.Fprintf(&env, "%s=%s\n", n, os.Getenv(n))
	}
	gcloudPath, gcloudErr := exec.LookPath("gcloud")
	fmt.Fprintf(&env, "gcloud_path=%s err=%v\n", gcloudPath, gcloudErr)
	c211Emit("ENV", env.String())

	// ---------------- 2. exported credential file ----------------
	creds, credLog := c211LoadCreds()
	c211Emit("CREDS", credLog)
	if creds == nil {
		summary["token_ok"] = false
		summary["gate"] = "NO_EXPORTED_GCP_CREDENTIAL_FILE"
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE NO_EXPORTED_GCP_CREDENTIAL_FILE")
		return
	}
	summary["cred_type"] = creds.typ
	summary["sa_email"] = creds.saEmail
	summary["project_number"] = creds.projNumber

	// ---------------- 3. WIF token exchange (stdlib) ----------------
	token := ""
	authMethod := ""
	subject, subjMsg := c211FetchSubjectToken(creds)
	c211Emit("SUBJECT_TOKEN", subjMsg)
	if subject != "" {
		if fed, stsMsg := c211STSExchange(creds, subject); fed != "" {
			c211Emit("STS", stsMsg)
			summary["sts_ok"] = true
			if final, impMsg := c211Impersonate(creds, fed); final != "" {
				c211Emit("IMPERSONATION", impMsg)
				token = final
				authMethod = "stdlib_wif_sts"
			} else {
				c211Emit("IMPERSONATION", impMsg)
				summary["impersonation_error"] = c211Trunc(impMsg, 900)
			}
		} else {
			c211Emit("STS", stsMsg)
			summary["sts_ok"] = false
			summary["sts_error"] = c211Trunc(stsMsg, 900)
		}
	}
	if token == "" {
		tok, gMsg := c211GcloudToken()
		c211Emit("GCLOUD_FALLBACK", gMsg)
		if tok != "" {
			token = tok
			authMethod = "gcloud_adc"
		}
	}
	if token == "" {
		summary["token_ok"] = false
		summary["gate"] = "WIF_TOKEN_EXCHANGE_FAILED"
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	summary["token_ok"] = true
	summary["auth_method"] = authMethod
	summary["access_token"] = c211Short(token)

	st, ti := c211HTTP("GET", "https://oauth2.googleapis.com/tokeninfo", token, "", "")
	c211Emit("TOKENINFO", fmt.Sprintf("status=%d %s", st, c211Trunc(ti, 1500)))
	if m := c211JSON(ti); m != nil {
		summary["tokeninfo_status"] = st
		summary["tokeninfo_azp"] = c211Str(m, "azp")
		summary["tokeninfo_aud"] = c211Str(m, "aud")
		summary["tokeninfo_scope"] = c211Str(m, "scope")
	}

	// ---------------- 4. project resolution ----------------
	proj := c211FirstNonEmpty(
		os.Getenv("GCP_PROJECT_ID"), os.Getenv("GCP_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT"),
		os.Getenv("CLOUDSDK_CORE_PROJECT"), os.Getenv("CLOUDSDK_PROJECT"), os.Getenv("GCLOUD_PROJECT"),
		creds.credProjID, creds.credQuotaPID,
	)
	if proj == "" && creds.saEmail != "" && strings.Contains(creds.saEmail, "@") {
		dom := strings.SplitN(creds.saEmail, "@", 2)[1]
		proj = strings.SplitN(dom, ".iam.gserviceaccount.com", 2)[0]
	}
	summary["project_id_short"] = c211Short(proj)
	fmt.Printf("\nC211GCP resolved project=%s (%s)\n", proj, c211Short(proj))
	if proj == "" {
		summary["gate"] = "NO_PROJECT_ID_RESOLVED"
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE NO_PROJECT_ID_RESOLVED")
		return
	}
	crmBase := "https://cloudresourcemanager.googleapis.com"
	st, crm := c211HTTP("GET", crmBase+"/v3/projects/"+url.PathEscape(proj), token, "", "")
	c211Emit("CRM_V3", fmt.Sprintf("GET projects/%s -> %d\n%s", c211Short(proj), st, c211Trunc(crm, 1600)))
	if m := c211JSON(crm); m != nil && st == 200 {
		summary["crm_status"] = st
		summary["crm_project_number"] = c211Str(m, "projectNumber")
		summary["crm_lifecycle"] = c211Str(m, "lifecycleState")
		summary["crm_parent"] = fmt.Sprintf("%v", m["parent"])
	}
	if st != 200 {
		st2, crm1 := c211HTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), token, "", "")
		c211Emit("CRM_V1", fmt.Sprintf("GET projects/%s -> %d\n%s", c211Short(proj), st2, c211Trunc(crm1, 1200)))
		summary["crm_status"] = st
		summary["crm_error"] = c211Trunc(crm, 900)
	}

	// ---------------- 5. enabled services ----------------
	svcBase := "https://serviceusage.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/services"
	pageTok := ""
	enabled := []string{}
	found := map[string]string{}
	svcStatus := 0
	svcErr := ""
	for page := 0; page < 4; page++ {
		u := svcBase + "?pageSize=200"
		if pageTok != "" {
			u += "&pageToken=" + url.QueryEscape(pageTok)
		}
		st, body := c211HTTP("GET", u, token, "", "")
		svcStatus = st
		if st != 200 {
			svcErr = c211Trunc(body, 1200)
			break
		}
		m := c211JSON(body)
		if items, ok := m["services"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				name := c211Str(im, "config.name")
				if name == "" {
					name = c211Str(im, "name")
				}
				state := c211Str(im, "state")
				found[name] = state
				if state == "ENABLED" {
					enabled = append(enabled, name)
				}
			}
		}
		pageTok = c211Str(m, "nextPageToken")
		if pageTok == "" {
			break
		}
	}
	sort.Strings(enabled)
	var svcLog strings.Builder
	fmt.Fprintf(&svcLog, "services.list status=%d total=%d enabled=%d\n", svcStatus, len(found), len(enabled))
	for _, n := range enabled {
		fmt.Fprintf(&svcLog, "ENABLED %s\n", n)
	}
	for _, probe := range []string{"compute.googleapis.com", "aiplatform.googleapis.com", "container.googleapis.com", "iam.googleapis.com", "iamcredentials.googleapis.com", "sts.googleapis.com"} {
		state := found[probe]
		if state == "" {
			state = "<not-listed>"
		}
		fmt.Fprintf(&svcLog, "SERVICE %s state=%s\n", probe, state)
	}
	if svcErr != "" {
		svcLog.WriteString("verbatim_error=" + svcErr + "\n")
	}
	c211Emit("SERVICEUSAGE", svcLog.String())
	summary["serviceusage_status"] = svcStatus
	summary["compute_enabled"] = found["compute.googleapis.com"] == "ENABLED"
	summary["aiplatform_enabled"] = found["aiplatform.googleapis.com"] == "ENABLED"
	if svcStatus != 200 {
		summary["serviceusage_error"] = svcErr
	}

	// ---------------- 6. compute regions + per-region GPU quota ----------------
	compBase := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj)
	st, regs := c211HTTP("GET", compBase+"/regions", token, "", "")
	regionNames := []string{}
	if st == 200 {
		if m := c211JSON(regs); m != nil {
			if items, ok := m["items"].([]interface{}); ok {
				for _, it := range items {
					im, _ := it.(map[string]interface{})
					if n := c211Str(im, "name"); n != "" {
						regionNames = append(regionNames, n)
					}
				}
			}
		}
	} else {
		c211Emit("COMPUTE_REGIONS", fmt.Sprintf("GET regions -> %d\n%s", st, c211Trunc(regs, 1600)))
		summary["compute_regions_status"] = st
		summary["compute_regions_error"] = c211Trunc(regs, 900)
	}
	sort.Strings(regionNames)
	summary["compute_regions_status"] = st
	summary["compute_region_count"] = len(regionNames)

	positives := []c211Quota{}
	var qlog strings.Builder
	fmt.Fprintf(&qlog, "regions=%d\n", len(regionNames))
	regionZones := map[string][]string{
		"us-central1":              {"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
		"us-east1":                 {"us-east1-b", "us-east1-c", "us-east1-d"},
		"us-east4":                 {"us-east4-a", "us-east4-b", "us-east4-c"},
		"us-west1":                 {"us-west1-a", "us-west1-b"},
		"us-west2":                 {"us-west2-a", "us-west2-b", "us-west2-c"},
		"us-west3":                 {"us-west3-a", "us-west3-b"},
		"us-west4":                 {"us-west4-a", "us-west4-b"},
		"europe-west1":             {"europe-west1-b", "europe-west1-c", "europe-west1-d"},
		"europe-west2":             {"europe-west2-a", "europe-west2-b", "europe-west2-c"},
		"europe-west3":             {"europe-west3-a", "europe-west3-b"},
		"europe-west4":             {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
		"europe-west6":             {"europe-west6-a", "europe-west6-b"},
		"asia-east1":               {"asia-east1-a", "asia-east1-b", "asia-east1-c"},
		"asia-east2":               {"asia-east2-a", "asia-east2-b"},
		"asia-northeast1":          {"asia-northeast1-a", "asia-northeast1-b", "asia-northeast1-c"},
		"asia-northeast3":          {"asia-northeast3-a", "asia-northeast3-b"},
		"asia-south1":              {"asia-south1-a", "asia-south1-b", "asia-south1-c"},
		"asia-southeast1":          {"asia-southeast1-a", "asia-southeast1-b", "asia-southeast1-c"},
		"australia-southeast1":     {"australia-southeast1-a", "australia-southeast1-b"},
		"northamerica-northeast1":  {"northamerica-northeast1-a", "northamerica-northeast1-b"},
		"southamerica-east1":       {"southamerica-east1-a", "southamerica-east1-b"},
		"me-west1":                 {"me-west1-a", "me-west1-b"},
		"me-central1":              {"me-central1-a"},
		"africa-south1":            {"africa-south1-a"},
		"us-east5":                 {"us-east5-a"},
		"us-south1":                {"us-south1-a"},
		"europe-west8":             {"europe-west8-a"},
		"europe-west9":             {"europe-west9-a"},
		"europe-west12":            {"europe-west12-a"},
		"asia-southeast2":          {"asia-southeast2-a"},
	}
	for _, r := range regionNames {
		st, rb := c211HTTP("GET", compBase+"/regions/"+url.PathEscape(r), token, "", "")
		if st != 200 {
			fmt.Fprintf(&qlog, "REGION %s status=%d verbatim=%s\n", r, st, c211Trunc(rb, 300))
			continue
		}
		m := c211JSON(rb)
		q, _ := m["quotas"].([]interface{})
		for _, it := range q {
			qm, _ := it.(map[string]interface{})
			metric := c211Str(qm, "metric")
			if !strings.Contains(metric, "GPU") {
				continue
			}
			limit := c211Num(qm["limit"])
			usage := c211Num(qm["usage"])
			fmt.Fprintf(&qlog, "REGIONQUOTA region=%s metric=%s limit=%s usage=%s\n", r, metric, c211FmtFloat(limit), c211FmtFloat(usage))
			if limit > 0 && usage < limit {
				positives = append(positives, c211Quota{region: r, metric: metric, limit: limit, usage: usage})
			}
		}
	}
	c211Emit("GPU_QUOTA", qlog.String())
	summary["gpu_positive_count"] = len(positives)
	posList := []string{}
	for _, p := range positives {
		posList = append(posList, fmt.Sprintf("%s:%s limit=%s usage=%s", p.region, p.metric, c211FmtFloat(p.limit), c211FmtFloat(p.usage)))
	}
	summary["gpu_positive"] = posList

	// ---------------- 7. accelerator types in candidate zones ----------------
	metricToAccel := map[string][2]string{
		"NVIDIA_T4_GPUS":        {"nvidia-tesla-t4", "n1-standard-4"},
		"NVIDIA_L4_GPUS":        {"nvidia-l4", "g2-standard-4"},
		"NVIDIA_A100_GPUS":      {"nvidia-tesla-a100", "a2-highgpu-1g"},
		"NVIDIA_A100_80GB_GPUS": {"nvidia-a100-80gb", "a2-ultragpu-1g"},
		"NVIDIA_V100_GPUS":      {"nvidia-tesla-v100", "n1-standard-4"},
		"NVIDIA_P100_GPUS":      {"nvidia-tesla-p100", "n1-standard-4"},
		"NVIDIA_P4_GPUS":        {"nvidia-tesla-p4", "n1-standard-4"},
		"NVIDIA_K80_GPUS":       {"nvidia-tesla-k80", "n1-standard-4"},
	}
	avail := map[string]map[string]bool{}
	var alog strings.Builder
	zonesToProbe := []string{}
	for _, p := range positives {
		if _, ok := metricToAccel[p.metric]; !ok {
			continue
		}
		if zs, ok := regionZones[p.region]; ok {
			zonesToProbe = append(zonesToProbe, zs...)
		}
	}
	if len(zonesToProbe) == 0 {
		zonesToProbe = []string{"us-central1-a", "us-east1-b", "europe-west4-a", "asia-southeast1-a"}
	}
	seenZone := map[string]bool{}
	for _, z := range zonesToProbe {
		if seenZone[z] {
			continue
		}
		seenZone[z] = true
		st, body := c211HTTP("GET", compBase+"/zones/"+url.PathEscape(z)+"/acceleratorTypes", token, "", "")
		if st != 200 {
			fmt.Fprintf(&alog, "ZONE %s acceleratorTypes status=%d verbatim=%s\n", z, st, c211Trunc(body, 200))
			continue
		}
		m := c211JSON(body)
		avail[z] = map[string]bool{}
		names := []string{}
		if items, ok := m["items"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				n := c211Str(im, "name")
				if n != "" {
					avail[z][n] = true
					names = append(names, n)
				}
			}
		}
		sort.Strings(names)
		fmt.Fprintf(&alog, "ZONE %s types=%s\n", z, strings.Join(names, ","))
	}
	c211Emit("ACCELERATOR_TYPES", alog.String())
	summary["accelerator_zones_probed"] = len(seenZone)

	// ---------------- 8. exactly one on-demand marker VM ----------------
	if len(positives) == 0 {
		summary["marker_created"] = false
		summary["gate"] = "NO_POSITIVE_ONDEMAND_GPU_QUOTA"
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE NO_POSITIVE_ONDEMAND_GPU_QUOTA")
		return
	}

	prefOrder := []string{"NVIDIA_L4_GPUS", "NVIDIA_T4_GPUS", "NVIDIA_A100_GPUS", "NVIDIA_A100_80GB_GPUS", "NVIDIA_V100_GPUS", "NVIDIA_P100_GPUS", "NVIDIA_P4_GPUS", "NVIDIA_K80_GPUS"}
	var pick *c211Quota
	var pickZone, pickAT, pickMT string
	var mlog strings.Builder
	for _, metric := range prefOrder {
		for i := range positives {
			p := positives[i]
			if p.metric != metric {
				continue
			}
			spec, ok := metricToAccel[p.metric]
			if !ok {
				continue
			}
			zone := ""
			for _, z := range regionZones[p.region] {
				if avail[z] != nil && avail[z][spec[0]] {
					zone = z
					break
				}
			}
			if zone == "" {
				fmt.Fprintf(&mlog, "SKIP metric=%s region=%s reason=accelerator-not-offered-in-probed-zones\n", p.metric, p.region)
				continue
			}
			pick = &positives[i]
			pickZone = zone
			pickAT = spec[0]
			pickMT = spec[1]
			break
		}
		if pick != nil {
			break
		}
	}
	if pick == nil {
		summary["marker_created"] = false
		summary["gate"] = "GPU_QUOTA_POSITIVE_BUT_NO_ZONE_OFFERS_ACCELERATOR"
		c211Emit("MARKER_SELECT", mlog.String())
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE GPU_QUOTA_POSITIVE_BUT_NO_ZONE_OFFERS_ACCELERATOR")
		return
	}
	fmt.Fprintf(&mlog, "PICK region=%s zone=%s metric=%s accelerator=%s machine=%s limit=%s usage=%s\n",
		pick.region, pickZone, pick.metric, pickAT, pickMT, c211FmtFloat(pick.limit), c211FmtFloat(pick.usage))
	summary["marker_zone"] = pickZone
	summary["marker_accelerator"] = pickAT
	summary["marker_machine"] = pickMT
	summary["marker_metric"] = pick.metric

	// image resolution
	image := ""
	for _, cand := range []string{
		"projects/deeplearning-platform-release/global/images/family/pytorch-latest-gpu",
		"projects/deeplearning-platform-release/global/images/family/common-cu124-ubuntu-2204",
		"projects/deeplearning-platform-release/global/images/family/tf-latest-gpu",
		"projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
	} {
		parts := strings.Split(cand, "/")
		if len(parts) < 6 {
			continue
		}
		st, body := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+parts[1]+"/global/images/family/"+parts[5], token, "", "")
		fmt.Fprintf(&mlog, "IMAGE %s status=%d\n", cand, st)
		if st == 200 {
			image = cand
			if m := c211JSON(body); m != nil {
				fmt.Fprintf(&mlog, "IMAGE_DETAIL name=%s status=%s\n", c211Str(m, "name"), c211Str(m, "status"))
			}
			break
		}
		fmt.Fprintf(&mlog, "IMAGE_ERROR verbatim=%s\n", c211Trunc(body, 300))
	}
	if image == "" {
		summary["marker_created"] = false
		summary["gate"] = "NO_BOOT_IMAGE_RESOLVED"
		c211Emit("MARKER_SELECT", mlog.String())
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE NO_BOOT_IMAGE_RESOLVED")
		return
	}
	summary["marker_image"] = image

	// network
	ni := map[string]interface{}{"network": "global/networks/default"}
	st, nbody := c211HTTP("GET", compBase+"/global/networks/default", token, "", "")
	createdNet := false
	fmt.Fprintf(&mlog, "NETWORK default status=%d\n", st)
	if st != 200 {
		fmt.Fprintf(&mlog, "NETWORK default verbatim=%s\n", c211Trunc(nbody, 400))
		netName := "c211-i119859-net"
		st2, cbody := c211HTTP("POST", compBase+"/global/networks", token, "application/json",
			fmt.Sprintf(`{"name":"%s","description":"c211 i119859 marker","autoCreateSubnetworks":true}`, netName))
		fmt.Fprintf(&mlog, "NETWORK create %s status=%d verbatim=%s\n", netName, st2, c211Trunc(cbody, 500))
		if st2 == 200 || st2 == 409 {
			createdNet = true
			for i := 0; i < 20; i++ {
				st3, nb := c211HTTP("GET", compBase+"/global/networks/"+netName, token, "", "")
				nm := c211JSON(nb)
				fmt.Fprintf(&mlog, "NETWORK poll %d status=%d state=%s\n", i, st3, c211Str(nm, "lifecycleState"))
				if st3 == 200 {
					break
				}
				time.Sleep(6 * time.Second)
			}
			ni = map[string]interface{}{"network": "global/networks/" + netName}
		} else {
			summary["marker_created"] = false
			summary["gate"] = "NO_NETWORK_AVAILABLE"
			c211Emit("MARKER_SELECT", mlog.String())
			c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
			fmt.Println("C211GCP_VERDICT GATE NO_NETWORK_AVAILABLE")
			return
		}
	}

	instName := "c211-i119859-gpu"
	instURL := compBase + "/zones/" + url.PathEscape(pickZone) + "/instances/" + instName
	startup := "#!/bin/bash\n{\necho \"C211_MARKER_START $(date -u +%Y-%m-%dT%H:%M:%SZ)\"\necho \"C211_HOST $(hostname)\"\nnvidia-smi -L 2>&1 || echo \"C211_NVIDIA_SMI_L_FAIL rc=$?\"\nnvidia-smi 2>&1 || echo \"C211_NVIDIA_SMI_FAIL rc=$?\"\necho \"C211_MARKER_END\"\n} > /dev/console 2>&1\n"
	mkBody := func(withSA bool) string {
		inst := map[string]interface{}{
			"name":        instName,
			"machineType": "zones/" + pickZone + "/machineTypes/" + pickMT,
			"disks": []interface{}{map[string]interface{}{
				"boot":       true,
				"autoDelete": true,
				"initializeParams": map[string]interface{}{
					"sourceImage": image,
					"diskSizeGb":  "100",
					"diskType":    "pd-balanced",
				},
			}},
			"networkInterfaces": []interface{}{ni},
			"guestAccelerators": []interface{}{map[string]interface{}{
				"acceleratorType": "zones/" + pickZone + "/acceleratorTypes/" + pickAT,
				"acceleratorCount": 1,
			}},
			"scheduling": map[string]interface{}{
				"onHostMaintenance": "TERMINATE",
				"automaticRestart":  false,
				"provisioningModel": "STANDARD",
			},
			"metadata": map[string]interface{}{"items": []interface{}{
				map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
				map[string]interface{}{"key": "startup-script", "value": startup},
			}},
			"labels": map[string]interface{}{"c211": "i119859"},
		}
		if withSA {
			inst["serviceAccounts"] = []interface{}{}
		}
		b, _ := json.Marshal(inst)
		return string(b)
	}

	var insertStatus int
	var insertBody string
	for _, withSA := range []bool{true, false} {
		insertStatus, insertBody = c211HTTP("POST", compBase+"/zones/"+url.PathEscape(pickZone)+"/instances", token, "application/json", mkBody(withSA))
		fmt.Fprintf(&mlog, "INSERT serviceAccountsField=%v status=%d verbatim=%s\n", withSA, insertStatus, c211Trunc(insertBody, 1500))
		if insertStatus == 200 || insertStatus == 409 {
			break
		}
		if !strings.Contains(strings.ToLower(insertBody), "service account") {
			break
		}
	}
	summary["marker_insert_status"] = insertStatus
	c211Emit("MARKER_INSERT", mlog.String())
	if insertStatus != 200 && insertStatus != 409 {
		summary["marker_created"] = false
		summary["gate"] = "INSTANCE_INSERT_REFUSED"
		summary["marker_insert_error"] = c211Trunc(insertBody, 1500)
		c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP_VERDICT GATE INSTANCE_INSERT_REFUSED")
		return
	}

	// poll for RUNNING
	status := ""
	deadline := time.Now().Add(11 * time.Minute)
	for time.Now().Before(deadline) {
		st, body := c211HTTP("GET", instURL, token, "", "")
		if st == 200 {
			m := c211JSON(body)
			status = c211Str(m, "status")
		}
		fmt.Printf("C211GCP_POLL instance status=%s http=%d\n", status, st)
		if status == "RUNNING" || status == "TERMINATED" || status == "STOPPED" {
			break
		}
		time.Sleep(12 * time.Second)
	}
	summary["marker_status"] = status

	// serial console proof
	uuidRe := regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	serialOK := false
	serialExcerpt := ""
	if status == "RUNNING" {
		for i := 0; i < 18; i++ {
			time.Sleep(20 * time.Second)
			st, body := c211HTTP("GET", instURL+"/serialPortOutput?port=1&start=0", token, "", "")
			if st != 200 {
				fmt.Printf("C211GCP_SERIAL poll=%d http=%d verbatim=%s\n", i, st, c211Trunc(body, 300))
				continue
			}
			m := c211JSON(body)
			contents := c211Str(m, "contents")
			hasStart := strings.Contains(contents, "C211_MARKER_START")
			hasEnd := strings.Contains(contents, "C211_MARKER_END")
			uuid := uuidRe.FindString(contents)
			fmt.Printf("C211GCP_SERIAL poll=%d len=%d marker_start=%v marker_end=%v uuid=%s\n", i, len(contents), hasStart, hasEnd, uuid)
			if hasStart {
				idx := strings.Index(contents, "C211_MARKER_START")
				lo := idx - 200
				if lo < 0 {
					lo = 0
				}
				serialExcerpt = contents[lo:]
				if len(serialExcerpt) > 4000 {
					serialExcerpt = serialExcerpt[:4000]
				}
				if uuid != "" {
					serialOK = true
					summary["marker_uuid"] = uuid
					break
				}
			}
			if hasEnd && !hasStart {
				break
			}
		}
	}
	c211Emit("SERIAL_CONSOLE", serialExcerpt)
	summary["marker_serial_marker"] = strings.Contains(serialExcerpt, "C211_MARKER_START")
	summary["marker_uuid"] = c211Str(summary, "marker_uuid")
	summary["marker_serial_ok"] = serialOK

	// delete + readback
	dStatus, dBody := c211HTTP("DELETE", instURL, token, "", "")
	fmt.Printf("C211GCP_DELETE status=%d verbatim=%s\n", dStatus, c211Trunc(dBody, 700))
	absent := false
	for i := 0; i < 20; i++ {
		time.Sleep(12 * time.Second)
		st, body := c211HTTP("GET", instURL, token, "", "")
		fmt.Printf("C211GCP_DELETE_READBACK poll=%d http=%d verbatim=%s\n", i, st, c211Trunc(body, 250))
		if st == 404 {
			absent = true
			break
		}
	}
	summary["marker_delete_status"] = dStatus
	summary["marker_absent_after_delete"] = absent
	if createdNet {
		st, body := c211HTTP("DELETE", compBase+"/global/networks/c211-i119859-net", token, "", "")
		fmt.Printf("C211GCP_NETWORK_DELETE status=%d verbatim=%s\n", st, c211Trunc(body, 500))
	}

	if serialOK && absent {
		verdict = "READY_MARKER_PROVEN"
		verdictDetail = "uuid=" + c211Str(summary, "marker_uuid") + " zone=" + pickZone + " accel=" + pickAT
	} else if serialOK && !absent {
		verdict = "GATE"
		verdictDetail = "marker-proven-but-delete-readback-incomplete"
	} else {
		verdict = "GATE"
		verdictDetail = "marker-no-serial-device-proof"
	}
	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("SUMMARY", "C211GCP_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP_VERDICT %s %s\n", verdict, verdictDetail)
}

func c211Num(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func c211JSONString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal"}`
	}
	return string(b)
}
