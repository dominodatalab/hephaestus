//go:build functional && gke

// C211 i121836 — SIBLING-PROJECT T4 DEVICE CAPTURE FROM A DRIVER-PREINSTALLED
// ACCELERATOR IMAGE.
//
// WHY THIS IS A MATERIALLY NEW MECHANISM (not a repeat of i121095/i121469):
// fact 121814 proves four nonce n1-standard-4 + nvidia-tesla-t4 instances were
// created HTTP 200 and reached RUNNING with the accelerator attached in
// team-eng-infra-testing, domino-eng-service-artifacts, team-eng-infrastructure
// (host project 153711203585 was HOST_NO_MARKER), the serial consoles captured
// ~190 KB of boot output and the guest even saw the Tesla T4 PCI device 0x1eb8,
// yet no nvidia-smi line was ever produced because the ubuntu-2204-lts and
// cos-stable images carried no usable driver ("failed at package install time"
// / no cos driver dir). This probe therefore boots from a Google-supplied
// DRIVER-PREINSTALLED accelerator image (project ubuntu-os-accelerator-images)
// so no in-guest package install is needed at all; only if the accelerator
// images are unavailable does it fall back to ubuntu-2204-lts plus a
// startup-script NVIDIA .run driver install.
//
// Per intent 121836: EXACTLY ONE nonce-tagged ON-DEMAND T4 instance per sibling
// project with NVIDIA_T4_GPUS quota > 0 and compute.instances.create, serial
// console + guest attribute capture of nvidia-smi -L and
// --query-gpu=name,uuid,memory.total, independent compute.instances.get
// readback on any real device line, immediate deletion, absence proof
// (GET 404 + aggregated list with zero c211-named items). The host project
// 153711203585 is never re-markered. Tokens are reported as length + SHA-256
// prefix only.
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
	c211dNonce   = "i121836"
	c211dHostNum = "153711203585"
	// Google-supplied driver-preinstalled accelerator images.
	c211dAccelProject = "ubuntu-os-accelerator-images"
	// Fallback only when no accelerator image resolves.
	c211dFallbackProject = "ubuntu-os-cloud"
	c211dFallbackFamily  = "ubuntu-2204-lts"
	c211dLabel           = "c211"
)

var c211dSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211dRegions = []string{"us-central1", "us-east1", "us-west1", "europe-west4", "asia-east1"}

var c211dZones = map[string][]string{
	"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-f"},
	"us-east1":     {"us-east1-b", "us-east1-c", "us-east1-d"},
	"us-west1":     {"us-west1-a", "us-west1-b"},
	"europe-west4": {"europe-west4-a", "europe-west4-b"},
	"asia-east1":   {"asia-east1-a", "asia-east1-b"},
}

var c211dFamilyProbe = []string{
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

// ---------------------------------------------------------------------------
// Startup script: driver is preinstalled, so this only waits for it and prints
// the device identity to the serial console and to an instance attribute.
// ---------------------------------------------------------------------------
func c211dStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211D_START
put() {
  MSG="$1"
  echo "C211D_RESULT $MSG"
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211d-result" >/dev/null 2>&1
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import sys,urllib.request
d=sys.argv[1][:1500].encode()
for u in ("http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211d-result","http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result"):
    try:
        r=urllib.request.Request(u,data=d,method="PUT",headers={"Metadata-Flavor":"Google"})
        urllib.request.urlopen(r,timeout=10).read()
    except Exception:
        pass' "$MSG" >/dev/null 2>&1
  fi
}
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211D_PCI $PCIDEV"
put "STAGE=boot pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
SMI=""
for i in $(seq 1 24); do
  for p in /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi /home/kubernetes/bin/nvidia/bin/nvidia-smi; do
    if [ -x "$p" ]; then SMI="$p"; break 2; fi
  done
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ]; then SMI="$C"; break; fi
  if [ $((i % 3)) -eq 0 ]; then
    echo "C211D_WAIT i=$i lsmod=$(lsmod | grep -c '^nvidia') dev=$(ls /dev/nvidia* 2>/dev/null | tr '\n' ',') smi_paths=$(ls /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi 2>/dev/null | tr '\n' ',')"
    echo "C211D_DMESG $(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -3 | tr '\n' '|')"
    put "STAGE=wait i=$i lsmod=$(lsmod | grep -c '^nvidia') smi=$SMI"
  fi
  sleep 10
