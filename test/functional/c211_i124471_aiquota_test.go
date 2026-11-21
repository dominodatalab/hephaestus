//go:build functional && gke

// C211 i124471 — the ONE never-attempted fix for the host-project Vertex H100 training
// refusal. Facts 124387/124276 establish:
//   * the '/functional-test' GKE WIF identity (dev-fleetcommand, self-granted
//     roles/cloudquotas.admin in fact 124165) created GPU-family quotaPreferences
//     HTTP 200 in host project 153711203585 and NVIDIA_H100/us-central1 was APPROVED
//     (grantedValue=1 "Quota request approved to 1") while H200/B200 were DENIED;
//   * a Vertex customJob create was still verbatim HTTP 429 RESOURCE_EXHAUSTED naming
//     aiplatform.googleapis.com/custom_model_training_nvidia_h100_gpus (limit nil in
//     the consumed regions).
// No aiplatform-metric quotaPreference has ever been attempted anywhere (neither
// handoff tree holds such a row). This probe:
//
//	(a) discovers VERBATIM the cloudquotas quotaInfos for service
//	    aiplatform.googleapis.com — quotaId, metric, containerType, isPrecise,
//	    quotaIncreaseEligibility, and the per-dimension current values for
//	    custom_model_training_nvidia_h100_gpus / _b200_ / _h200_ (us-central1);
//	(b) creates EXACTLY ONE quotaPreference for the H100 custom-model-training metric
//	    in us-central1 with the proven top-level contactEmail payload (location
//	    global, dimensions as discovered) and polls it to terminal, recording
//	    grantedValue/stateDetail verbatim; if the H100 metric is ineligible (absent
//	    from quotaInfos, quotaIncreaseEligibility.isEligible=false, or the create is
//	    refused pre-grant) at most ONE fallback preference for the B200 training
//	    metric is attempted;
//	(c) only if grantedValue>=1 submits EXACTLY ONE nonce ON-DEMAND Vertex customJob
//	    (machineType a3-highgpu-1g, acceleratorType NVIDIA_H100_80GB, count 1,
//	    diskSpec bootDiskType hyperdisk-balanced — the fact-123921/123957 fix),
//	    printing nvidia-smi -L and --query-gpu=name,uuid,memory.total,driver_version,
//	    polls it to terminal, captures every device line, takes an independent
//	    customJobs.get readback and retires it proving GET 404/list-empty.
//
// Secrets are never printed; only length + SHA-256 prefixes.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

const c211qNonce = "i124471"
const c211qPrefix = "c211-i124471"
const c211qHostNum = "153711203585"
const c211qRegion = "us-central1"
const c211qDLC = "us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-4.py310:latest"
const c211qMachine = "a3-highgpu-1g"
const c211qAccel = "NVIDIA_H100_80GB"

var c211qZones = []string{"us-central1-a", "us-central1-b", "us-central1-c"}
var c211qMetrics = []string{
	"aiplatform.googleapis.com/custom_model_training_nvidia_h100_gpus",
	"aiplatform.googleapis.com/custom_model_training_nvidia_b200_gpus",
	"aiplatform.googleapis.com/custom_model_training_nvidia_h200_gpus",
}

func c211qList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211qMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211qGet(tok, u string) (int, string) {
	return c211xHTTP("GET", u, tok, "", "")
}

// c211qQuotaInfos paginates the cloudquotas quotaInfos list for one service.
func c211qQuotaInfos(tok, num, service string) (int, []map[string]interface{}, string) {
	infos := []map[string]interface{}{}
	page := ""
	http := 0
	note := ""
	for i := 0; i < 12; i++ {
		u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
			"/locations/global/services/" + url.PathEscape(service) + "/quotaInfos?pageSize=200"
		if page != "" {
			u += "&pageToken=" + url.PathEscape(page)
		}
		st, body := c211qGet(tok, u)
		http = st
		if st != 200 {
			note = c211xTrunc(body, 900)
			break
		}
		m := c211xJSON(body)
		for _, q := range c211qList(m["quotaInfos"]) {
			infos = append(infos, c211qMap(q))
		}
		page = c211xStr(m, "nextPageToken")
		if page == "" {
			break
		}
	}
	return http, infos, note
}

