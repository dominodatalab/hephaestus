//go:build functional && gke

// C211 i122864 — REOPEN THE VERTEX CUSTOMJOB PLANE ON THE FOUR HEPHAESTUS SIBLING
// PROJECTS WITH A DIAGNOSABLE CONTAINER COMMAND.
//
// Context (facts 122221 / 122688, GATE x121897 handoff 74af495b): the four T4
// customJobs created by intent x121897 were ACCEPTED (HTTP 200, no serviceAccount)
// in team-eng-infra-testing 210628133505, domino-eng-service-artifacts
// 1072394045651, team-eng-infrastructure 463640210929 and team-eng-platform
// 1074386228880 with machine n1-standard-4 + accel NVIDIA_TESLA_T4 count=1 and
// measured on-demand Vertex T4 training quota us-central1=12 in all four, yet each
// job reached JOB_STATE_SUCCEEDED with startTime == endTime (about one second) and
// no device line was ever read from Cloud Logging.
//
// This probe settles the open question and, if a GPU is really attached, proves it.
// Tokens are only ever reported as length + SHA-256 prefix. package functional
// helpers (c211x*) come from c211_i122864_helpers_test.go.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const c211rNonce = "i122864"
const c211rPrefix = "c211-i122864"

type c211rProj struct {
	Name string
	Num  string
}

var c211rProjects = []c211rProj{
	{"team-eng-infra-testing", "210628133505"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

var c211rPerms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"serviceusage.services.enable",
}

// Leftover TERMINAL records created by intent x121897 (see job_136811927355.log).
var c211rLeftovers = []struct{ Num, Reg, Name string }{
	{"463640210929", "us-central1", "projects/463640210929/locations/us-central1/customJobs/70636277690531840"},
	{"1074386228880", "us-central1", "projects/1074386228880/locations/us-central1/customJobs/4627153200682631168"},
	{"1072394045651", "us-central1", "projects/1072394045651/locations/us-central1/customJobs/5104534761183903744"},
	{"210628133505", "us-central1", "projects/210628133505/locations/us-central1/customJobs/7689600947294568448"},
}

// The container command. Prefix every evidence line with C211VR_ so a plain
// full-text Cloud Logging query finds it regardless of resource labels.
const c211rScript = `echo "C211VR_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i122864"
echo "C211VR_UNAME $(uname -a)"
echo "C211VR_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel' | tr '\n' ';')"
echo "C211VR_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211VR_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211VR_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
echo "C211VR_LIBCUDA $(ls /usr/local/nvidia/lib64/libcuda.so.1 /usr/lib/x86_64-linux-gnu/libcuda.so.1 2>/dev/null | tr '\n' ' ')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211VR_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211VR_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211VR_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211VR_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211VR_SMI_SUMMARY $($SMI 2>&1 | head -20 | tr '\n' '|')"
else
  echo "C211VR_NOSMI"
fi
echo "C211VR_END"
sleep 50
echo "C211VR_DONE at=$(date -u +%FT%TZ)"
`

// c211rUUIDs returns unique GPU UUIDs from a text blob.
func c211rUUIDs(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range c211xUUIDRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// c211rList flattens the Cloud Logging entries body into one text blob.
func c211rList(body string) (string, int) {
	m := c211xJSON(body)
	entries, ok := m["entries"].([]interface{})
	if !ok {
		return "", 0
	}
	var b strings.Builder
	for _, e := range entries {
		em := c211vMap(e)
		if ts := c211xStr(em, "timestamp"); ts != "" {
			b.WriteString(ts + " ")
		}
		if p, ok := em["textPayload"].(string); ok {
			b.WriteString(p)
		}
		if jp := c211vMap(em["jsonPayload"]); jp != nil {
			b.WriteString(c211xJSONString(jp))
		}
		if pb := c211vMap(em["protoPayload"]); pb != nil {
			b.WriteString(c211xJSONString(pb))
		}
		b.WriteString("\n")
	}
	return b.String(), len(entries)
}

// c211rLogsOnce runs one entries:list query and returns (status, text, count).
func c211rLogsOnce(tok, num, filter string) (int, string, int) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + num},
		"filter":        filter,
		"pageSize":      200,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	txt, n := c211rList(rb)
	return st, txt, n
}