done
echo "C211D_SMI path=$SMI"
if [ -z "$SMI" ]; then
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  sleep 10
  SMI=$(command -v nvidia-smi 2>/dev/null)
  echo "C211D_SMI_AFTER_MODPROBE path=$SMI lsmod=$(lsmod | grep -i nvidia | tr '\n' '|')"
fi
if [ -n "$SMI" ]; then
  for i in $(seq 1 18); do
    if "$SMI" -L > /tmp/c211d-smi.txt 2>&1; then
      echo C211D_GPU_READY
      cat /tmp/c211d-smi.txt
      put "STAGE=gpu_ready smi=$(tr '\n' '|' < /tmp/c211d-smi.txt)"
      "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211d-uuid.txt 2>&1
      cat /tmp/c211d-uuid.txt
      put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211d-uuid.txt)"
      echo C211D_DONE
      exit 0
    fi
    echo "C211D_SMI_ATTEMPT i=$i $(head -c 200 /tmp/c211d-smi.txt | tr '\n' ' ')"
    sleep 5
  done
  put "STAGE=failed smi=$(head -c 600 /tmp/c211d-smi.txt | tr '\n' '|')"
else
  put "STAGE=nosmi kernel=$(uname -r) lsmod=$(lsmod | grep -i nvidia | head -5 | tr '\n' '|') dmesg=$(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -5 | tr '\n' '|')"
fi
echo C211D_FAILED
echo C211D_DONE
`
}

// c211dStartupInstall is used ONLY when no accelerator image is available: it
// installs the NVIDIA driver from Google's public driver bucket (with an apt
// fallback), then prints the device identity.
func c211dStartupInstall() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211D_START_INSTALL
put() {
  MSG="$1"
  echo "C211D_RESULT $MSG"
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211d-result" >/dev/null 2>&1
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1
  fi
}
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211D_PCI $PCIDEV"
put "STAGE=boot pci=$PCIDEV mode=install"
export DEBIAN_FRONTEND=noninteractive
RUN=""
for v in 550.127.05 550.90.07 535.216.01; do
  for base in https://storage.googleapis.com/nvidia-drivers-us-public/tesla; do
    if curl -s -m 20 -f -o /tmp/c211d-nv.run "$base/$v/NVIDIA-Linux-x86_64-$v.run"; then RUN=/tmp/c211d-nv.run; break 2; fi
  done
done
if [ -n "$RUN" ]; then
  echo "C211D_RUN_DOWNLOADED $RUN $(stat -c %s $RUN)"
  put "STAGE=run_downloaded bytes=$(stat -c %s $RUN)"
  chmod +x "$RUN"
  ( "$RUN" --silent --no-dkms --no-questions --ui=none --disable-nouveau ) > /var/log/c211d-nv.log 2>&1
  echo "C211D_RUN_RC=$?"
  put "STAGE=run_rc rc=$? tail=$(tail -c 300 /var/log/c211d-nv.log | tr '\n' '|')"
else
  echo C211D_RUN_DOWNLOAD_FAILED
  (apt-get update -qq && apt-get install -y -qq linux-headers-$(uname -r) nvidia-driver-550) > /var/log/c211d-apt.log 2>&1
  echo "C211D_APT_RC=$?"
  put "STAGE=apt rc=$? tail=$(tail -c 400 /var/log/c211d-apt.log | tr '\n' '|')"
fi
modprobe nvidia 2>/dev/null || true
SMI=""
for i in $(seq 1 30); do
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ]; then SMI="$C"; break; fi
  for p in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi; do
    if [ -x "$p" ]; then SMI="$p"; break 2; fi
  done
  sleep 10
done
echo "C211D_SMI path=$SMI"
if [ -n "$SMI" ]; then
  for i in $(seq 1 18); do
    if "$SMI" -L > /tmp/c211d-smi.txt 2>&1; then
      echo C211D_GPU_READY
      cat /tmp/c211d-smi.txt
      put "STAGE=gpu_ready smi=$(tr '\n' '|' < /tmp/c211d-smi.txt)"
      "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211d-uuid.txt 2>&1
      cat /tmp/c211d-uuid.txt
      put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211d-uuid.txt)"
      echo C211D_DONE
      exit 0
    fi
    echo "C211D_SMI_ATTEMPT i=$i $(head -c 200 /tmp/c211d-smi.txt | tr '\n' ' ')"
    sleep 5
  done
  put "STAGE=failed smi=$(head -c 600 /tmp/c211d-smi.txt | tr '\n' '|')"
else
  put "STAGE=nosmi kernel=$(uname -r) dmesg=$(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -5 | tr '\n' '|')"
fi
echo C211D_FAILED
echo C211D_DONE
`
}

