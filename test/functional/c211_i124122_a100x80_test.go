//go:build functional && gke

// C211 i124122 — Vertex AI A100-80GB RESTRICTED-IMAGE on-demand custom-training cell.
//
// Fact basis (zero-network preflight): the hephaestus host project 153711203585
// measures aiplatform restricted_image_training_nvidia_a100_80gb_gpus = 8.0 in
// us-central1 and 2.0 in us-east4 while custom_model_training_nvidia_a100_80gb_gpus
// is 0 in every region.  The restricted-image quota family has never been
// exercised by any lane, so this probe:
//
//   (a) reads the verbatim cloudquotas.googleapis.com quotaInfos metadata for every
//       restricted_image_training_nvidia_* metric (quotaId / metric / display names /
//       dimensions) in 153711203585,
//   (b) reads the a2-ultragpu-1g (1 x nvidia-a100-80gb) and a2-ultragpu-8g offerings
//       plus acceleratorTypes nvidia-a100-80gb in us-central1-a/-c and us-east4-c,
//   (c) creates EXACTLY ONE nonce-tagged ON-DEMAND customJob (machineType
//       a2-ultragpu-1g, acceleratorType NVIDIA_A100_80GB, acceleratorCount 1) with a
//       Vertex-managed us-docker.pkg.dev/vertex-ai/training/* DLC image — the image
//       class the restricted-image quota governs — printing nvidia-smi -L and
//       nvidia-smi --query-gpu, polls it to terminal, reads Cloud Logging stdout
//       verbatim, takes an independent customJobs.get readback, and retires it
//       (cancel + delete + GET 404).  The job's scheduling.timeout self-terminates
//       it as a fallback if a deletion endpoint is blocked.
//
// Exactly ONE alternate-image attempt is permitted, and only after a verbatim
// create refusal of the DLC image.
//
// Secrets are never printed; only length + SHA-256 prefixes.
package functional

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const c211x8Nonce = "i124122"
const c211x8Prefix = "c211-i124122"
const c211x8HostNum = "153711203585"
const c211x8DLC = "us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-4.py310:latest"
const c211x8AltImg = "nvidia/cuda:12.4.0-base-ubuntu22.04"

// us-central1 first (restricted-image limit 8), then us-east4 (limit 2).
var c211x8RegionPref = []string{"us-central1", "us-east4"}

// zones where the saved offering matrix found a2-ultragpu-1g
var c211x8ZonesByRegion = map[string][]string{
	"us-central1": {"us-central1-a", "us-central1-c"},
	"us-east4":    {"us-east4-c"},
}

// regions whose restricted_image_training_nvidia_a100_80gb_gpus limit the quota
// read must report verbatim
var c211x8QuotaRegions = []string{"us-central1", "us-east4", "europe-west4"}

var c211x8Perms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete", "logging.logEntries.list",
	"serviceusage.services.get",
}

// ---------------------------------------------------------------------------
// local helpers (c211x8* prefix — no collisions with the shared c211x* helpers)
// ---------------------------------------------------------------------------

