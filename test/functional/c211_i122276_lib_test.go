//go:build functional && gke

// C211 i122025 — shared library for the A100/L4 sibling-project probes (helper
// functions and the proven startup/image/quota/marker implementations).
// Original header: A100 DEVICE CAPTURE ON THE FOUR PROVEN DOMINO GCP WIF SIBLING
// PROJECTS (materially larger accelerator class than the landed x121836 T4
// READYs).
//
// Basis: facts 121968/121976 (four dedupe-checked dual-tree T4 READY rows landed
// for team-eng-infra-testing 210628133505, domino-eng-service-artifacts
// 1072394045651, team-eng-infrastructure 463640210929 and team-eng-platform
// 1074386228880 with in-run compute.instances.get readback, teardown and
// absence proof), fact 121979 (NVIDIA_A100_GPUS limit=16 usage=0 and
// NVIDIA_L4_GPUS limit=16 usage=0 in every queried region; only one T4 per
// project consumed) and fact 121388 (the fleetcommand SA minted through this
// same dominodatalab/hephaestus '/functional-test' GKE chain holds
// compute.instances.create, compute.instances.get and compute.instances.delete
// there).
//
// Per intent 122025: read the Compute regions/quotas (NVIDIA_A100_GPUS,
// NVIDIA_L4_GPUS, CPU-family entries) and the a2-highgpu-Ng offerings for each
// sibling project (host project 153711203585 quota read only, never marked),
// then create EXACTLY ONE nonce-tagged ON-DEMAND a2-highgpu-Ng instance (largest
// N whose GPU count fits the measured A100 quota and whose vCPUs fit the
// measured CPU quota, default a2-highgpu-1g with 1 x nvidia-tesla-a100) booted
// from a Google driver-preinstalled accelerator image, capture
// nvidia-smi -L and --query-gpu=name,uuid,memory.total through the serial
// console and guest attributes, take an independent compute.instances.get
// readback, tear it down and prove absence (GET 404 + aggregated list with zero
// c211-named items). At most ONE marker alive at a time, one instance per
// project. ONLY if a2-highgpu/A100 is provider-verbatim refused for a project
// is the identical single-marker flow repeated once with g2-standard-4 +
// 1 x nvidia-tesla-l4 in a region whose measured NVIDIA_L4_GPUS quota is
// nonzero. Every refusal is printed verbatim. Tokens are reported as length +
// SHA-256 prefix only.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	c211aNonce   = "i122276"
	c211aHostNum = "153711203585"
	// Google-supplied driver-preinstalled accelerator images.
	c211aAccelProject = "ubuntu-os-accelerator-images"
	// Fallback only when no accelerator image resolves.
	c211aFallbackProject = "ubuntu-os-cloud"
	c211aFallbackFamily  = "ubuntu-2204-lts"
	c211aLabel           = "c211"
)

var c211aSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211aRegions = []string{"us-central1", "us-east1", "us-west1", "europe-west4", "asia-east1"}

var c211aFamilyProbe = []string{
	"ubuntu-2204-amd64-with-nvidia-580",
	"ubuntu-2204-amd64-with-nvidia-570",
	"ubuntu-2204-amd64-with-nvidia-560",
	"ubuntu-2204-amd64-with-nvidia-550",
	"ubuntu-2204-amd64-with-nvidia-535",
	"ubuntu-2404-amd64-with-nvidia-580",
	"ubuntu-2404-amd64-with-nvidia-570",
	"ubuntu-2404-amd64-with-nvidia-550",
	"ubuntu-2204-amd64-with-nvidia",
	"ubuntu-2404-amd64-with-nvidia",
}

// a2-highgpu-Ng candidates, largest first, with GPU count and vCPU count.
type c211aA2Cand struct {
	machine string
	gpus    int
	vcpus   int
}

var c211aA2Cands = []c211aA2Cand{
	{"a2-highgpu-8g", 8, 96},
	{"a2-highgpu-4g", 4, 48},
	{"a2-highgpu-2g", 2, 24},
	{"a2-highgpu-1g", 1, 12},
}

type c211aImage struct {
	Project string
	Family  string
	Kind    string // "accel" or "install"
}