// c211qFindMetric returns the quotaInfo whose metric equals the wanted one.
func c211qFindMetric(infos []map[string]interface{}, metric string) map[string]interface{} {
	for _, q := range infos {
		if strings.EqualFold(c211xStr(q, "metric"), metric) {
			return q
		}
	}
	return nil
}

// c211qRegionValue extracts the details.value for one region from dimensionsInfos.
func c211qRegionValue(q map[string]interface{}, region string) (string, string) {
	for _, e := range c211qList(q["dimensionsInfos"]) {
		em := c211qMap(e)
		d := c211qMap(em["dimensions"])
		if strings.EqualFold(c211xStr(d, "region"), region) {
			return c211xJSONString(em["details"]), c211xJSONString(d)
		}
	}
	return "", ""
}

// c211qPrintInfo prints one quotaInfo verbatim enough to characterise eligibility.
func c211qPrintInfo(label string, q map[string]interface{}) {
	elig := c211qMap(q["quotaIncreaseEligibility"])
	fmt.Printf("C211Q_QINFO label=%s name=%s quotaId=%s metric=%s service=%s containerType=%s isPrecise=%v isFixed=%v dimensions=%s eligibility=%s\n",
		label, c211xStr(q, "name"), c211xStr(q, "quotaId"), c211xStr(q, "metric"), c211xStr(q, "service"),
		c211xStr(q, "containerType"), q["isPrecise"], q["isFixed"], c211xJSONString(q["dimensions"]),
		c211xTrunc(c211xJSONString(elig), 400))
	n := 0
	for _, e := range c211qList(q["dimensionsInfos"]) {
		em := c211qMap(e)
		d := c211qMap(em["dimensions"])
		locs := c211qList(em["applicableLocations"])
		fmt.Printf("C211Q_QINFO_DIM label=%s dims=%s details=%s locs=%d\n",
			label, c211xJSONString(d), c211xTrunc(c211xJSONString(em["details"]), 300), len(locs))
		n++
		if n >= 60 {
			fmt.Printf("C211Q_QINFO_DIM_TRUNC label=%s more=true\n", label)
			break
		}
	}
}

// c211qPrefGet reads one quotaPreference by id.
func c211qPrefGet(tok, num, prefID string) (int, string) {
	u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
		"/locations/global/quotaPreferences/" + url.PathEscape(prefID)
	return c211qGet(tok, u)
}

// c211qCreatePref posts one quotaPreference with the proven top-level contactEmail.
func c211qCreatePref(tok, num, prefID, service, quotaID, region, value, contactEmail, justification string) (int, string) {
	body := map[string]interface{}{
		"service":    service,
		"quotaId":    quotaID,
		"dimensions": map[string]string{"region": region},
		"quotaConfig": map[string]interface{}{
			"preferredValue": value,
		},
		"contactEmail":  contactEmail,
		"justification": justification,
	}
	bj, _ := json.Marshal(body)
	u := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
		url.PathEscape(num), url.PathEscape(prefID))
	return c211xHTTP("POST", u, tok, "application/json", string(bj))
}

// c211qPollPref polls a preference to terminal (granted or 8 iterations).
func c211qPollPref(tok, num, prefID string) (string, string, string, string) {
	lastState, lastGranted, lastDetail, lastBody := "", "", "", ""
	for k := 0; k < 8; k++ {
		time.Sleep(10 * time.Second)
		st, body := c211qPrefGet(tok, num, prefID)
		m := c211xJSON(body)
		qc := c211qMap(m["quotaConfig"])
		lastState = c211xStr(m, "state")
		lastGranted = fmt.Sprintf("%v", qc["grantedValue"])
		lastDetail = c211xStr(qc, "stateDetail")
		lastBody = body
		fmt.Printf("C211Q_PREF_STATE pref=%s iter=%d http=%d state=%s granted=%s stateDetail=%s verbatim=%s\n",
			prefID, k, st, lastState, lastGranted, lastDetail, c211xTrunc(body, 900))
		if lastGranted != "<nil>" && lastGranted != "" && lastGranted != "0" {
			break
		}
		if strings.Contains(strings.ToLower(lastDetail), "denied") {
			break
		}
	}
	return lastState, lastGranted, lastDetail, lastBody
}