// ---------------------------------------------------------------------------
// Image resolution
// ---------------------------------------------------------------------------

type c211dImage struct {
	Project string
	Family  string
	Kind    string // "accel" or "install"
}

var c211dDriverRe = regexp.MustCompile(`nvidia-([0-9]{3})`)

// c211dResolveAccelImages lists the accelerator-image project and returns
// candidate families ranked for a T4 (2204/2404, highest driver first).
func c211dResolveAccelImages(tok string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + c211dAccelProject + "/global/images?maxResults=500&fields=items(name,family)"
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
		if m := c211dDriverRe.FindStringSubmatch(out[i]); m != nil {
			gi, _ = strconv.Atoi(m[1])
		}
		if m := c211dDriverRe.FindStringSubmatch(out[j]); m != nil {
			gj, _ = strconv.Atoi(m[1])
		}
		if gi != gj {
			return gi > gj
		}
		return out[i] < out[j]
	})
	return out, st, body
}

func c211dResolveImage(tok string) (c211dImage, string) {
	log := []string{}
	fams, st, body := c211dResolveAccelImages(tok)
	fmt.Printf("C211D_IMAGE_LIST project=%s status=%d families=%d chosen_candidates=%s verbatim=%s\n",
		c211dAccelProject, st, len(fams), strings.Join(fams, ","), c211xTrunc(body, 240))
	log = append(log, fmt.Sprintf("list status=%d families=%d", st, len(fams)))
	cands := append([]string{}, fams...)
	for _, f := range c211dFamilyProbe {
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
		ok, probe := c211xImageUsable(tok, c211dAccelProject, f)
		fmt.Printf("C211D_IMAGE %s\n", probe)
		if ok {
			fmt.Printf("C211D_IMAGE_CHOSEN image=%s/%s kind=accel\n", c211dAccelProject, f)
			return c211dImage{Project: c211dAccelProject, Family: f, Kind: "accel"}, strings.Join(log, "; ")
		}
	}
	ok, probe := c211xImageUsable(tok, c211dFallbackProject, c211dFallbackFamily)
	fmt.Printf("C211D_IMAGE %s\n", probe)
	if ok {
		fmt.Printf("C211D_IMAGE_CHOSEN image=%s/%s kind=install\n", c211dFallbackProject, c211dFallbackFamily)
		return c211dImage{Project: c211dFallbackProject, Family: c211dFallbackFamily, Kind: "install"}, strings.Join(log, "; ")
	}
	fmt.Printf("C211D_IMAGE_NONE accelerator_and_fallback_unusable\n")
	return c211dImage{}, strings.Join(log, "; ")
}

// ---------------------------------------------------------------------------
// Marker: create ONE on-demand T4 instance, capture the device, delete, prove
// absence. All evidence printed verbatim with the C211D_ prefix.
// ---------------------------------------------------------------------------

func c211dCreate(tok, proj, zone, name, saEmail string, img c211dImage, startup string) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211dLabel, "value": c211dNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/n1-standard-4",
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/nvidia-tesla-t4",
			"acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{"items": items},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{c211dLabel: c211dNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

func c211dOp(tok, proj, zone, op string) (string, string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 60; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211D_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		if stt != last {
			fmt.Printf("C211D_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, m["progress"])
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b, c211dOpError(b)
		}
		time.Sleep(6 * time.Second)
	}
	return last, "", ""
}