// ---------------------------------------------------------------------------
// Startup script: driver preinstalled, so this only waits for it and prints the
// device identity to the serial console and to an instance/guest attribute.
// ---------------------------------------------------------------------------
func c211aStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211A_START
put() {
  MSG="$1"
  echo "C211A_RESULT $MSG"
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211a-result" >/dev/null 2>&1
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211a/result" >/dev/null 2>&1
  fi
}
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211A_PCI $PCIDEV"
put "STAGE=boot pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
SMI=""
for i in $(seq 1 24); do
  for p in /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi /home/kubernetes/bin/nvidia/bin/nvidia-smi; do
    if [ -x "$p" ]; then SMI="$p"; break 2; fi
  done
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ]; then SMI="$C"; break; fi
  if [ $((i % 3)) -eq 0 ]; then
    echo "C211A_WAIT i=$i lsmod=$(lsmod | grep -c '^nvidia') dev=$(ls /dev/nvidia* 2>/dev/null | tr '\n' ',') smi_paths=$(ls /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi 2>/dev/null | tr '\n' ',')"
    echo "C211A_DMESG $(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -3 | tr '\n' '|')"
    put "STAGE=wait i=$i lsmod=$(lsmod | grep -c '^nvidia') smi=$SMI"
  fi
  sleep 10
done
echo "C211A_SMI path=$SMI"
if [ -z "$SMI" ]; then
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  sleep 10
  SMI=$(command -v nvidia-smi 2>/dev/null)
  echo "C211A_SMI_AFTER_MODPROBE path=$SMI lsmod=$(lsmod | grep -i nvidia | tr '\n' '|')"
fi
if [ -n "$SMI" ]; then
  for i in $(seq 1 18); do
    if "$SMI" -L > /tmp/c211a-smi.txt 2>&1; then
      echo C211A_GPU_READY
      cat /tmp/c211a-smi.txt
      put "STAGE=gpu_ready smi=$(tr '\n' '|' < /tmp/c211a-smi.txt)"
      "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211a-uuid.txt 2>&1
      cat /tmp/c211a-uuid.txt
      put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211a-uuid.txt)"
      echo C211A_DONE
      exit 0
    fi
    echo "C211A_SMI_ATTEMPT i=$i $(head -c 200 /tmp/c211a-smi.txt | tr '\n' ' ')"
    sleep 5
  done
  put "STAGE=failed smi=$(head -c 600 /tmp/c211a-smi.txt | tr '\n' '|')"
else
  put "STAGE=nosmi kernel=$(uname -r) lsmod=$(lsmod | grep -i nvidia | head -5 | tr '\n' '|') dmesg=$(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -5 | tr '\n' '|')"