func c211l4List(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211l4Map(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211l4Float(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		var f float64
		fmt.Sscanf(x, "%f", &f)
		return f
	}
	return 0
}

func c211x8Script() string {
	return `set -u
PROOF=/tmp/c211x8-proof.txt
{
echo "C211X8_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124122 shape=a2-ultragpu-1g-x1"
echo "C211X8_UNAME $(uname -a)"
echo "C211X8_OSREL $(cat /etc/os-release 2>/dev/null | tr '\n' ';')"
echo "C211X8_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211X8_LSPCI $(lspci -nn 2>/dev/null | grep -i nvidia | tr '\n' '|')"
echo "C211X8_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211X8_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  if [ -x /usr/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211X8_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
if [ -z "$SMI" ] && [ -x /usr/bin/nvidia-smi ]; then SMI=/usr/bin/nvidia-smi; fi
echo "C211X8_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211X8_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211X8_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211X8_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>&1 | wc -l)"
else
  echo "C211X8_NOSMI"
fi
echo "C211X8_END"
} > $PROOF 2>&1
cat $PROOF
sleep 25
echo "C211X8_DONE at=$(date -u +%FT%TZ)"
`
}

// c211x8Get returns status + body for a GET.
func c211x8Get(tok, u string) (int, string) {
	return c211xHTTP("GET", u, tok, "", "")
}

// c211x8AccelZone probes one acceleratorType in one zone.
func c211x8AccelZone(tok, num, zone, accel string) (int, string) {
	u := "https://www.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/acceleratorTypes/" + accel
	return c211x8Get(tok, u)
}

// c211x8MType probes one machineType in one zone and returns the accelerator
// summary the offering carries.
func c211x8MType(tok, num, zone, mt string) (int, string) {
	u := "https://www.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/machineTypes/" + mt
	st, body := c211x8Get(tok, u)
	m := c211xJSON(body)
	acc := []string{}
	for _, a := range c211l4List(m["accelerators"]) {
		am := c211l4Map(a)
		acc = append(acc, fmt.Sprintf("%s=%v", c211xStr(am, "guestAcceleratorType"), am["guestAcceleratorCount"]))
	}
	return st, fmt.Sprintf("guestCpus=%v memMB=%v accel=[%s]", m["guestCpus"], m["memoryMb"], strings.Join(acc, ","))
}

// c211x8QuotaInfos paginates cloudquotas quotaInfos and prints the verbatim
// metadata of every restricted_image_training_nvidia_* metric.
func c211x8QuotaInfos(tok, num string) (int, []map[string]interface{}, string) {
	out := []map[string]interface{}{}
	page := ""
	http := 0
	last := ""
	for i := 0; i < 20; i++ {
		u := "https://cloudquotas.googleapis.com/v1/projects/" + num +
			"/locations/global/services/aiplatform.googleapis.com/quotaInfos?pageSize=200"
		if page != "" {
			u += "&pageToken=" + page
		}
		st, body := c211xHTTP("GET", u, tok, "", "")
		http = st
		if st != 200 {
			last = c211xTrunc(body, 600)
			break
		}
		m := c211xJSON(body)
		for _, qi := range c211l4List(m["quotaInfos"]) {
			qm := c211l4Map(qi)
			out = append(out, qm)
			metric := c211xStr(qm, "metric")
			if strings.Contains(metric, "restricted_image_training_nvidia_") {
				fmt.Printf("C211X8_QUOTAINFO metric=%s quotaId=%s metricDisplayName=%s quotaDisplayName=%s dimensions=%s isPrecise=%v containerType=%s\n",
					metric, c211xStr(qm, "quotaId"), c211xStr(qm, "metricDisplayName"),
					c211xStr(qm, "quotaDisplayName"), c211xJSONString(qm["dimensions"]),
					qm["isPrecise"], c211xStr(qm, "containerType"))
				for _, di := range c211l4List(qm["dimensionsInfos"]) {
					dm := c211l4Map(di)
					details := c211l4Map(dm["details"])
					vals := c211xJSONString(details["value"])
					if vals == "{}" || vals == "" {
						vals = c211xJSONString(details)
					}
					fmt.Printf("C211X8_QUOTADIM metric=%s dim=%s applicableLocations=%s details=%s\n",
						metric, c211xStr(dm, "dimensions"), c211xJSONString(dm["applicableLocations"]), c211xTrunc(vals, 300))
				}
			}
		}
		page = c211xStr(m, "nextPageToken")
		if page == "" {
			last = "pagination-complete"
			break
		}
	}
	return http, out, last
}

// c211x8QuotaLimits reads the consumerQuotaMetrics FULL view for one metric and
// returns its per-region effective limits.
func c211x8QuotaLimits(tok, num, metric string) (map[string]string, int) {
	res := map[string]string{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211x8Get(tok, u)
	if st != 200 {
		return res, st
	}
	m := c211xJSON(body)
	for _, mi := range c211l4List(m["metrics"]) {
		mm := c211l4Map(mi)
		if c211xStr(mm, "metric") != metric {
			continue
		}
		for _, li := range c211l4List(mm["consumerQuotaLimits"]) {
			lm := c211l4Map(li)
			for _, bi := range c211l4List(lm["quotaBuckets"]) {
				bm := c211l4Map(bi)
				dims := c211l4Map(bm["dimensions"])
				reg := c211xStr(dims, "region")
				if reg == "" {
					reg = "global"
				}
				res[reg] = fmt.Sprintf("%v", bm["effectiveLimit"])
			}
		}
	}
	return res, st
}

// c211x8Retire cancels + deletes a customJob and proves GET 404.
func c211x8Retire(tok, region, name string) (int, int, string) {
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + name
	cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
	fmt.Printf("C211X8_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
	dst := -1
	note := ""
	for i := 0; i < 10; i++ {
		dst, note = c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211X8_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(note, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 204 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211x8Get(tok, jurl)
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

// c211x8JobState reads the terminal state of a customJob payload.
func c211x8JobState(m map[string]interface{}) string {
	if s := c211xStr(m, "state"); s != "" {
		return s
	}
	return "UNKNOWN"
}

// c211x8Spec returns the stored machine spec of an independent readback.
func c211x8Spec(m map[string]interface{}) string {
	js := c211l4Map(m["jobSpec"])
	for _, w := range c211l4List(js["workerPoolSpecs"]) {
		wm := c211l4Map(w)
		ms := c211l4Map(wm["machineSpec"])
		cs := c211l4Map(wm["containerSpec"])
		return fmt.Sprintf("machineType=%s acceleratorType=%s acceleratorCount=%v imageUri=%s",
			c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"),
			ms["acceleratorCount"], c211xStr(cs, "imageUri"))
	}
	return "none"
}

// c211x8Logs reads Cloud Logging stdout for the job across fallback filters.
func c211x8Logs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`"C211X8_" AND resource.labels.job_id="` + jobID + `"`,
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
			em := c211l4Map(e)
			if ts := c211xStr(em, "timestamp"); ts != "" {
				t.WriteString(ts + " ")
			}
			if p, ok := em["textPayload"].(string); ok {
				t.WriteString(p)
			}
			if jp := c211l4Map(em["jsonPayload"]); jp != nil {
				t.WriteString(c211xJSONString(jp))
			}
			if pb := c211l4Map(em["protoPayload"]); pb != nil {
				t.WriteString(c211xJSONString(pb))
			}
			t.WriteString("\n")
		}
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 80), st, len(entries), t.Len()))
		if t.Len() > 0 {
			all.WriteString(t.String() + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

func TestC211A100X80RestrictedImage(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211X8_START nonce=%s at=%s\n", c211x8Nonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211X8_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", c211x8HostNum
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = c211x8HostNum
	}
	fmt.Printf("C211X8_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211X8_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211X8_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}

	// project state + permissions
	pmSt, pmBody := c211x8Get(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+hostNum)
	fmt.Printf("C211X8_PROJECT proj=%s num=%s http=%d state=%s\n", "domino-eng-platform-dev", hostNum, pmSt, c211xStr(c211xJSON(pmBody), "lifecycleState"))
	polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + hostNum
	al, ast, _ := c211xTestIamBatch(ftok, polURL, c211x8Perms)
	fmt.Printf("C211X8_PERM http=%d allowed=%v\n", ast, al)
	svSt, svBody := c211x8Get(ftok, "https://serviceusage.googleapis.com/v1/projects/"+hostNum+"/services/aiplatform.googleapis.com")
	fmt.Printf("C211X8_SVC http=%d state=%s\n", svSt, c211xStr(c211xJSON(svBody), "state"))

	// (a) verbatim cloudquotas quotaInfos metadata for restricted_image_training_nvidia_*
	qhttp, qall, qnote := c211x8QuotaInfos(ftok, hostNum)
	nri := 0
	for _, q := range qall {
		if strings.Contains(c211xStr(q, "metric"), "restricted_image_training_nvidia_") {
			nri++
		}
	}
	fmt.Printf("C211X8_QUOTAINFOS http=%d total=%d restricted_nvidia=%d note=%s\n", qhttp, len(qall), nri, c211xTrunc(qnote, 200))
	if qhttp != 200 {
		// fall back to the modelowner-visible consumerQuotaMetrics view
		fmt.Printf("C211X8_QUOTAINFOS_DENIED falling_back=serviceusage-FULL\n")
	}

	// per-region limits of the two A100-80GB metrics
	riLim, riSt := c211x8QuotaLimits(ftok, hostNum, "aiplatform.googleapis.com/restricted_image_training_nvidia_a100_80gb_gpus")
	cmLim, cmSt := c211x8QuotaLimits(ftok, hostNum, "aiplatform.googleapis.com/custom_model_training_nvidia_a100_80gb_gpus")
	regs := append([]string{}, c211x8QuotaRegions...)
	sort.Strings(regs)
	riParts, cmParts := []string{}, []string{}
	for _, r := range regs {
		rv, rok := riLim[r]
		if !rok {
			rv = "absent"
		}
		cv, cok := cmLim[r]
		if !cok {
			cv = "absent"
		}
		riParts = append(riParts, r+"="+rv)
		cmParts = append(cmParts, r+"="+cv)
	}
	fmt.Printf("C211X8_QUOTA restricted_image_a100_80gb http=%d %s\n", riSt, strings.Join(riParts, ","))
	fmt.Printf("C211X8_QUOTA custom_model_a100_80gb http=%d %s\n", cmSt, strings.Join(cmParts, ","))

	// (b) offering matrix for a2-ultragpu-1g / a2-ultragpu-8g + nvidia-a100-80gb
	chosenRegion, chosenZone := "", ""
	offering := map[string]string{}
	for _, reg := range c211x8RegionPref {
		for _, z := range c211x8ZonesByRegion[reg] {
			a1, _ := c211x8AccelZone(ftok, hostNum, z, "nvidia-a100-80gb")
			m1, d1 := c211x8MType(ftok, hostNum, z, "a2-ultragpu-1g")
			m8, d8 := c211x8MType(ftok, hostNum, z, "a2-ultragpu-8g")
			offering[z] = fmt.Sprintf("nvidia-a100-80gb_http=%d mt1g_http=%d{%s} mt8g_http=%d{%s}", a1, m1, d1, m8, d8)
			fmt.Printf("C211X8_OFFER zone=%s accel_nvidia_a100_80gb_http=%d mt_a2_ultragpu_1g_http=%d %s mt_a2_ultragpu_8g_http=%d %s\n",
				z, a1, m1, c211xTrunc(d1, 220), m8, c211xTrunc(d8, 220))
			if chosenZone == "" && a1 == 200 && m1 == 200 {
				chosenRegion, chosenZone = reg, z
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	fmt.Printf("C211X8_CHOICE region=%s zone=%s ri_limit_us_central1=%s ri_limit_us_east4=%s\n",
		chosenRegion, chosenZone, riLim["us-central1"], riLim["us-east4"])
	if chosenZone == "" {
		fmt.Printf("C211X8_VERDICT NO_OFFERING_FOR_a2-ultragpu-1g\n")
		fmt.Printf("C211X8_DONE at=%s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		return
	}

	// quota must cover 1 card
	limit := riLim[chosenRegion]
	if limit == "" || limit == "<nil>" || limit == "absent" {
		fmt.Printf("C211X8_VERDICT RESTRICTED_IMAGE_QUOTA_ABSENT_IN_%s\n", chosenRegion)
		fmt.Printf("C211X8_DONE at=%s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		return
	}

	// (c) exactly ONE nonce-tagged ON-DEMAND customJob, one alternate image at most
	images := []string{c211x8DLC, c211x8AltImg}
	script := c211x8Script()
	created := false
	jobName, jobID := "", ""
	createHTTP := 0
	createBody := ""
	usedImage := ""
	for ii, img := range images {
		display := fmt.Sprintf("%s-a100x80-%s", c211x8Prefix, time.Now().UTC().Format("150405"))
		if len(display) > 128 {
			display = display[:128]
		}
		jobSpec := map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      "a2-ultragpu-1g",
					"acceleratorType":  "NVIDIA_A100_80GB",
					"acceleratorCount": 1,
				},
				"replicaCount": 1,
				"containerSpec": map[string]interface{}{
					"imageUri": img,
					"command":  []interface{}{"/bin/bash", "-c"},
					"args":     []interface{}{script},
				},
			}},
			"scheduling": map[string]interface{}{"timeout": "900s"},
		}
		jobBody := map[string]interface{}{
			"displayName": display,
			"labels":      map[string]interface{}{"c211": c211x8Nonce},
			"jobSpec":     jobSpec,
		}
		body, _ := json.Marshal(jobBody)
		createURL := "https://" + chosenRegion + "-aiplatform.googleapis.com/v1/projects/" + hostNum +
			"/locations/" + chosenRegion + "/customJobs"
		fmt.Printf("C211X8_REQUEST region=%s shape=a2-ultragpu-1g-x1 image=%s bytes=%d attempt=%d\n",
			chosenRegion, img, len(body), ii+1)
		cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
		createHTTP, createBody = cst, cb
		fmt.Printf("C211X8_CREATE region=%s image=%s http=%d verbatim=%s\n", chosenRegion, img, cst, c211xTrunc(cb, 900))
		if cst == 200 {
			cm := c211xJSON(cb)
			jobName = c211xStr(cm, "name")
			jobID = jobName
			if i := strings.LastIndex(jobID, "/"); i >= 0 {
				jobID = jobID[i+1:]
			}
			usedImage = img
			created = true
			fmt.Printf("C211X8_JOB name=%s id=%s\n", jobName, jobID)
			fmt.Printf("C211X8_CREATE_SPEC stored={%s} state=%s\n", c211x8Spec(cm), c211x8JobState(cm))
			break
		}
		// only a verbatim refusal of the DLC image may trigger ONE alternate attempt
		if ii == 0 {
			fmt.Printf("C211X8_DLC_REFUSED verbatim=%s\n", c211xTrunc(cb, 600))
		}
	}
	if !created {
		fmt.Printf("C211X8_VERDICT CREATE_REFUSED http=%d\n", createHTTP)
		fmt.Printf("C211X8_CREATE_BODY %s\n", c211xTrunc(createBody, 1200))
		fmt.Printf("C211X8_DONE at=%s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		return
	}

	jurl := "https://" + chosenRegion + "-aiplatform.googleapis.com/v1/" + jobName
	// poll to terminal (bounded)
	deadline := started.Add(13 * time.Minute)
	state := ""
	states := []string{}
	for i := 0; i < 60; i++ {
		gst, gbody := c211x8Get(ftok, jurl)
		gm := c211xJSON(gbody)
		state = c211x8JobState(gm)
		states = append(states, state)
		fmt.Printf("C211X8_POLL i=%d http=%d state=%s\n", i, gst, state)
		if state == "JOB_STATE_SUCCEEDED" || state == "JOB_STATE_FAILED" || state == "JOB_STATE_CANCELLED" || state == "JOB_STATE_EXPIRED" {
			break
		}
		if time.Until(deadline) < 2*time.Minute {
			fmt.Printf("C211X8_POLL_BUDGET_EXHAUSTED\n")
			break
		}
		time.Sleep(20 * time.Second)
	}
	fmt.Printf("C211X8_STATES %s\n", strings.Join(states, ">"))

	// independent readback
	rst, rbody := c211x8Get(ftok, jurl)
	rm := c211xJSON(rbody)
	fmt.Printf("C211X8_READBACK http=%d state=%s stored={%s}\n", rst, c211x8JobState(rm), c211x8Spec(rm))

	// Cloud Logging stdout (retry with backoff for ingestion lag)
	logText, logVerdict := "", ""
	for i := 0; i < 6; i++ {
		logText, logVerdict = c211x8Logs(ftok, hostNum, jobID)
		fmt.Printf("C211X8_LOG_READ i=%d bytes=%d %s\n", i, len(logText), c211xTrunc(logVerdict, 400))
		if len(logText) > 0 {
			break
		}
		if time.Until(deadline) < 1*time.Minute {
			break
		}
		time.Sleep(30 * time.Second)
	}
	uuids := []string{}
	for _, ln := range strings.Split(logText, "\n") {
		if strings.Contains(ln, "C211X8_") || strings.Contains(ln, "GPU-") {
			fmt.Printf("C211X8_LOG %s\n", c211xTrunc(ln, 400))
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
	fmt.Printf("C211X8_LOG_SUMMARY log_bytes=%d uuids=%d %s\n", len(logText), len(uuids), strings.Join(uuids, ","))

	// retire + absence proof
	dst, getAfter, note := c211x8Retire(ftok, chosenRegion, jobName)
	fmt.Printf("C211X8_ABSENCE delete_http=%d get_after=%d note=%s\n", dst, getAfter, c211xTrunc(note, 120))

	verdict := "NO_DEVICE_LINE"
	if len(uuids) > 0 {
		verdict = fmt.Sprintf("DEVICE_PROVEN_%d", len(uuids))
	}
	fmt.Printf("C211X8_VERDICT %s image=%s state=%s\n", verdict, usedImage, state)
	fmt.Printf("C211X8_DONE at=%s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}