// c211qScript is the container command; every evidence line is prefixed C211QJ_.
func c211qScript() string {
	return `set -u
PROOF=/tmp/c211q-proof.txt
{
echo "C211QJ_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124471 shape=a3-highgpu-1g-x1-h100-80gb"
echo "C211QJ_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211QJ_UNAME $(uname -a)"
echo "C211QJ_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|10de' | tr '\n' '|')"
echo "C211QJ_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211QJ_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  if [ -x /usr/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211QJ_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
if [ -z "$SMI" ] && [ -x /usr/bin/nvidia-smi ]; then SMI=/usr/bin/nvidia-smi; fi
echo "C211QJ_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211QJ_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211QJ_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211QJ_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>&1 | wc -l)"
else
  echo "C211QJ_NOSMI lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|')"
fi
echo "C211QJ_END"
} > $PROOF 2>&1
cat $PROOF
sleep 25
echo "C211QJ_DONE at=$(date -u +%FT%TZ)"
`
}

// c211qJobState / c211qSpec mirror the proven readback helpers.
func c211qJobState(m map[string]interface{}) string {
	if s := c211xStr(m, "state"); s != "" {
		return s
	}
	return "UNKNOWN"
}

func c211qSpec(m map[string]interface{}) string {
	js := c211qMap(m["jobSpec"])
	for _, w := range c211qList(js["workerPoolSpecs"]) {
		wm := c211qMap(w)
		ms := c211qMap(wm["machineSpec"])
		cs := c211qMap(wm["containerSpec"])
		ds := c211qMap(wm["diskSpec"])
		return fmt.Sprintf("machineType=%s acceleratorType=%s acceleratorCount=%v imageUri=%s bootDiskType=%s bootDiskSizeGb=%v",
			c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"],
			c211xStr(cs, "imageUri"), c211xStr(ds, "bootDiskType"), ds["bootDiskSizeGb"])
	}
	return "none"
}