fi
echo C211A_FAILED
echo C211A_DONE
`
}

// ---------------------------------------------------------------------------
// Image resolution (same proven mechanism as x121836: driver-preinstalled
// ubuntu-os-accelerator-images first, ubuntu-2204-lts install-path fallback).
// ---------------------------------------------------------------------------

var c211aDriverRe = regexp.MustCompile(`nvidia-([0-9]{3})`)

func c211aResolveAccelImages(tok string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + c211aAccelProject + "/global/images?maxResults=500&fields=items(name,family)"
	st, body := c211xHTTP("GET", u, tok, "", "")
	fam := map[string]bool{}
	if st == 200 {
		if m := c211xJSON(body); m != nil {
			if items, ok := m["items"].([]interface{}); ok {
				for _, it := range items {
					im, _ := it.(map[string]interface{})
					if f := c211xStr(im, "family"); f != "" {
						fam[f] = true
					}
				}
			}
		}
	}
	out := []string{}
	for f := range fam {
		lf := strings.ToLower(f)
		if !strings.Contains(lf, "nvidia") || !strings.Contains(lf, "amd64") {
			continue
		}
		if !strings.Contains(lf, "2204") && !strings.Contains(lf, "2404") {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		gi, gj := 0, 0
		if m := c211aDriverRe.FindStringSubmatch(out[i]); m != nil {
			gi, _ = strconv.Atoi(m[1])
		}
		if m := c211aDriverRe.FindStringSubmatch(out[j]); m != nil {
			gj, _ = strconv.Atoi(m[1])
		}
		if gi != gj {
			return gi > gj
		}
		return out[i] < out[j]
	})
	return out, st, body
}

func c211aResolveImage(tok string) (c211aImage, string) {
	log := []string{}
	fams, st, body := c211aResolveAccelImages(tok)
	fmt.Printf("C211A_IMAGE_LIST project=%s status=%d families=%d chosen_candidates=%s verbatim=%s\n",
		c211aAccelProject, st, len(fams), strings.Join(fams, ","), c211xTrunc(body, 240))
	log = append(log, fmt.Sprintf("list status=%d families=%d", st, len(fams)))
	cands := append([]string{}, fams...)
	for _, f := range c211aFamilyProbe {
		dup := false
		for _, c := range cands {
			if c == f {
				dup = true
			}
		}
		if !dup {
			cands = append(cands, f)
		}
	}
	for _, f := range cands {
		ok, probe := c211xImageUsable(tok, c211aAccelProject, f)
		fmt.Printf("C211A_IMAGE %s\n", probe)
		if ok {
			fmt.Printf("C211A_IMAGE_CHOSEN image=%s/%s kind=accel\n", c211aAccelProject, f)
			return c211aImage{Project: c211aAccelProject, Family: f, Kind: "accel"}, strings.Join(log, "; ")
		}
	}
	ok, probe := c211xImageUsable(tok, c211aFallbackProject, c211aFallbackFamily)
	fmt.Printf("C211A_IMAGE %s\n", probe)
	if ok {
		fmt.Printf("C211A_IMAGE_CHOSEN image=%s/%s kind=install\n", c211aFallbackProject, c211aFallbackFamily)
		return c211aImage{Project: c211aFallbackProject, Family: c211aFallbackFamily, Kind: "install"}, strings.Join(log, "; ")
	}
	fmt.Printf("C211A_IMAGE_NONE accelerator_and_fallback_unusable\n")
	return c211aImage{}, strings.Join(log, "; ")
}

// ---------------------------------------------------------------------------
// Quota / offering reads
// ---------------------------------------------------------------------------

// c211aRegionQuotas returns the A100/L4/CPU quota rows of a region plus the
// region's zone names.
func c211aRegionQuotas(tok, proj, region string) (map[string]string, []string, int) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + region
	st, body := c211xHTTP("GET", u, tok, "", "")
	out := map[string]string{}
	zones := []string{}
	if st != 200 {
		return out, zones, st
	}
	m := c211xJSON(body)
	if qs, ok := m["quotas"].([]interface{}); ok {
		for _, q := range qs {
			qm, _ := q.(map[string]interface{})
			metric := c211xStr(qm, "metric")
			if !(strings.Contains(metric, "A100") || strings.Contains(metric, "L4_GPUS") || strings.Contains(metric, "CPU")) {
				continue
			}
			lim, _ := qm["limit"].(float64)
			usg, _ := qm["usage"].(float64)
			out[metric] = fmt.Sprintf("limit=%v usage=%v", lim, usg)
		}
	}
	if zs, ok := m["zones"].([]interface{}); ok {
		for _, z := range zs {
			s, _ := z.(string)
			if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
				zones = append(zones, s[i+1:])
			}
		}
	}
	sort.Strings(zones)
	return out, zones, st
}

func c211aQVal(rows map[string]string, metric string) (float64, float64, bool) {
	s, ok := rows[metric]
	if !ok {
		return 0, 0, false
	}
	var l, u float64
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "limit=") {
			l, _ = strconv.ParseFloat(strings.TrimPrefix(f, "limit="), 64)
		}
		if strings.HasPrefix(f, "usage=") {
			u, _ = strconv.ParseFloat(strings.TrimPrefix(f, "usage="), 64)
		}
	}
	return l, u, true
}

func c211aQMatrixString(rows map[string]string) string {
	keys := []string{}
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+" "+rows[k])
	}
	return strings.Join(parts, " ; ")
}

func c211aMachineOffered(tok, proj, zone, mt string) (bool, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/machineTypes/" + mt
	st, body := c211xHTTP("GET", u, tok, "", "")
	return st == 200, st, c211xTrunc(body, 220)
}

// ---------------------------------------------------------------------------
// Marker: create ONE nonce on-demand instance, capture the device, tear it
// down, prove absence. All evidence printed verbatim with the C211A_ prefix.
// ---------------------------------------------------------------------------

func c211aCreate(tok, proj, zone, name, saEmail string, img c211aImage, mt, accel string, acount int, startup string) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211aLabel, "value": c211aNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + mt,
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + img.Project + "/global/images/family/" + img.Family,
				"diskSizeGb":  "100",
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
			"acceleratorCount": acount,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{"items": items},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{c211aLabel: c211aNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

func c211aOp(tok, proj, zone, op string) (string, string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 60; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211A_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		if stt != last {
			fmt.Printf("C211A_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, m["progress"])
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b, c211aOpError(b)
		}
		time.Sleep(6 * time.Second)
	}
	return last, "", ""
}

func c211aOpError(body string) string {
	m := c211xJSON(body)
	if m == nil {
		return ""
	}
	e, ok := m["error"].(map[string]interface{})
	if !ok {
		return ""
	}
	msg := c211xStr(e, "message")
	if msg == "" {
		msg = c211xJSONString(e)
	}
	code, _ := e["code"].(float64)
	return fmt.Sprintf("code=%v message=%s", code, msg)
}

func c211aPrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211A_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211A_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 420))
	}
}

func c211aSerial(tok, proj, zone, name string) string {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", tok, "", "")
	if st != 200 {
		return ""
	}
	if m := c211xJSON(body); m != nil {
		if s := c211xStr(m, "contents"); s != "" {
			return s
		}
	}
	return body
}

func c211aGuest(tok, proj, zone, name string) (string, int) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/getGuestAttributes?queryPath=c211a/result", tok, "", "")
	val := ""
	if st == 200 {
		if gm := c211xJSON(body); gm != nil {
			if qr, ok := gm["queryValue"].(map[string]interface{}); ok {
				val = c211xStr(qr, "value")
			}
		}
	}
	return val, st
}

func c211aDeleteAndProveAbsence(tok, proj, zone, name string) (bool, []string) {
	trace := []string{}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211A_TEARDOWN project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, name, dst, c211xTrunc(db, 320))
	trace = append(trace, fmt.Sprintf("teardown http=%d", dst))
	absent := false
	for i := 0; i < 18; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211xInstanceGet(tok, proj, zone, name)
		if ist == 404 {
			absent = true
			fmt.Printf("C211A_ABSENCE project=%s name=%s poll=%d http=404\n", proj, name, i)
			break
		}
		if i == 17 {
			fmt.Printf("C211A_ABSENCE project=%s name=%s poll=%d http=%d verbatim=%s\n", proj, name, i, ist, c211xTrunc(ib, 300))
			trace = append(trace, fmt.Sprintf("last_get_http=%d", ist))
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211aNonce)+"&maxResults=500", tok, "", "")
	zero := false
	names := []string{}
	if lst == 200 {
		if m := c211xJSON(lb); m != nil {
			if items, ok := m["items"].(map[string]interface{}); ok {
				for zk, zv := range items {
					zm, _ := zv.(map[string]interface{})
					list, _ := zm["instances"].([]interface{})
					for _, it := range list {
						im, _ := it.(map[string]interface{})
						if n := c211xStr(im, "name"); n != "" {
							names = append(names, strings.TrimPrefix(zk, "zones/")+"/"+n)
						}
					}
				}
			}
			zero = len(names) == 0
		}
	}
	fmt.Printf("C211A_AGG project=%s status=%d c211_named=%d names=%s verbatim=%s\n", proj, lst, len(names), strings.Join(names, ","), c211xTrunc(lb, 360))
	trace = append(trace, fmt.Sprintf("agg http=%d c211_named=%d", lst, len(names)))
	return absent && zero, trace
}

// c211aMarker creates ONE on-demand instance, captures any NVIDIA device line,
// takes the independent compute.instances.get readback, tears the instance down
// and proves absence. Returns (uuid, metaAndAccel, created, opErr, statusFinal).
func c211aMarker(tok, proj, zone, name, saEmail string, img c211aImage, mt, accel string, acount int, startup string, deadline time.Time) (string, string, bool, string, string) {
	cst, cbody := c211aCreate(tok, proj, zone, name, saEmail, img, mt, accel, acount, startup)
	fmt.Printf("C211A_MARKER_CREATE project=%s zone=%s name=%s machine=%s accel=%s count=%d image=%s/%s kind=%s status=%d verbatim=%s\n",
		proj, zone, name, mt, accel, acount, img.Project, img.Family, img.Kind, cst, c211xTrunc(cbody, 1400))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211aOp(tok, proj, zone, opName)
		fmt.Printf("C211A_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 400), c211xTrunc(ob, 3000))
		created = opStatus == "DONE" && opErr == ""
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}

	uuid, metaFinal := "", ""
	if created {
		seen := map[string]bool{}
		for i := 0; i < 26; i++ {
			if time.Now().After(deadline) {
				fmt.Printf("C211A_CAPTURE project=%s STOP_TIME_BUDGET poll=%d\n", proj, i)
				break
			}
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211a-result"); v != "" {
					metaFinal = v
				}
			}
			content := c211aSerial(tok, proj, zone, name)
			if content != "" {
				c211aPrintNew(seen, proj, content)
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
			}
			gval, gst := c211aGuest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
			}
			fmt.Printf("C211A_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuid=%s\n",
				proj, i, state, len(content), gst, c211xTrunc(gval, 200), c211xTrunc(metaFinal, 220), uuid)
			if u := c211xUUIDRe.FindString(metaFinal); u != "" {
				uuid = u
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(metaFinal, "STAGE=nosmi") {
				break
			}
			if ist == 404 && i >= 2 {
				break
			}
			time.Sleep(9 * time.Second)
		}
	}

	// Independent provider readback of the same instance (accelerator attached).
	accelJSON, statusFinal := "", ""
	if rst, rm, rb := c211xInstanceGet(tok, proj, zone, name); rst >= 0 {
		if rm != nil {
			statusFinal = c211xStr(rm, "status")
			if ga, ok := rm["guestAccelerators"].([]interface{}); ok {
				accelJSON = c211xJSONString(ga)
			}
			if v := c211xMetaValue(rm, "c211a-result"); v != "" {
				metaFinal = v
			}
		}
		fmt.Printf("C211A_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 320), c211xTrunc(metaFinal, 420), c211xTrunc(rb, 200))
	}
	if u := c211xUUIDRe.FindString(metaFinal); u != "" {
		uuid = u
	}
	return uuid, metaFinal + " | accel=" + accelJSON, created, opErr, statusFinal
}

// c211aReaper tears down any leftover own-label instance in the project.
func c211aReaper(tok, proj string) {
	for round := 0; round < 3; round++ {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211aNonce)+"&maxResults=500", tok, "", "")
		fmt.Printf("C211A_REAPER_LIST project=%s round=%d status=%d verbatim=%s\n", proj, round, st, c211xTrunc(body, 480))
		if st != 200 {
			return
		}
		m := c211xJSON(body)
		items, _ := m["items"].(map[string]interface{})
		gone := 0
		for zoneKey, zv := range items {
			zm, _ := zv.(map[string]interface{})
			list, _ := zm["instances"].([]interface{})
			zone := strings.TrimPrefix(zoneKey, "zones/")
			for _, it := range list {
				im, _ := it.(map[string]interface{})
				n := c211xStr(im, "name")
				if n == "" {
					continue
				}
				ds, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+n, tok, "", "")
				fmt.Printf("C211A_REAPER_TEARDOWN project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 280))
				gone++
			}
		}
		if gone == 0 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// c211aPickZone finds the first zone of the region where the machine type is
// offered AND the accelerator type is advertised.
func c211aPickZone(tok, proj, region string, zones []string, mt, accel string) (string, []string) {
	notes := []string{}
	for _, z := range zones {
		ok, mst, mbody := c211aMachineOffered(tok, proj, z, mt)
		ats, ast, _ := c211xAccelTypes(tok, proj, z)
		hasAccel := ast == 200 && c211xContains(ats, accel)
		notes = append(notes, fmt.Sprintf("%s machine=%s http=%d accel_http=%d has_%s=%v", z, mt, mst, ast, accel, hasAccel))
		fmt.Printf("C211A_OFFER project=%s region=%s zone=%s machine=%s machine_http=%d accel_http=%d has_accel=%v accel_n=%d verbatim=%s\n",
			proj, region, z, mt, mst, ast, hasAccel, len(ats), c211xTrunc(mbody, 160))
		if ok && hasAccel {
			return z, notes
		}
	}
	return "", notes
}

// c211aA100Attempt runs the A100 single-marker flow for one project.
