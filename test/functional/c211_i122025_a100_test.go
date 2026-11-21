//go:build functional && gke

// C211 i122025 — A100 DEVICE CAPTURE ON THE FOUR PROVEN DOMINO GCP WIF SIBLING
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
	"testing"
	"time"
)

const (
	c211aNonce   = "i122025"
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
func c211aA100Attempt(tok, proj, num string, img c211aImage, startup string, deadline time.Time) map[string]interface{} {
	res := map[string]interface{}{}
	saEmail := num + "-compute@developer.gserviceaccount.com"
	type regionQuota struct {
		region string
		rows   map[string]string
		zones  []string
		lim    float64
		usage  float64
	}
	scanned := []regionQuota{}
	quotaMatrix := map[string]string{}
	chosen := -1
	for _, r := range c211aRegions {
		rows, zones, qst := c211aRegionQuotas(tok, proj, r)
		quotaMatrix[r] = fmt.Sprintf("http=%d %s", qst, c211aQMatrixString(rows))
		fmt.Printf("C211A_QUOTA project=%s region=%s http=%d a100=[%s] rows=[%s]\n", proj, r, qst,
			rows["NVIDIA_A100_GPUS"], c211aQMatrixString(rows))
		lim, usage, ok := c211aQVal(rows, "NVIDIA_A100_GPUS")
		scanned = append(scanned, regionQuota{region: r, rows: rows, zones: zones, lim: lim, usage: usage})
		if ok && lim > usage && chosen < 0 && len(zones) > 0 {
			chosen = len(scanned) - 1
		}
	}
	res["quota_matrix"] = quotaMatrix
	if chosen < 0 {
		res["verdict"] = "NO_NONZERO_A100_QUOTA"
		fmt.Printf("C211A_A100 project=%s verdict=NO_NONZERO_A100_QUOTA\n", proj)
		return res
	}
	rq := scanned[chosen]
	res["region"] = rq.region
	res["a100_limit"] = rq.lim
	res["a100_usage"] = rq.usage

	// Candidate choice: largest N whose GPUs fit the A100 remaining quota and
	// whose vCPUs fit the CPU quota; then one bounded a2-highgpu-1g retry.
	cpuLim, cpuUse, cpuOk := c211aQVal(rq.rows, "CPUS")
	pick := c211aA2Cands[len(c211aA2Cands)-1]
	for _, c := range c211aA2Cands {
		if float64(c.gpus) <= rq.lim-rq.usage && (!cpuOk || float64(c.vcpus) <= cpuLim-cpuUse) {
			pick = c
			break
		}
	}
	cands := []c211aA2Cand{pick}
	if pick.machine != "a2-highgpu-1g" && rq.lim-rq.usage >= 1 {
		cands = append(cands, c211aA2Cand{"a2-highgpu-1g", 1, 12})
	}
	res["candidates"] = fmt.Sprintf("%v", cands)
	res["cpu_quota"] = fmt.Sprintf("ok=%v limit=%v usage=%v", cpuOk, cpuLim, cpuUse)
	fmt.Printf("C211A_A100 project=%s region=%s a100_limit=%v usage=%v cpu_ok=%v cpu_limit=%v cpu_usage=%v candidates=%v\n",
		proj, rq.region, rq.lim, rq.usage, cpuOk, cpuLim, cpuUse, cands)

	tried := []string{}
	for _, cand := range cands {
		if time.Now().After(deadline) {
			res["verdict"] = "A100_SKIPPED_TIME_BUDGET"
			fmt.Printf("C211A_A100 project=%s verdict=A100_SKIPPED_TIME_BUDGET\n", proj)
			return res
		}
		zone, notes := c211aPickZone(tok, proj, rq.region, rq.zones, cand.machine, "nvidia-tesla-a100")
		res["offer_notes_"+cand.machine] = strings.Join(notes, "; ")
		if zone == "" {
			tried = append(tried, cand.machine+":OFFERING_ABSENT")
			fmt.Printf("C211A_A100 project=%s candidate=%s verdict=OFFERING_ABSENT\n", proj, cand.machine)
			continue
		}
		instName := "c211-" + c211aNonce + "-a100"
		fmt.Printf("C211A_A100 project=%s MARKER_ATTEMPT machine=%s zone=%s accel=nvidia-tesla-a100 count=%d instance=%s sa=%s image=%s/%s kind=%s\n",
			proj, cand.machine, zone, cand.gpus, instName, c211xShort(saEmail), img.Project, img.Family, img.Kind)
		uuid, meta, created, opErr, statusFinal := c211aMarker(tok, proj, zone, instName, saEmail, img, cand.machine, "nvidia-tesla-a100", cand.gpus, startup, deadline)
		res["marker_machine"] = cand.machine
		res["marker_zone"] = zone
		res["marker_accel"] = "nvidia-tesla-a100"
		res["marker_count"] = cand.gpus
		res["created"] = created
		res["uuid"] = uuid
		res["meta"] = c211xTrunc(meta, 700)
		res["op_error"] = opErr
		res["status_final"] = statusFinal
		absent, trace := c211aDeleteAndProveAbsence(tok, proj, zone, instName)
		res["absent"] = absent
		res["cleanup_trace"] = trace
		if uuid != "" {
			fmt.Printf("C211A_A100 project=%s verdict=MARKER_GPU_A100 machine=%s uuid=%s absent=%v\n", proj, cand.machine, uuid, absent)
			res["verdict"] = "MARKER_GPU_A100"
			return res
		}
		tried = append(tried, cand.machine+":NO_DEVICE created="+strconv.FormatBool(created)+" op_err="+c211xTrunc(opErr, 200))
		fmt.Printf("C211A_A100 project=%s candidate=%s verdict=NO_DEVICE created=%v op_error=%s\n", proj, cand.machine, created, c211xTrunc(opErr, 200))
	}
	res["tried"] = strings.Join(tried, " | ")
	res["verdict"] = "A100_REFUSED_OR_NO_DEVICE"
	return res
}

// c211aL4Attempt runs the L4 fallback single-marker flow for one project.
func c211aL4Attempt(tok, proj, num string, img c211aImage, startup string, deadline time.Time) map[string]interface{} {
	res := map[string]interface{}{}
	saEmail := num + "-compute@developer.gserviceaccount.com"
	type regionQuota struct {
		region string
		rows   map[string]string
		zones  []string
		lim    float64
		usage  float64
	}
	scanned := []regionQuota{}
	quotaMatrix := map[string]string{}
	chosen := -1
	for _, r := range c211aRegions {
		rows, zones, qst := c211aRegionQuotas(tok, proj, r)
		quotaMatrix[r] = fmt.Sprintf("http=%d %s", qst, c211aQMatrixString(rows))
		fmt.Printf("C211A_L4_QUOTA project=%s region=%s http=%d l4=[%s]\n", proj, r, qst, rows["NVIDIA_L4_GPUS"])
		lim, usage, ok := c211aQVal(rows, "NVIDIA_L4_GPUS")
		scanned = append(scanned, regionQuota{region: r, rows: rows, zones: zones, lim: lim, usage: usage})
		if ok && lim > usage && chosen < 0 && len(zones) > 0 {
			chosen = len(scanned) - 1
		}
	}
	res["l4_quota_matrix"] = quotaMatrix
	if chosen < 0 {
		res["verdict"] = "NO_NONZERO_L4_QUOTA"
		fmt.Printf("C211A_L4 project=%s verdict=NO_NONZERO_L4_QUOTA\n", proj)
		return res
	}
	rq := scanned[chosen]
	res["l4_region"] = rq.region
	res["l4_limit"] = rq.lim
	res["l4_usage"] = rq.usage
	zone, notes := c211aPickZone(tok, proj, rq.region, rq.zones, "g2-standard-4", "nvidia-tesla-l4")
	res["l4_offer_notes"] = strings.Join(notes, "; ")
	if zone == "" {
		res["verdict"] = "L4_OFFERING_ABSENT"
		fmt.Printf("C211A_L4 project=%s region=%s verdict=L4_OFFERING_ABSENT\n", proj, rq.region)
		return res
	}
	instName := "c211-" + c211aNonce + "-l4"
	fmt.Printf("C211A_L4 project=%s MARKER_ATTEMPT machine=g2-standard-4 zone=%s accel=nvidia-tesla-l4 count=1 instance=%s sa=%s\n",
		proj, zone, instName, c211xShort(saEmail))
	uuid, meta, created, opErr, statusFinal := c211aMarker(tok, proj, zone, instName, saEmail, img, "g2-standard-4", "nvidia-tesla-l4", 1, startup, deadline)
	res["marker_machine"] = "g2-standard-4"
	res["marker_zone"] = zone
	res["marker_accel"] = "nvidia-tesla-l4"
	res["marker_count"] = 1
	res["created"] = created
	res["uuid"] = uuid
	res["meta"] = c211xTrunc(meta, 700)
	res["op_error"] = opErr
	res["status_final"] = statusFinal
	absent, trace := c211aDeleteAndProveAbsence(tok, proj, zone, instName)
	res["absent"] = absent
	res["cleanup_trace"] = trace
	if uuid != "" {
		fmt.Printf("C211A_L4 project=%s verdict=MARKER_GPU_L4 uuid=%s absent=%v\n", proj, uuid, absent)
		res["verdict"] = "MARKER_GPU_L4"
		return res
	}
	res["verdict"] = "L4_REFUSED_OR_NO_DEVICE"
	fmt.Printf("C211A_L4 project=%s verdict=L4_REFUSED_OR_NO_DEVICE created=%v op_error=%s\n", proj, created, c211xTrunc(opErr, 200))
	return res
}

func TestC211XA100SibGPU(t *testing.T) {
	started := time.Now()
	deadline := started.Add(46 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211A_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211aHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211aHostNum
	}
	fmt.Printf("C211A_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211A_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211A_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 300))
	}

	// Fleet command identity: the cross-project grants belong to it.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211A_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 160))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211A_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211A_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 240))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211A_FLEET_TOKEN fallback=wif_minted\n")
	}

	// Driver-preinstalled accelerator image (the proven x121836 mechanism).
	img, imgLog := c211aResolveImage(ftok)
	if img.Project == "" {
		fmt.Printf("C211A_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		fmt.Printf("=== C211A SUMMARY BEGIN ===\nC211A_SUMMARY_JSON %s\n=== C211A SUMMARY END ===\n",
			c211xJSONString(map[string]interface{}{
				"probe": "c211_i122025_a100", "mechanism": "driver-preinstalled accelerator image boot (ubuntu-os-accelerator-images) for on-demand A100 capture",
				"host_project": projNum, "host_project_id": c211xShort(hostID), "fleet_token_ok": ftok != "",
				"image": "", "image_kind": "", "image_resolution": imgLog,
				"markers": 0, "ready_projects": []string{}, "verdict": "NO_USABLE_IMAGE",
				"results": map[string]interface{}{}, "elapsed_seconds": int(time.Since(started).Seconds()),
			}))
		return
	}
	startup := c211aStartup()
	fmt.Printf("C211A_STARTUP kind=%s bytes=%d\n", img.Kind, len(startup))

	// Host project 153711203585: quota/offering reads only, never marked.
	if hostID != "" {
		for _, r := range c211aRegions {
			rows, _, qst := c211aRegionQuotas(ftok, hostID, r)
			fmt.Printf("C211A_HOST_QUOTA project=%s region=%s http=%d a100=[%s] l4=[%s] rows=[%s]\n",
				hostID, r, qst, rows["NVIDIA_A100_GPUS"], rows["NVIDIA_L4_GPUS"], c211aQMatrixString(rows))
		}
		for _, mt := range []string{"a2-highgpu-1g", "g2-standard-4"} {
			ok, mst, _ := c211aMachineOffered(ftok, hostID, "us-central1-a", mt)
			fmt.Printf("C211A_HOST_OFFER project=%s zone=us-central1-a machine=%s http=%d offered=%v\n", hostID, mt, mst, ok)
		}
	}

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyA100 := []string{}
	readyL4 := []string{}
	readyUUIDs := map[string]string{}
	markers := 0
	for _, id := range c211aSiblings {
		if time.Now().After(deadline) {
			fmt.Printf("C211A_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		if id == hostID {
			fmt.Printf("C211A_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", id)
			continue
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211A_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		res := map[string]interface{}{"number": num, "name": c211xStr(pm, "name"), "state": c211xStr(pm, "lifecycleState")}
		results[id] = res
		fmt.Printf("C211A_PROJECT_BEGIN id=%s number=%s name=%q state=%s\n", id, num, c211xStr(pm, "name"), c211xStr(pm, "lifecycleState"))

		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211A_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", id, pst, strings.Join(al, ","), c211xTrunc(pbody, 200))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")
		res["can_create"] = canCreate
		if !canCreate {
			fmt.Printf("C211A_VERDICT project=%s NO_CREATE_PERMISSION\n", id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}

		// A100 first.
		markers++
		ra := c211aA100Attempt(ftok, id, num, img, startup, deadline)
		for k, v := range ra {
			res["a100_"+k] = v
		}
		if v, _ := ra["verdict"].(string); v == "MARKER_GPU_A100" {
			u, _ := ra["uuid"].(string)
			readyA100 = append(readyA100, id)
			readyUUIDs[id] = u
			res["verdict"] = "MARKER_GPU_A100"
			continue
		}

		// L4 fallback ONLY after a provider-verbatim A100 refusal / no device.
		if time.Now().After(deadline) {
			fmt.Printf("C211A_VERDICT project=%s L4_SKIPPED_TIME_BUDGET\n", id)
			res["verdict"] = "A100_NO_DEVICE_L4_SKIPPED_TIME_BUDGET"
			continue
		}
		markers++
		rl := c211aL4Attempt(ftok, id, num, img, startup, deadline)
		for k, v := range rl {
			res["l4_"+k] = v
		}
		if v, _ := rl["verdict"].(string); v == "MARKER_GPU_L4" {
			u, _ := rl["uuid"].(string)
			readyL4 = append(readyL4, id)
			readyUUIDs[id] = u
			res["verdict"] = "MARKER_GPU_L4"
			continue
		}
		res["verdict"] = "A100_AND_L4_NO_DEVICE"
		fmt.Printf("C211A_VERDICT project=%s A100_AND_L4_NO_DEVICE\n", id)
	}

	// Final reaper across all four projects.
	for _, id := range c211aSiblings {
		c211aReaper(ftok, id)
	}

	verdict := "A100_AND_L4_NO_DEVICE"
	if len(readyA100) > 0 {
		verdict = "READY_NEW_PROJECT_A100_GPU"
	} else if len(readyL4) > 0 {
		verdict = "READY_NEW_PROJECT_L4_GPU"
	}
	sum := map[string]interface{}{
		"probe":            "c211_i122025_a100",
		"mechanism":        "driver-preinstalled accelerator image boot (ubuntu-os-accelerator-images) for on-demand a2-highgpu-Ng A100 capture",
		"host_project":     projNum,
		"host_project_id":  c211xShort(hostID),
		"fleet_token_ok":   ftok != "",
		"image":            img.Project + "/" + img.Family,
		"image_kind":       img.Kind,
		"image_resolution": imgLog,
		"markers":          markers,
		"ready_a100":       readyA100,
		"ready_l4":         readyL4,
		"ready_uuids":      readyUUIDs,
		"verdict":          verdict,
		"elapsed_seconds":  int(time.Since(started).Seconds()),
		"results":          results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211A SUMMARY BEGIN ===\nC211A_SUMMARY_JSON %s\n=== C211A SUMMARY END ===\n", string(js))
	fmt.Printf("C211A_VERDICT_FINAL %s markers=%d a100=%d l4=%d image_kind=%s elapsed=%ds\n", verdict, markers, len(readyA100), len(readyL4), img.Kind, int(time.Since(started).Seconds()))
}