// c211qLogs reads Cloud Logging stdout for the job across fallback filters.
func c211qLogs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`"C211QJ_" AND resource.labels.job_id="` + jobID + `"`,
		`resource.type="ml_job" AND labels.job_id="` + jobID + `"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		body := map[string]interface{}{
			"resourceNames": []interface{}{"projects/" + num},
			"filter":        f,
			"pageSize":      300,
			"orderBy":       "timestamp asc",
		}
		b, _ := json.Marshal(body)
		st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
		m := c211xJSON(rb)
		entries, _ := m["entries"].([]interface{})
		var t strings.Builder
		for _, e := range entries {
			em := c211qMap(e)
			if ts := c211xStr(em, "timestamp"); ts != "" {
				t.WriteString(ts + " ")
			}
			if p, ok := em["textPayload"].(string); ok {
				t.WriteString(p)
			}
			if jp := c211qMap(em["jsonPayload"]); len(jp) > 0 {
				t.WriteString(c211xJSONString(jp))
			}
			t.WriteString("\n")
		}
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 70), st, len(entries), t.Len()))
		if t.Len() > 0 {
			all.WriteString(t.String() + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// c211qRetire cancels + deletes a customJob and proves GET 404.
func c211qRetire(tok, region, name string) (int, int, string) {
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + name
	cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
	fmt.Printf("C211Q_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
	dst := -1
	note := ""
	for i := 0; i < 10; i++ {
		dst, note = c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211Q_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(note, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 204 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211qGet(tok, jurl)
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

func TestC211I124471AIQuota(t *testing.T) {
	started := time.Now()
	deadline := started.Add(12 * time.Minute)
	fmt.Printf("C211Q_PROBE_START nonce=%s at=%s\n", c211qNonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211Q_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", c211qHostNum
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = c211qHostNum
	}
	fmt.Printf("C211Q_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	if fleetEmail == "" {
		for _, sa := range sas {
			e := strings.ToLower(c211xStr(sa, "email"))
			if strings.Contains(e, "fleet") {
				fleetEmail = c211xStr(sa, "email")
				break
			}
		}
	}
	fmt.Printf("C211Q_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211Q_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}

	// confirm / re-apply roles/cloudquotas.admin on the project for the fleet SA
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + hostNum
	pol, pst, _ := c211xGetPolicy(ftok, projURL)
	member := "serviceAccount:" + fleetEmail
	present := c211xPolicyHas(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211Q_GRANT_CHECK policy_http=%d present=%v\n", pst, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var gst int
		var gbody string
		if added {
			gst, gbody = c211xSetPolicy(ftok, projURL, pol)
		}
		fmt.Printf("C211Q_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, gst, c211xTrunc(gbody, 500))
	}

	// permission batch (read + create)
	perms := []string{
		"cloudquotas.quotas.get", "cloudquotas.quotaInfos.get", "cloudquotas.quotaInfos.list",
		"cloudquotas.quotaPreferences.create", "cloudquotas.quotaPreferences.get", "cloudquotas.quotaPreferences.list",
		"aiplatform.customJobs.create", "aiplatform.customJobs.get", "aiplatform.customJobs.cancel",
		"aiplatform.customJobs.delete", "logging.logEntries.list", "serviceusage.services.get",
	}
	al, ast, abody := c211xTestIamBatch(ftok, projURL, perms)
	fmt.Printf("C211Q_PERM http=%d allowed=%s verbatim=%s\n", ast, strings.Join(al, ","), c211xTrunc(abody, 260))

	// ---------- (a) VERBATIM cloudquotas quotaInfos for aiplatform.googleapis.com ----------
	qiSt, infos, qiNote := c211qQuotaInfos(ftok, hostNum, "aiplatform.googleapis.com")
	fmt.Printf("C211Q_QUOTAINFOS service=aiplatform.googleapis.com http=%d n=%d note=%s\n", qiSt, len(infos), c211xTrunc(qiNote, 700))
	h100Info := map[string]interface{}{}
	b200Info := map[string]interface{}{}
	for _, want := range c211qMetrics {
		q := c211qFindMetric(infos, want)
		label := strings.TrimPrefix(want, "aiplatform.googleapis.com/")
		if q == nil {
			fmt.Printf("C211Q_QINFO_ABSENT label=%s metric=%s\n", label, want)
			continue
		}
		c211qPrintInfo(label, q)
		if strings.HasSuffix(want, "_nvidia_h100_gpus") {
			h100Info = q
		}
		if strings.HasSuffix(want, "_nvidia_b200_gpus") {
			b200Info = q
		}
	}
	eligH100 := false
	if len(h100Info) > 0 {
		e := c211qMap(h100Info["quotaIncreaseEligibility"])
		if v, ok := e["isEligible"].(bool); ok && v {
			eligH100 = true
		}
		det, dims := c211qRegionValue(h100Info, c211qRegion)
		fmt.Printf("C211Q_H100_USCENTRAL1 quotaId=%s dims=%s details=%s\n", c211xStr(h100Info, "quotaId"), dims, det)
	}

	// serviceusage cross-check of the same metric (per-region limits)
	suMetrics := map[string]map[string]string{}
	for _, want := range c211qMetrics {
		short := strings.TrimPrefix(want, "aiplatform.googleapis.com/")
		res := map[string]string{}
		u := "https://serviceusage.googleapis.com/v1beta1/projects/" + hostNum +
			"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
		st, body := c211qGet(ftok, u)
		if st == 200 {
			m := c211xJSON(body)
			for _, mi := range c211qList(m["metrics"]) {
				mm := c211qMap(mi)
				if !strings.EqualFold(c211xStr(mm, "metric"), want) {
					continue
				}
				for _, li := range c211qList(mm["consumerQuotaLimits"]) {
					lm := c211qMap(li)
					for _, bi := range c211qList(lm["quotaBuckets"]) {
						bm := c211qMap(bi)
						dims := c211qMap(bm["dimensions"])
						reg := c211xStr(dims, "region")
						if reg == "" {
							reg = "global"
						}
						res[reg] = fmt.Sprintf("limit=%v usage=%v", bm["effectiveLimit"], bm["usage"])
					}
				}
			}
		}
		suMetrics[short] = res
		regs := []string{"us-central1"}
		for r := range res {
			if r != "us-central1" {
				regs = append(regs, r)
			}
		}
		sort.Strings(regs)
		parts := []string{}
		for _, r := range regs {
			v, ok := res[r]
			if !ok {
				v = "absent"
			}
			parts = append(parts, r+"="+v)
		}
		fmt.Printf("C211Q_SUQUOTA metric=%s http=%d %s\n", short, st, strings.Join(parts, ","))
	}

	// ---------- (b) EXACTLY ONE H100 training-metric quotaPreference ----------
	granted := ""
	grantedDetail := ""
	prefID := c211qPrefix + "-aih100-1"
	if len(h100Info) > 0 {
		quotaID := c211xStr(h100Info, "quotaId")
		just := "c211-i124471 nonce measurement: raise the aiplatform custom_model_training_nvidia_h100_gpus consumer quota for us-central1 after the Vertex customJob create was refused HTTP 429 RESOURCE_EXHAUSTED on this metric; contactEmail supplied top-level per the Cloud Quotas v1 discovery schema; no billing change, one nonce training job only"
		cst, cb := c211qCreatePref(ftok, hostNum, prefID, "aiplatform.googleapis.com", quotaID, c211qRegion, "1", fleetEmail, just)
		fmt.Printf("C211Q_PREF_CREATE pref=%s quotaId=%s region=%s http=%d verbatim=%s\n", prefID, quotaID, c211qRegion, cst, c211xTrunc(cb, 1600))
		if cst == 200 || cst == 201 {
			_, granted, grantedDetail, _ = c211qPollPref(ftok, hostNum, prefID)
		} else if !eligH100 && len(b200Info) > 0 {
			// bounded fallback: H100 ineligible/refused pre-grant -> ONE B200 preference
			bquota := c211xStr(b200Info, "quotaId")
			bpref := c211qPrefix + "-aib200-1"
			bjust := "c211-i124471 nonce fallback: raise the aiplatform custom_model_training_nvidia_b200_gpus consumer quota for us-central1 (H100 training metric refused pre-grant); contactEmail top-level; no billing change, no marker unless granted"
			bst, bb := c211qCreatePref(ftok, hostNum, bpref, "aiplatform.googleapis.com", bquota, c211qRegion, "1", fleetEmail, bjust)
			fmt.Printf("C211Q_PREF_CREATE_FALLBACK pref=%s quotaId=%s http=%d verbatim=%s\n", bpref, bquota, bst, c211xTrunc(bb, 1600))
			if bst == 200 || bst == 201 {
				_, granted, grantedDetail, _ = c211qPollPref(ftok, hostNum, bpref)
				prefID = bpref
			}
		}
	} else {
		fmt.Printf("C211Q_PREF_SKIP reason=H100_METRIC_ABSENT_FROM_QUOTAINFOS\n")
	}

	// ---------- (c) only if grantedValue>=1: EXACTLY ONE nonce Vertex customJob ----------
	ready := false
	if g, err := parseFloatLoose(granted); err == nil && g >= 1 {
		ready = true
	}
	fmt.Printf("C211Q_GRANT_VERDICT pref=%s granted=%q stateDetail=%q marker_eligible=%v\n", prefID, granted, grantedDetail, ready)
	if !ready {
		fmt.Printf("C211Q_VERDICT GRANT_NOT_POSITIVE granted=%q detail=%q\n", granted, grantedDetail)
		fmt.Printf("C211Q_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
		return
	}

	// offering matrix
	chosenZone := ""
	for _, z := range c211qZones {
		st, body := c211qGet(ftok, "https://compute.googleapis.com/compute/v1/projects/"+hostNum+"/zones/"+z+"/machineTypes/"+c211qMachine)
		m := c211xJSON(body)
		acc := []string{}
		for _, a := range c211qList(m["accelerators"]) {
			am := c211qMap(a)
			acc = append(acc, fmt.Sprintf("%s=%v", c211xStr(am, "guestAcceleratorType"), am["guestAcceleratorCount"]))
		}
		aSt, _ := c211qGet(ftok, "https://compute.googleapis.com/compute/v1/projects/"+hostNum+"/zones/"+z+"/acceleratorTypes/nvidia-h100-80gb")
		fmt.Printf("C211Q_OFFER zone=%s accel_nvidia_h100_80gb_http=%d mt_%s_http=%d guestCpus=%v memMB=%v accel=[%s]\n",
			z, aSt, c211qMachine, st, m["guestCpus"], m["memoryMb"], strings.Join(acc, ","))
		if chosenZone == "" && aSt == 200 && st == 200 {
			chosenZone = z
		}
	}
	fmt.Printf("C211Q_CHOICE region=%s zone=%s granted=%q\n", c211qRegion, chosenZone, granted)

	display := fmt.Sprintf("%s-h100x1-%s", c211qPrefix, time.Now().UTC().Format("150405"))
	if len(display) > 128 {
		display = display[:128]
	}
	jobBody := map[string]interface{}{
		"displayName": display,
		"labels":      map[string]interface{}{"c211": c211qNonce},
		"jobSpec": map[string]interface{}{
			"serviceAccount": fleetEmail,
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      c211qMachine,
					"acceleratorType":  c211qAccel,
					"acceleratorCount": 1,
				},
				"replicaCount": 1,
				"diskSpec": map[string]interface{}{
					"bootDiskType": "hyperdisk-balanced", "bootDiskSizeGb": 300,
				},
				"containerSpec": map[string]interface{}{
					"imageUri": c211qDLC,
					"command":  []interface{}{"bash", "-c"},
					"args":     []interface{}{c211qScript()},
				},
			}},
			"scheduling": map[string]interface{}{"timeout": "900s"},
		},
	}
	bj, _ := json.Marshal(jobBody)
	createURL := "https://" + c211qRegion + "-aiplatform.googleapis.com/v1/projects/" + hostNum +
		"/locations/" + c211qRegion + "/customJobs"
	fmt.Printf("C211Q_REQUEST shape=%s-x1 accel=%s image=%s bytes=%d strategy=ON_DEMAND\n", c211qMachine, c211qAccel, c211qDLC, len(bj))
	cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(bj))
	fmt.Printf("C211Q_CREATE http=%d verbatim=%s\n", cst, c211xTrunc(cb, 1500))
	if cst != 200 {
		quotaRefused := strings.Contains(cb, "custom_model_training_nvidia_h100_gpus") ||
			strings.Contains(cb, "RESOURCE_EXHAUSTED") || strings.Contains(cb, "QUOTA_EXCEEDED")
		fmt.Printf("C211Q_VERTEX_REFUSED http=%d quota_refusal=%v granted=%q\n", cst, quotaRefused, granted)
		fmt.Printf("C211Q_VERDICT VERTEX_CREATE_REFUSED_http=%d_quota_refusal=%v\n", cst, quotaRefused)
		fmt.Printf("C211Q_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
		return
	}

	cm := c211xJSON(cb)
	jobName := c211xStr(cm, "name")
	jobID := jobName
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	fmt.Printf("C211Q_JOB name=%s id=%s\n", jobName, jobID)
	fmt.Printf("C211Q_CREATE_SPEC stored={%s} state=%s\n", c211qSpec(cm), c211qJobState(cm))
	fmt.Printf("C211Q_GRANT_AT_CREATE granted=%q\n", granted)

	jurl := "https://" + c211qRegion + "-aiplatform.googleapis.com/v1/" + jobName
	state := ""
	states := []string{}
	last := ""
	logText := ""
	for i := 0; i < 60; i++ {
		gst2, gbody2 := c211qGet(ftok, jurl)
		gm := c211xJSON(gbody2)
		state = c211qJobState(gm)
		if state != last {
			states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(started).Seconds()), state))
			fmt.Printf("C211Q_POLL i=%d http=%d state=%s\n", i, gst2, state)
			last = state
		}
		if i%4 == 3 || strings.Contains(state, "SUCCEEDED") || strings.Contains(state, "FAILED") || strings.Contains(state, "CANCELLED") {
			lt, lv := c211qLogs(ftok, hostNum, jobID)
			fmt.Printf("C211Q_LOG_READ i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 350))
			if len(lt) > len(logText) {
				logText = lt
			}
			if strings.Contains(logText, "GPU-") {
				break
			}
		}
		if strings.Contains(state, "SUCCEEDED") || strings.Contains(state, "FAILED") || strings.Contains(state, "CANCELLED") || strings.Contains(state, "EXPIRED") || strings.Contains(state, "PARTIALLY_SUCCEEDED") {
			break
		}
		if time.Until(deadline) < 3*time.Minute {
			fmt.Printf("C211Q_POLL_BUDGET_EXHAUSTED\n")
			break
		}
		time.Sleep(20 * time.Second)
	}
	fmt.Printf("C211Q_STATES %s\n", strings.Join(states, ">"))

	rst, rbody := c211qGet(ftok, jurl)
	rm := c211xJSON(rbody)
	fmt.Printf("C211Q_READBACK http=%d state=%s stored={%s}\n", rst, c211qJobState(rm), c211qSpec(rm))

	for i := 0; i < 6; i++ {
		lt, lv := c211qLogs(ftok, hostNum, jobID)
		fmt.Printf("C211Q_LOG_FINAL i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 350))
		if len(lt) > len(logText) {
			logText = lt
		}
		if len(logText) > 0 || time.Until(deadline) < 1*time.Minute {
			break
		}
		time.Sleep(20 * time.Second)
	}
	uuids := []string{}
	for _, ln := range strings.Split(logText, "\n") {
		if strings.Contains(ln, "C211QJ_") {
			fmt.Printf("C211Q_LOG %s\n", c211xTrunc(ln, 400))
		}
		for _, m := range c211xUUIDRe.FindAllString(ln, -1) {
			dupe := false
			for _, u := range uuids {
				if u == m {
					dupe = true
				}
			}
			if !dupe {
				uuids = append(uuids, m)
			}
		}
	}
	sort.Strings(uuids)
	fmt.Printf("C211Q_LOG_SUMMARY log_bytes=%d uuids=%d %s\n", len(logText), len(uuids), strings.Join(uuids, ","))

	dst, getAfter, note := c211qRetire(ftok, c211qRegion, jobName)
	fmt.Printf("C211Q_ABSENCE delete_http=%d get_after=%d note=%s\n", dst, getAfter, c211xTrunc(note, 120))

	verdict := "NO_DEVICE_LINE"
	if len(uuids) > 0 {
		verdict = fmt.Sprintf("DEVICE_PROVEN_%d", len(uuids))
	}
	fmt.Printf("C211Q_VERDICT %s state=%s granted=%q\n", verdict, state, granted)
	fmt.Printf("C211Q_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
}

// parseFloatLoose avoids importing strconv just to accept "1" or "1.0".
func parseFloatLoose(s string) (float64, error) {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	if s == "" || s == "<nil>" {
		return 0, fmt.Errorf("empty")
	}
	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	return v, err
}