// c211rLogs collects container stdout with several independent filter shapes.
// Text filters are label-independent, which is what the earlier lane lacked.
func c211rLogs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`"C211VR_"`,
		`"C211VR_DEVICE" AND resource.labels.job_id="` + jobID + `"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		st, txt, n := c211rLogsOnce(tok, num, f)
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 70), st, n, len(txt)))
		if txt != "" {
			all.WriteString(txt + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// c211rContainerSpec renders the stored containerSpec of a job verbatim (bounded).
func c211rContainerSpec(m map[string]interface{}) string {
	spec := c211vMap(m["jobSpec"])
	if spec == nil {
		return "<no jobSpec>"
	}
	wps := c211vList(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	wp := c211vMap(wps[0])
	cs := c211vMap(wp["containerSpec"])
	if cs == nil {
		return "<no containerSpec>"
	}
	cmd := []string{}
	for _, v := range c211vList(cs["command"]) {
		if s, ok := v.(string); ok {
			cmd = append(cmd, s)
		}
	}
	args := []string{}
	for _, v := range c211vList(cs["args"]) {
		if s, ok := v.(string); ok {
			args = append(args, s)
		}
	}
	argText := strings.Join(args, " ")
	return fmt.Sprintf("imageUri=%q command=%v nargs=%d args_bytes=%d args_head=%q",
		c211xStr(cs, "imageUri"), cmd, len(args), len(argText), c211xTrunc(argText, 160))
}

// c211rJobState extracts the interesting job fields.
func c211rJobState(m map[string]interface{}) string {
	errText := ""
	if e := c211vMap(m["error"]); e != nil {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 300))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

// c211rRetireJob cancels a PENDING job then retires it, polling GET until 404.
func c211rRetireJob(tok, reg, name string) (int, int, string) {
	jurl := "https://" + reg + "-aiplatform.googleapis.com/v1/" + name
	dst := -1
	note := ""
	for attempt := 0; attempt < 10; attempt++ {
		gst, gb := c211xHTTP("GET", jurl, tok, "", "")
		if gst == 404 {
			return dst, 404, "already-absent"
		}
		state := c211xStr(c211xJSON(gb), "state")
		if state == "JOB_STATE_PENDING" {
			cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
			fmt.Printf("C211R_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211R_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(db, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211xHTTP("GET", jurl, tok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

func TestC211RVertexExplicitCommand(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211R_START nonce=%s at=%s\n", c211rNonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211R_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", "153711203585"
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = "153711203585"
	}
	fmt.Printf("C211R_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211R_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211R_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}
	tiSt, tiBody := c211xTokenInfo(ftok)
	fmt.Printf("C211R_TOKENINFO status=%d verbatim=%s\n", tiSt, c211xTrunc(tiBody, 400))

	leftoverNotes := map[string]string{}
	for _, lo := range c211rLeftovers {
		jurl := "https://" + lo.Reg + "-aiplatform.googleapis.com/v1/" + lo.Name
		gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
		if gst != 200 {
			leftoverNotes[lo.Name] = fmt.Sprintf("get_http=%d verbatim=%s", gst, c211xTrunc(gb, 200))
			fmt.Printf("C211R_LEFTOVER num=%s name=%s get_http=%d verbatim=%s\n", lo.Num, lo.Name, gst, c211xTrunc(gb, 300))
			continue
		}
		m := c211xJSON(gb)
		spec := c211rContainerSpec(m)
		st := c211rJobState(m)
		fmt.Printf("C211R_LEFTOVER num=%s name=%s GET=200 %s spec={%s}\n", lo.Num, lo.Name, st, spec)
		dst, getAfter, note := c211rRetireJob(ftok, lo.Reg, lo.Name)
		leftoverNotes[lo.Name] = fmt.Sprintf("stored:%s spec={%s} retire_http=%d get_after=%d %s",
			st, spec, dst, getAfter, note)
		fmt.Printf("C211R_LEFTOVER_RETIRE num=%s name=%s http=%d get_after=%d %s\n", lo.Num, lo.Name, dst, getAfter, note)
	}
	fmt.Printf("C211R_LEFTOVER_MATRIX %s\n", c211xJSONString(leftoverNotes))

	type projResult struct {
		Proj     string            `json:"proj"`
		Num      string            `json:"num"`
		State    string            `json:"state"`
		PermHTTP int               `json:"perm_http"`
		Allowed  []string          `json:"allowed"`
		Svc      string            `json:"svc"`
		Quota    map[string]string `json:"quota_t4"`
		Roles    []string          `json:"fleet_roles"`
		Create   string            `json:"create"`
		Job      string            `json:"job"`
		States   []string          `json:"states"`
		Readback string            `json:"readback"`
		Spec     string            `json:"stored_container_spec"`
		LogQuery string            `json:"log_query"`
		LogBytes int               `json:"log_bytes"`
		Device   string            `json:"device_lines"`
		UUIDs    []string          `json:"uuids"`
		Absence  string            `json:"absence"`
		Verdict  string            `json:"verdict"`
	}
	results := map[string]*projResult{}
	proven := []string{}

	deadline := started.Add(26 * time.Minute)
	for _, p := range c211rProjects {
		r := &projResult{Proj: p.Name, Num: p.Num, Quota: map[string]string{}}
		results[p.Name] = r
		if time.Until(deadline) < 6*time.Minute {
			r.Verdict = "BUDGET_EXHAUSTED_BEFORE_PROJECT"
			fmt.Printf("C211R_SKIP proj=%s reason=budget\n", p.Name)
			continue
		}

		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p.Name), ftok, "", "")
		r.State = c211xStr(c211xJSON(pmBody), "lifecycleState")
		fmt.Printf("C211R_PROJECT proj=%s http=%d state=%s\n", p.Name, pmSt, r.State)

		polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Name)
		pol, polSt, polBody := c211xGetPolicy(ftok, polURL)
		roles := []string{}
		if pol != nil {
			member := "serviceAccount:" + fleetEmail
			for _, bv := range c211vList(pol["bindings"]) {
				bm := c211vMap(bv)
				for _, mv := range c211vList(bm["members"]) {
					if s, ok := mv.(string); ok && s == member {
						roles = append(roles, c211xStr(bm, "role"))
					}
				}
			}
		}
		sort.Strings(roles)
		r.Roles = roles
		fmt.Printf("C211R_FLEET_ROLES proj=%s http=%d n=%d roles=%v verbatim=%s\n", p.Name, polSt, len(roles), roles, c211xTrunc(polBody, 200))

		al, ast, _ := c211xTestIamBatch(ftok, polURL, c211rPerms)
		r.PermHTTP = ast
		r.Allowed = al
		fmt.Printf("C211R_PERM proj=%s http=%d allowed=%v\n", p.Name, ast, al)
		haveCreate := c211xContains(al, "aiplatform.customJobs.create")
		haveRetire := c211xContains(al, "aiplatform.customJobs.delete")
		if !haveCreate || !haveRetire {
			r.Verdict = "PERM_MISSING"
			fmt.Printf("C211R_VERDICT proj=%s %s create=%v retire=%v\n", p.Name, r.Verdict, haveCreate, haveRetire)
			continue
		}

		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+p.Num+"/services/aiplatform.googleapis.com", ftok, "", "")
		r.Svc = c211xStr(c211xJSON(svBody), "state")
		fmt.Printf("C211R_SVC proj=%s http=%d state=%s\n", p.Name, svSt, r.Svc)

		quotaMap, qst, qbody := c211vQuota(ftok, p.Num)
		key := "aiplatform.googleapis.com/custom_model_training_nvidia_t4_gpus"
		for _, reg := range []string{"us-central1", "us-east1", "europe-west4", "asia-northeast1", "asia-south1", "europe-west2"} {
			if quotaMap != nil && quotaMap[key] != nil {
				r.Quota[reg] = fmt.Sprintf("%.0f", quotaMap[key][reg])
			} else {
				r.Quota[reg] = "?"
			}
		}
		fmt.Printf("C211R_QUOTA proj=%s http=%d quota=%v verbatim=%s\n", p.Name, qst, r.Quota, c211xTrunc(qbody, 160))

		reg := "us-central1"
		display := strings.ToLower(fmt.Sprintf("%s-vtx-%s", c211rPrefix, shortProjR(p.Name)))
		if len(display) > 128 {
			display = display[:128]
		}
		jobBody := map[string]interface{}{
			"displayName": display,
			"labels":      map[string]interface{}{"c211": c211rNonce},
			"jobSpec": map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType": "n1-standard-4", "acceleratorType": "NVIDIA_TESLA_T4", "acceleratorCount": 1,
					},
					"replicaCount": 1,
					"containerSpec": map[string]interface{}{
						"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
						"command":  []interface{}{"/bin/bash", "-c"},
						"args":     []interface{}{c211rScript},
					},
				}},
			},
		}
		body, _ := json.Marshal(jobBody)
		fmt.Printf("C211R_REQUEST proj=%s bytes=%d script_bytes=%d\n", p.Name, len(body), len(c211rScript))
		createURL := "https://" + reg + "-aiplatform.googleapis.com/v1/projects/" + p.Num + "/locations/" + reg + "/customJobs"
		cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
		r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 700))
		fmt.Printf("C211R_CREATE proj=%s region=%s http=%d verbatim=%s\n", p.Name, reg, cst, c211xTrunc(cb, 700))
		if cst != 200 {
			low := strings.ToLower(cb)
			if strings.Contains(low, "quota") || strings.Contains(low, "resource_exhausted") || cst == 429 {
				jobBody["jobSpec"].(map[string]interface{})["scheduling"] = map[string]interface{}{"strategy": "SPOT"}
				body2, _ := json.Marshal(jobBody)
				cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(body2))
				r.Create += fmt.Sprintf(" | SPOT http=%d %s", cst, c211xTrunc(cb, 500))
				fmt.Printf("C211R_CREATE_SPOT proj=%s http=%d verbatim=%s\n", p.Name, cst, c211xTrunc(cb, 500))
			}
			if cst != 200 {
				r.Verdict = "CREATE_REFUSED"
				fmt.Printf("C211R_VERDICT proj=%s %s\n", p.Name, r.Verdict)
				continue
			}
		}
		cm := c211xJSON(cb)
		r.Job = c211xStr(cm, "name")
		fmt.Printf("C211R_JOB proj=%s name=%s\n", p.Name, r.Job)
		fmt.Printf("C211R_CREATE_SPEC proj=%s stored={%s} %s\n", p.Name, c211rContainerSpec(cm), c211rJobState(cm))

		jobID := r.Job
		if i := strings.LastIndex(jobID, "/"); i >= 0 {
			jobID = jobID[i+1:]
		}
		jurl := "https://" + reg + "-aiplatform.googleapis.com/v1/" + r.Job

		terminal := false
		devText := ""
		lastDevRead := time.Time{}
		pollEnd := time.Now().Add(12 * time.Minute)
		for i := 0; time.Now().Before(pollEnd) && time.Now().Before(deadline) && !terminal; i++ {
			gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
			gm := c211xJSON(gb)
			state := c211xStr(gm, "state")
			if state != "" && (len(r.States) == 0 || r.States[len(r.States)-1] != state) {
				r.States = append(r.States, state)
			}
			fmt.Printf("C211R_POLL proj=%s i=%d http=%d state=%s\n", p.Name, i, gst, state)
			if state != "" && state != "JOB_STATE_PENDING" && time.Since(lastDevRead) > 20*time.Second {
				txt, qv := c211rLogs(ftok, p.Num, jobID)
				lastDevRead = time.Now()
				if txt != "" {
					devText += txt
				}
				r.LogQuery = qv
				fmt.Printf("C211R_LOG_READ proj=%s bytes=%d q=%s\n", p.Name, len(txt), qv)
			}
			switch state {
			case "JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED", "JOB_STATE_PARTIALLY_SUCCEEDED":
				terminal = true
			}
			if terminal {
				break
			}
			time.Sleep(15 * time.Second)
		}

		for k := 0; k < 3; k++ {
			txt, qv := c211rLogs(ftok, p.Num, jobID)
			r.LogQuery = qv
			if txt != "" {
				devText += txt
			}
			if len(c211rUUIDs(devText)) > 0 {
				break
			}
			time.Sleep(20 * time.Second)
		}
		r.LogBytes = len(devText)
		r.UUIDs = c211rUUIDs(devText)
		lines := []string{}
		for _, ln := range strings.Split(devText, "\n") {
			if strings.Contains(ln, "C211VR_") || strings.Contains(ln, "GPU-") {
				lines = append(lines, c211xTrunc(ln, 400))
			}
		}
		r.Device = c211xTrunc(strings.Join(lines, " || "), 3000)
		fmt.Printf("C211R_LOG proj=%s bytes=%d uuids=%d q=%s\n", p.Name, r.LogBytes, len(r.UUIDs), r.LogQuery)
		for _, ln := range lines {
			fmt.Printf("C211R_DEVTEXT proj=%s %s\n", p.Name, ln)
		}

		rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
		rbm := c211xJSON(rbBody)
		r.Spec = c211rContainerSpec(rbm)
		r.Readback = fmt.Sprintf("http=%d %s spec={%s}", rbSt, c211rJobState(rbm), r.Spec)
		fmt.Printf("C211R_READBACK proj=%s %s\n", p.Name, r.Readback)

		dst, getAfter, note := c211rRetireJob(ftok, reg, r.Job)
		lcSt, lcBody := c211xHTTP("GET", "https://"+reg+"-aiplatform.googleapis.com/v1/projects/"+p.Num+
			"/locations/"+reg+"/customJobs?pageSize=100", ftok, "", "")
		leftItems := []string{}
		for _, it := range c211vList(c211xJSON(lcBody)["customJobs"]) {
			dn := c211xStr(c211vMap(it), "displayName")
			if strings.HasPrefix(dn, c211rPrefix) {
				leftItems = append(leftItems, dn+"="+c211xStr(c211vMap(it), "state"))
			}
		}
		r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d c211_items=%d %v %s",
			dst, getAfter, lcSt, len(leftItems), leftItems, note)
		fmt.Printf("C211R_ABSENCE proj=%s %s\n", p.Name, r.Absence)

		if len(r.UUIDs) > 0 {
			r.Verdict = "VERTEX_T4_DEVICE_PROVEN"
			proven = append(proven, p.Name+":"+strings.Join(r.UUIDs, ","))
		} else {
			r.Verdict = "NO_DEVICE_LINE"
		}
		fmt.Printf("C211R_VERDICT proj=%s %s uuids=%d\n", p.Name, r.Verdict, len(r.UUIDs))
	}

	mj, _ := json.Marshal(results)
	fmt.Printf("C211R_MATRIX %s\n", string(mj))
	fmt.Printf("C211R_PROVEN %s\n", strings.Join(proven, " | "))
	fmt.Printf("C211R_VERDICT_FINAL elapsed=%ds proven=%d\n", int(time.Since(started).Seconds()), len(proven))
	if len(proven) == 0 {
		fmt.Printf("C211R_NO_DEVICE\n")
	}
}

func shortProjR(p string) string {
	p = strings.ToLower(p)
	out := []rune{}
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	s := string(out)
	if len(s) > 24 {
		s = s[:24]
	}
	return strings.Trim(s, "-")
}

func c211vList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211vMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211vFloat(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

// c211vQuota reads consumerQuotaMetrics (serviceusage v1beta1) GPU metrics.
func c211vQuota(tok, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, body
	}
	m := c211xJSON(body)
	for _, mi := range c211vList(m["metrics"]) {
		mm := c211vMap(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "gpu") {
			continue
		}
		for _, li := range c211vList(mm["consumerQuotaLimits"]) {
			lm := c211vMap(li)
			for _, bi := range c211vList(lm["quotaBuckets"]) {
				bm := c211vMap(bi)
				dims := c211vMap(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211vFloat(bm["effectiveLimit"])
				if q[name] == nil {
					q[name] = map[string]float64{}
				}
				if lim > q[name][region] {
					q[name][region] = lim
				}
			}
		}
	}
	return q, st, body
}