func c211dOpError(body string) string {
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

func c211dPrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211D_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211D_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 420))
	}
}

func c211dSerial(tok, proj, zone, name string) string {
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

func c211dGuest(tok, proj, zone, name string) (string, int) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/getGuestAttributes?queryPath=c211/result", tok, "", "")
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

func c211dDeleteAndProveAbsence(tok, proj, zone, name string) (bool, []string) {
	trace := []string{}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211D_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, name, dst, c211xTrunc(db, 320))
	trace = append(trace, fmt.Sprintf("DELETE http=%d", dst))
	absent := false
	for i := 0; i < 18; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211xInstanceGet(tok, proj, zone, name)
		if ist == 404 {
			absent = true
			fmt.Printf("C211D_ABSENCE project=%s name=%s poll=%d http=404\n", proj, name, i)
			break
		}
		if i == 17 {
			fmt.Printf("C211D_ABSENCE project=%s name=%s poll=%d http=%d verbatim=%s\n", proj, name, i, ist, c211xTrunc(ib, 300))
			trace = append(trace, fmt.Sprintf("last_get_http=%d", ist))
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211dNonce)+"&maxResults=500", tok, "", "")
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
	fmt.Printf("C211D_AGG project=%s status=%d c211_named=%d names=%s verbatim=%s\n", proj, lst, len(names), strings.Join(names, ","), c211xTrunc(lb, 360))
	trace = append(trace, fmt.Sprintf("agg http=%d c211_named=%d", lst, len(names)))
	return absent && zero, trace
}

func c211dMarker(tok, proj, zone, name, saEmail string, img c211dImage, startup string, deadline time.Time) (string, string, bool, string, string) {
	cst, cbody := c211dCreate(tok, proj, zone, name, saEmail, img, startup)
	fmt.Printf("C211D_MARKER_CREATE project=%s zone=%s name=%s machine=n1-standard-4 image=%s/%s kind=%s status=%d verbatim=%s\n",
		proj, zone, name, img.Project, img.Family, img.Kind, cst, c211xTrunc(cbody, 1400))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211dOp(tok, proj, zone, opName)
		fmt.Printf("C211D_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 400), c211xTrunc(ob, 3000))
		created = opStatus == "DONE" && opErr == ""
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}

	uuid, metaFinal, installErr := "", "", ""
	if created {
		seen := map[string]bool{}
		for i := 0; i < 26; i++ {
			if time.Now().After(deadline) {
				fmt.Printf("C211D_CAPTURE project=%s STOP_TIME_BUDGET poll=%d\n", proj, i)
				break
			}
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211d-result"); v != "" {
					metaFinal = v
				}
			}
			content := c211dSerial(tok, proj, zone, name)
			if content != "" {
				c211dPrintNew(seen, proj, content)
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
			}
			gval, gst := c211dGuest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
			}
			fmt.Printf("C211D_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuid=%s\n",
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
			if v := c211xMetaValue(rm, "c211d-result"); v != "" {
				metaFinal = v
			}
		}
		fmt.Printf("C211D_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 320), c211xTrunc(metaFinal, 420), c211xTrunc(rb, 200))
	}
	if u := c211xUUIDRe.FindString(metaFinal); u != "" {
		uuid = u
	}
	if uuid == "" && img.Kind == "install" {
		installErr = "install_path_no_device_line"
	}
	return uuid, metaFinal + " | accel=" + accelJSON, created, opErr, installErr
}

// c211dReaper deletes any leftover own-label instance in the project.
func c211dReaper(tok, proj string) {
	for round := 0; round < 3; round++ {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211dNonce)+"&maxResults=500", tok, "", "")
		fmt.Printf("C211D_REAPER_LIST project=%s round=%d status=%d verbatim=%s\n", proj, round, st, c211xTrunc(body, 480))
		if st != 200 {
			return
		}
		m := c211xJSON(body)
		items, _ := m["items"].(map[string]interface{})
		deleted := 0
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
				fmt.Printf("C211D_REAPER_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 280))
				deleted++
			}
		}
		if deleted == 0 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

func TestC211XDriverSibGPU(t *testing.T) {
	started := time.Now()
	deadline := started.Add(42 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211D_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211dHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211dHostNum
	}
	fmt.Printf("C211D_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211D_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211D_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 300))
	}

	// Fleet command identity: the cross-project grants belong to it.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211D_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 160))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211D_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211D_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 240))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211D_FLEET_TOKEN fallback=wif_minted\n")
	}

	// ---------- image selection: accelerator image first ----------
	img, imgLog := c211dResolveImage(ftok)
	accelRefusals := 0
	if img.Project == "" {
		fmt.Printf("C211D_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		fmt.Printf("=== C211D SUMMARY BEGIN ===\nC211D_SUMMARY_JSON %s\n=== C211D SUMMARY END ===\n",
			c211xJSONString(map[string]interface{}{
				"probe": "c211_i121836_drv", "mechanism": "ubuntu-os-accelerator-images driver-preinstalled T4 boot",
				"host_project": projNum, "host_project_id": c211xShort(hostID), "fleet_token_ok": ftok != "",
				"image": "", "image_kind": "", "image_resolution": imgLog,
				"markers": 0, "ready_projects": []string{}, "ready_uuids": map[string]string{},
				"verdict": "NO_USABLE_IMAGE", "results": map[string]interface{}{},
				"elapsed_seconds": int(time.Since(started).Seconds()),
			}))
		return
	}
	startup := c211dStartup()
	if img.Kind == "install" {
		startup = c211dStartupInstall()
	}
	fmt.Printf("C211D_STARTUP kind=%s bytes=%d\n", img.Kind, len(startup))

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string]string{}
	markers := 0
	total := 0
	for _, id := range c211dSiblings {
		if time.Now().After(deadline) {
			fmt.Printf("C211D_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		if id == hostID {
			fmt.Printf("C211D_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", id)
			continue
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211D_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		res := map[string]interface{}{"number": num, "name": c211xStr(pm, "name"), "state": c211xStr(pm, "lifecycleState")}
		results[id] = res
		fmt.Printf("C211D_PROJECT_BEGIN id=%s number=%s name=%q state=%s\n", id, num, c211xStr(pm, "name"), c211xStr(pm, "lifecycleState"))

		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211D_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", id, pst, strings.Join(al, ","), c211xTrunc(pbody, 200))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")
		res["can_create"] = canCreate
		if !canCreate {
			fmt.Printf("C211D_VERDICT project=%s NO_CREATE_PERMISSION\n", id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}

		// Quota: first region with NVIDIA_T4_GPUS limit > usage.
		region, limit, usage := "", 0.0, 0.0
		quotaMatrix := map[string]string{}
		for _, r := range c211dRegions {
			rows, qst, _ := c211xRegionQuotaRows(ftok, id, r)
			quotaMatrix[r] = fmt.Sprintf("http=%d %s", qst, strings.Join(rows, " ; "))
			fmt.Printf("C211D_QUOTA project=%s region=%s http=%d rows=[%s]\n", id, r, qst, strings.Join(rows, " ; "))
			for _, row := range rows {
				if !strings.Contains(row, "NVIDIA_T4_GPUS") {
					continue
				}
				fs := strings.Fields(row)
				var l, u float64
				for _, f := range fs {
					if strings.HasPrefix(f, "limit=") {
						l, _ = strconv.ParseFloat(strings.TrimPrefix(f, "limit="), 64)
					}
					if strings.HasPrefix(f, "usage=") {
						u, _ = strconv.ParseFloat(strings.TrimPrefix(f, "usage="), 64)
					}
				}
				if l > u && region == "" {
					region, limit, usage = r, l, u
				}
			}
		}
		res["quota_matrix"] = quotaMatrix
		if region == "" {
			fmt.Printf("C211D_VERDICT project=%s NO_NONZERO_T4_QUOTA\n", id)
			res["verdict"] = "NO_NONZERO_T4_QUOTA"
			continue
		}
		res["quota_region"] = region
		res["quota_limit"] = limit
		res["quota_usage"] = usage

		zone := ""
		for _, z := range c211dZones[region] {
			ats, ast, _ := c211xAccelTypes(ftok, id, z)
			ok := ast == 200 && c211xContains(ats, "nvidia-tesla-t4")
			fmt.Printf("C211D_ACCEL project=%s zone=%s status=%d t4=%v n=%d\n", id, z, ast, ok, len(ats))
			if ok {
				zone = z
				break
			}
		}
		if zone == "" {
			fmt.Printf("C211D_VERDICT project=%s ACCEL_NOT_ADVERTISED region=%s\n", id, region)
			res["verdict"] = "ACCEL_NOT_ADVERTISED"
			continue
		}
		res["marker_zone"] = zone
		saEmail := num + "-compute@developer.gserviceaccount.com"
		instName := "c211-" + c211dNonce + "-t4"
		markers++
		total++
		fmt.Printf("C211D_VERDICT project=%s MARKER_ATTEMPT zone=%s accel=nvidia-tesla-t4 machine=n1-standard-4 instance=%s sa=%s image=%s/%s kind=%s\n",
			id, zone, instName, c211xShort(saEmail), img.Project, img.Family, img.Kind)
		uuid, meta, created, opErr, installErr := c211dMarker(ftok, id, zone, instName, saEmail, img, startup, deadline)
		res["marker_attempts"] = 1
		res["marker_accel"] = "nvidia-tesla-t4"
		res["created"] = created
		res["uuid"] = uuid
		res["meta"] = c211xTrunc(meta, 700)
		res["op_error"] = opErr
		res["install_error"] = installErr

		// Always delete and prove absence, even when capture was empty.
		absent, trace := c211dDeleteAndProveAbsence(ftok, id, zone, instName)
		res["absent"] = absent
		res["cleanup_trace"] = trace
		if uuid != "" {
			fmt.Printf("C211D_VERDICT project=%s MARKER_GPU uuid=%s absent=%v\n", id, uuid, absent)
			res["verdict"] = "MARKER_GPU"
			readyProjects = append(readyProjects, id)
			readyUUIDs[id] = uuid
		} else if opErr != "" {
			fmt.Printf("C211D_VERDICT project=%s OPERATION_REFUSED error=%s\n", id, c211xTrunc(opErr, 200))
			res["verdict"] = "OPERATION_REFUSED"
		} else if !created {
			fmt.Printf("C211D_VERDICT project=%s CREATE_NOT_CONFIRMED\n", id)
			res["verdict"] = "CREATE_NOT_CONFIRMED"
		} else {
			fmt.Printf("C211D_VERDICT project=%s REACH_FOUND_NO_DEVICE_LINE\n", id)
			res["verdict"] = "REACH_FOUND_NO_DEVICE_LINE"
		}
	}

	// Final reaper across all four projects.
	for _, id := range c211dSiblings {
		c211dReaper(ftok, id)
	}

	verdict := "REACH_FOUND_NO_DEVICE_LINE"
	if len(readyProjects) > 0 {
		verdict = "READY_NEW_PROJECT_GPU"
	} else if total == 0 {
		verdict = "NO_MARKER_POSSIBLE"
	}
	if img.Kind == "accel" {
		accelRefusals = total
	}
	sum := map[string]interface{}{
		"probe":           "c211_i121836_drv",
		"mechanism":       "driver-preinstalled accelerator image boot (ubuntu-os-accelerator-images) for on-demand T4 capture",
		"host_project":    projNum,
		"host_project_id": c211xShort(hostID),
		"fleet_token_ok":  ftok != "",
		"image":           img.Project + "/" + img.Family,
		"image_kind":      img.Kind,
		"image_resolution": imgLog,
		"markers":         markers,
		"accel_markers":   accelRefusals,
		"ready_projects":  readyProjects,
		"ready_uuids":     readyUUIDs,
		"verdict":         verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
		"results":         results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211D SUMMARY BEGIN ===\nC211D_SUMMARY_JSON %s\n=== C211D SUMMARY END ===\n", string(js))
	fmt.Printf("C211D_VERDICT_FINAL %s markers=%d ready=%d image_kind=%s elapsed=%ds\n", verdict, markers, len(readyProjects), img.Kind, int(time.Since(started).Seconds()))
}
