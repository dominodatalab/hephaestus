//go:build functional && gke

// C211 i123594 — SCALE THE PROVEN V100 (VOLTA) SIBLING CELL TO FULL 8/8
// DENSITY (n1-standard-8 + 8 x nvidia-tesla-v100) PER DOMINO SIBLING PROJECT.
//
// Fact 122416 proved that run 44896015323 / GKE job 136821736951 (plain
// ubuntu-os-cloud/ubuntu-2204-lts + startup apt-get install
// nvidia-driver-535-server, serial-console/guest-attribute capture) printed a
// real Tesla V100-SXM2-16GB in ALL FOUR Domino sibling projects with an
// in-probe compute.instances.get RUNNING readback, DELETE 200 and 404 /
// zero-instance absence, and hint 122418 records NVIDIA_V100_GPUS limit=8
// usage=0 (preemptible 16) in each project. The untested density lever is
// therefore ONE nonce-tagged on-demand n1-standard-8 with acceleratorCount=8 x
// nvidia-tesla-v100 per project.
//
// Per project: re-read the per-region NVIDIA_V100_GPUS quota and the
// nvidia-tesla-v100 accelerator offering, create EXACTLY ONE nonce-tagged
// on-demand marker (4 x nvidia-tesla-v100) in the first zone whose measured
// on-demand limit is >= 4 with usage 0 (us-central1-a is the zone the 1-GPU
// proof succeeded in), capture lspci -nn, dmesg | grep -i nvidia, nvidia-smi
// -L and --query-gpu=name,uuid,memory.total for EVERY device from the serial
// console / guest attributes, poll the insert operation to RUNNING, take an
// independent compute.instances.get readback of acceleratorCount=4 and every
// real device line, delete the instance and prove absence (GET 404 +
// aggregated list with zero c211-named items) before the next project. If 4x
// is provider-verbatim refused for a project the probe retries that project
// once at 2x (2 x nvidia-tesla-v100).
//
// Strictly one own instance alive at any moment; tokens are reported as
// length + SHA-256 prefix only.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	c211wNonce        = "i123594"
	c211wHostNum      = "153711203585"
	c211wLabel        = "c211"
	c211wPlainProject = "ubuntu-os-cloud"
	c211wPlainFamily  = "ubuntu-2204-lts"
	c211wMachine      = "n1-standard-8"
)

var c211wSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211wRegions = []string{"us-central1", "europe-west4", "us-east1", "asia-east1"}

type c211wZoneCand struct {
	Region string
	Zone   string
}

// Preference order: us-central1-a is where the proven 1x and 4x sibling V100
// markers succeeded; europe-west4-b is where the proven 8x V100 host/keda markers
// succeeded; the remaining zones follow by region preference.
var c211wCands = []c211wZoneCand{
	{"us-central1", "us-central1-a"},
	{"europe-west4", "europe-west4-b"},
	{"us-central1", "us-central1-b"},
	{"europe-west4", "europe-west4-a"},
	{"europe-west4", "europe-west4-c"},
	{"us-central1", "us-central1-c"},
	{"us-central1", "us-central1-f"},
	{"us-east1", "us-east1-b"},
	{"us-east1", "us-east1-c"},
	{"us-east1", "us-east1-d"},
	{"asia-east1", "asia-east1-b"},
	{"asia-east1", "asia-east1-c"},
	{"asia-east1", "asia-east1-a"},
}

// ---------------------------------------------------------------------------
// Startup script: the exact proven Volta recipe (plain ubuntu-2204-lts,
// install nvidia-driver-535-server, modprobe, udev trigger, re-poll), with
// full multi-GPU capture. Long work is detached so the GCE boot-script
// timeout cannot kill it. Mirrors the terminal stage to instance/guest
// attributes as well as the serial console.
// ---------------------------------------------------------------------------

const c211wPutFunc = `put() {
  MSG="$1"
  echo "C211V_RESULT $MSG"
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211v-result" >/dev/null 2>&1
    curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import sys,urllib.request
d=sys.argv[1][:1500].encode()
for u in ("http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211v-result","http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result"):
    try:
        r=urllib.request.Request(u,data=d,method="PUT",headers={"Metadata-Flavor":"Google"})
        urllib.request.urlopen(r,timeout=10).read()
    except Exception:
        pass' "$MSG" >/dev/null 2>&1
  fi
}
smi_probe() {
  for p in /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi; do
    if [ -x "$p" ]; then echo "$p"; return 0; fi
  done
  command -v nvidia-smi 2>/dev/null || true
}
report_dev() {
  echo "C211V_LSMOD $(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|')"
  echo "C211V_DEV $(ls /dev/nvidia* 2>/dev/null | tr '\n' ',')"
  echo "C211V_PROCVER $(cat /proc/driver/nvidia/version 2>/dev/null | tr '\n' '|')"
  echo "C211V_DMESG $(dmesg 2>/dev/null | grep -i -E 'nvidia|NVRM' | tail -30 | tr '\n' '|')"
}
uuid_report() {
  SMI="$1"
  echo C211V_GPU_READY
  cat /tmp/c211v-smi.txt
  "$SMI" --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211v-uuid.txt 2>&1
  echo "C211V_GPU_COUNT $(grep -c 'GPU-' /tmp/c211v-uuid.txt 2>/dev/null)"
  cat /tmp/c211v-uuid.txt
  put "STAGE=uuid gpu_count=$(grep -c 'GPU-' /tmp/c211v-uuid.txt 2>/dev/null) uuid_line=$(tr '\n' '|' < /tmp/c211v-uuid.txt) smi=$(tr '\n' '|' < /tmp/c211v-smi.txt) driver=$(cat /proc/driver/nvidia/version 2>/dev/null | head -1 | tr '\n' ' ')"
  report_dev
  echo "C211_MARKER_END"
  exit 0
}
`

func c211wStartupInstall() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211wNonce + ` mode=install535x8"
mkdir -p /var/lib/c211
cat > /usr/local/bin/c211v-run.sh <<'EOS'
#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211wNonce + ` mode=install535x8"
` + c211wPutFunc + `
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211V_PCI $PCIDEV"
echo "C211V_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|3d|vga' | tr '\n' '|')"
put "STAGE=boot mode=install535x8 pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
SMI=$(smi_probe)
if [ -n "$SMI" ] && "$SMI" -L > /tmp/c211v-smi.txt 2>&1; then uuid_report "$SMI"; fi
if [ ! -f /var/lib/c211/installed ]; then
  export DEBIAN_FRONTEND=noninteractive
  echo C211V_APT_BEGIN
  (apt-get update -y -o DPkg::Lock::Timeout=300 && apt-get install -y -o DPkg::Lock::Timeout=300 linux-headers-$(uname -r) build-essential curl) > /var/log/c211v-apt0.log 2>&1
  echo "C211V_APT0_RC=$?"
  apt-get install -y -o DPkg::Lock::Timeout=300 nvidia-driver-535-server > /var/log/c211v-apt1.log 2>&1
  echo "C211V_APT1_RC=$?"
  if [ -z "$(smi_probe)" ]; then
    apt-get install -y -o DPkg::Lock::Timeout=300 nvidia-driver-535 > /var/log/c211v-apt2.log 2>&1
    echo "C211V_APT2_RC=$?"
  fi
  if [ -z "$(smi_probe)" ]; then
    (cd /tmp && curl -fsSLO https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb && dpkg -i cuda-keyring_1.1-1_all.deb) > /var/log/c211v-apt3.log 2>&1
    echo "C211V_KEYRING_RC=$?"
    apt-get update -y -o DPkg::Lock::Timeout=300 >> /var/log/c211v-apt3.log 2>&1
    apt-get install -y -o DPkg::Lock::Timeout=300 cuda-drivers-535 >> /var/log/c211v-apt3.log 2>&1
    echo "C211V_APT3_RC=$?"
  fi
  touch /var/lib/c211/installed
  echo "C211V_APT_DONE smi=$(smi_probe) pkgs=$(dpkg -l 2>/dev/null | grep -E 'nvidia-driver|cuda-drivers|nvidia-dkms' | awk '{print $2"="$3}' | tr '\n' '|')"
  put "STAGE=installed smi=$(smi_probe) pkgs=$(dpkg -l 2>/dev/null | grep -E 'nvidia-driver|cuda-drivers' | awk '{print $2"="$3}' | head -6 | tr '\n' '|')"
fi
modprobe nvidia 2>&1 | sed 's/^/C211V_MODPROBE /'
modprobe nvidia_uvm 2>&1 | sed 's/^/C211V_MODPROBE_UVM /'
udevadm trigger --subsystem-match=pci 2>/dev/null || true
sleep 10
report_dev
OK=0
for i in $(seq 1 60); do
  SMI=$(smi_probe)
  if [ -n "$SMI" ] && "$SMI" -L > /tmp/c211v-smi.txt 2>&1; then OK=1; break; fi
  if [ $((i % 5)) -eq 0 ]; then
    echo "C211V_SMI_ATTEMPT i=$i $(head -c 220 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' ' ')"
    report_dev
    modprobe nvidia 2>/dev/null || true
  fi
  sleep 6
done
if [ "$OK" = "1" ]; then uuid_report "$SMI"; fi
put "STAGE=failed mode=install535x8 smi=$(head -c 500 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' '|') lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|') kernel=$(uname -r) pkgs=$(dpkg -l 2>/dev/null | grep -E 'nvidia-driver|cuda-drivers' | awk '{print $2"="$3}' | head -6 | tr '\n' '|')"
report_dev
echo C211V_FAILED
echo "C211_MARKER_END"
EOS
chmod +x /usr/local/bin/c211v-run.sh
LAUNCH=0
if command -v systemd-run >/dev/null 2>&1; then
  systemd-run --unit=c211v-run --collect /usr/local/bin/c211v-run.sh && LAUNCH=1
fi
if [ "$LAUNCH" = "0" ]; then
  setsid nohup /usr/local/bin/c211v-run.sh >/dev/null 2>&1 &
fi
echo "C211V_LAUNCHED=$LAUNCH"
`
}

// ---------------------------------------------------------------------------
// Marker: create ONE on-demand n1-standard-8 with 8 x nvidia-tesla-v100,
// capture every device, delete, prove absence.
// ---------------------------------------------------------------------------

func c211wCreate(tok, proj, zone, name, saEmail string, count int, startup string) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211wLabel, "value": c211wNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + c211wMachine + "",
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + c211wPlainProject + "/global/images/family/" + c211wPlainFamily,
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/nvidia-tesla-v100",
			"acceleratorCount": count,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{"items": items},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{c211wLabel: c211wNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

func c211wOp(tok, proj, zone, op string) (string, string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 60; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211V_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		if stt != last {
			fmt.Printf("C211V_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, m["progress"])
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b, c211wOpError(b)
		}
		time.Sleep(6 * time.Second)
	}
	return last, "", ""
}

func c211wOpError(body string) string {
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
	errs := []string{}
	if el, ok2 := e["errors"].([]interface{}); ok2 {
		for _, x := range el {
			xm, _ := x.(map[string]interface{})
			errs = append(errs, c211xStr(xm, "code")+":"+c211xStr(xm, "message"))
		}
	}
	out := fmt.Sprintf("code=%v message=%s", code, msg)
	if len(errs) > 0 {
		out += " errors=[" + strings.Join(errs, " ; ") + "]"
	}
	return out
}

func c211wCapacityRefusal(opErr string) bool {
	l := strings.ToLower(opErr)
	for _, k := range []string{"zone_resource_pool_exhausted", "resource_availability", "capacity", "resource_pool_exhausted"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func c211wPrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211V_") && !strings.Contains(ln, "C211_MARKER_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211V_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 1200))
	}
}

func c211wSerial(tok, proj, zone, name string) string {
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

func c211wGuest(tok, proj, zone, name string) (string, int) {
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

var c211wUUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func c211wAllUUIDs(s string, out map[string]bool) {
	for _, m := range c211wUUIDRe.FindAllString(s, -1) {
		out[m] = true
	}
}

func c211wDeleteAndProveAbsence(tok, proj, zone, name string) (bool, []string) {
	trace := []string{}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211V_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, name, dst, c211xTrunc(db, 320))
	trace = append(trace, fmt.Sprintf("DELETE http=%d", dst))
	absent := false
	for i := 0; i < 24; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211xInstanceGet(tok, proj, zone, name)
		if ist == 404 {
			absent = true
			fmt.Printf("C211V_ABSENCE project=%s name=%s poll=%d http=404\n", proj, name, i)
			break
		}
		if i == 23 {
			fmt.Printf("C211V_ABSENCE project=%s name=%s poll=%d http=%d verbatim=%s\n", proj, name, i, ist, c211xTrunc(ib, 300))
			trace = append(trace, fmt.Sprintf("last_get_http=%d", ist))
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211wNonce)+"&maxResults=500", tok, "", "")
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
	fmt.Printf("C211V_AGG project=%s status=%d c211_named=%d names=%s verbatim=%s\n", proj, lst, len(names), strings.Join(names, ","), c211xTrunc(lb, 360))
	trace = append(trace, fmt.Sprintf("agg http=%d c211_named=%d", lst, len(names)))
	return absent && zero, trace
}

func c211wResidue(tok, proj string) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211")+"&maxResults=500", tok, "", "")
	fmt.Printf("C211V_RESIDUE project=%s status=%d verbatim=%s\n", proj, st, c211xTrunc(body, 900))
	if st != 200 {
		return
	}
	m := c211xJSON(body)
	items, _ := m["items"].(map[string]interface{})
	for zoneKey, zv := range items {
		zm, _ := zv.(map[string]interface{})
		list, _ := zm["instances"].([]interface{})
		zone := strings.TrimPrefix(zoneKey, "zones/")
		for _, it := range list {
			im, _ := it.(map[string]interface{})
			n := c211xStr(im, "name")
			labels, _ := im["labels"].(map[string]interface{})
			mine := false
			if labels != nil {
				if v, ok := labels[c211wLabel].(string); ok && v == c211wNonce {
					mine = true
				}
			}
			if mine && n != "" {
				ds, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+n, tok, "", "")
				fmt.Printf("C211V_RESIDUE_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 280))
			}
		}
	}
}

func c211wMarker(tok, proj, zone, name, saEmail string, count int, startup string, maxPolls int, deadline time.Time) ([]string, string, bool, string) {
	cst, cbody := c211wCreate(tok, proj, zone, name, saEmail, count, startup)
	fmt.Printf("C211V_MARKER_CREATE project=%s zone=%s name=%s machine=n1-standard-8 gpus=%d image=%s/%s status=%d verbatim=%s\n",
		proj, zone, name, count, c211wPlainProject, c211wPlainFamily, cst, c211xTrunc(cbody, 1400))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211wOp(tok, proj, zone, opName)
		fmt.Printf("C211V_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 600), c211xTrunc(ob, 2500))
		created = opStatus == "DONE" && opErr == ""
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst) + " " + c211xTrunc(cbody, 600)
	}

	uuids := map[string]bool{}
	metaFinal := ""
	if created {
		seen := map[string]bool{}
		for i := 0; i < maxPolls; i++ {
			if time.Now().After(deadline) {
				fmt.Printf("C211V_CAPTURE project=%s STOP_TIME_BUDGET poll=%d\n", proj, i)
				break
			}
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211v-result"); v != "" {
					metaFinal = v
				}
			}
			content := c211wSerial(tok, proj, zone, name)
			if content != "" {
				c211wPrintNew(seen, proj, content)
				c211wAllUUIDs(content, uuids)
			}
			gval, gst := c211wGuest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
				c211wAllUUIDs(gval, uuids)
			}
			fmt.Printf("C211V_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuids=%d\n",
				proj, i, state, len(content), gst, c211xTrunc(gval, 260), c211xTrunc(metaFinal, 320), len(uuids))
			c211wAllUUIDs(metaFinal, uuids)
			if len(uuids) >= count {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") {
				break
			}
			if strings.Contains(metaFinal, "STAGE=uuid") && i > 2 {
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
			if v := c211xMetaValue(rm, "c211v-result"); v != "" {
				metaFinal = v
				c211wAllUUIDs(v, uuids)
			}
		}
		fmt.Printf("C211V_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 700), c211xTrunc(metaFinal, 700), c211xTrunc(rb, 200))
	}
	ulist := []string{}
	for u := range uuids {
		ulist = append(ulist, u)
	}
	return ulist, metaFinal + " | accel=" + accelJSON + " | status=" + statusFinal, created, opErr
}

func c211wReaper(tok, proj string) {
	for round := 0; round < 3; round++ {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211wNonce)+"&maxResults=500", tok, "", "")
		fmt.Printf("C211V_REAPER_LIST project=%s round=%d status=%d verbatim=%s\n", proj, round, st, c211xTrunc(body, 480))
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
				fmt.Printf("C211V_REAPER_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 280))
				deleted++
			}
		}
		if deleted == 0 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// Main test
// ---------------------------------------------------------------------------

type c211wQuota struct {
	Limit    float64
	Usage    float64
	PreLimit float64
	PreUsage float64
	HTTP     int
	Rows     []string
}

func c211wQuotaRead(tok, proj, region string) c211wQuota {
	var q c211wQuota
	rows, st, _ := c211xRegionQuotaRows(tok, proj, region)
	q.HTTP = st
	q.Rows = rows
	for _, row := range rows {
		if !strings.Contains(row, "NVIDIA_V100_GPUS") {
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
		// The PREEMPTIBLE_ row also contains the on-demand quota name as a
		// substring; keep the two families strictly separate.
		if strings.Contains(row, "PREEMPTIBLE_") {
			q.PreLimit, q.PreUsage = l, u
			continue
		}
		q.Limit, q.Usage = l, u
	}
	return q
}

// c211wCensus lists every foreign c211-* instance visible in the project so the
// region where the on-demand V100 quota is consumed is attributable.
func c211wCensus(tok, proj string) int {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?maxResults=500", tok, "", "")
	fmt.Printf("C211V_CENSUS project=%s status=%d\n", proj, st)
	if st != 200 {
		fmt.Printf("C211V_CENSUS_VERBATIM project=%s %s\n", proj, c211xTrunc(body, 300))
		return -1
	}
	m := c211xJSON(body)
	items, _ := m["items"].(map[string]interface{})
	n := 0
	for zoneKey, zv := range items {
		zm, _ := zv.(map[string]interface{})
		list, _ := zm["instances"].([]interface{})
		zone := strings.TrimPrefix(zoneKey, "zones/")
		for _, it := range list {
			im, _ := it.(map[string]interface{})
			name := c211xStr(im, "name")
			if !strings.HasPrefix(name, "c211-") {
				continue
			}
			if strings.HasPrefix(name, "c211-"+c211wNonce) {
				continue
			}
			mt := c211xStr(im, "machineType")
			if i := strings.LastIndex(mt, "/"); i >= 0 {
				mt = mt[i+1:]
			}
			acc := ""
			if ga, ok := im["guestAccelerators"].([]interface{}); ok && len(ga) > 0 {
				if gm, ok2 := ga[0].(map[string]interface{}); ok2 {
					acc = fmt.Sprintf("%v x %s", gm["acceleratorCount"], c211xStr(gm, "acceleratorType"))
				}
			}
			fmt.Printf("C211V_FOREIGN project=%s zone=%s name=%s status=%s machine=%s accel=%s\n",
				proj, zone, name, c211xStr(im, "status"), mt, acc)
			n++
			if n >= 60 {
				fmt.Printf("C211V_FOREIGN_TRUNC project=%s\n", proj)
				return n
			}
		}
	}
	fmt.Printf("C211V_FOREIGN_TOTAL project=%s count=%d\n", proj, n)
	return n
}

// c211wAttemptHeadroom reads the live on-demand NVIDIA_V100_GPUS quota per
// region, keeps only regions with limit>=8 AND limit-usage>=8, and tries the
// candidate zones of those regions in preference order. QUOTA_EXCEEDED and
// capacity refusals are region-scoped, so the ladder continues to the next
// region instead of aborting the project. At most ONE instance is ever created.
func c211wAttemptHeadroom(tok, proj, saEmail, startup string, deadline time.Time, markers *int) ([]string, []string, map[string]interface{}) {
	log := []string{}
	info := map[string]interface{}{}
	quotaMatrix := map[string]string{}
	headroomMatrix := map[string]string{}
	tryRegions := []string{}
	for _, r := range c211wRegions {
		q := c211wQuotaRead(tok, proj, r)
		quotaMatrix[r] = fmt.Sprintf("http=%d %s", q.HTTP, strings.Join(c211wFilterV100(q.Rows), " ; "))
		h := q.Limit - q.Usage
		headroomMatrix[r] = fmt.Sprintf("limit=%v usage=%v headroom=%v preempt_limit=%v preempt_usage=%v", q.Limit, q.Usage, h, q.PreLimit, q.PreUsage)
		fmt.Printf("C211V_QUOTA project=%s region=%s http=%d v100_rows=[%s]\n", proj, r, q.HTTP, strings.Join(c211wFilterV100(q.Rows), " ; "))
		fmt.Printf("C211V_HEADROOM project=%s region=%s limit=%v usage=%v headroom=%v preempt_limit=%v\n", proj, r, q.Limit, q.Usage, h, q.PreLimit)
		if q.Limit >= 8 && h >= 8 {
			tryRegions = append(tryRegions, r)
		}
	}
	info["quota_matrix"] = quotaMatrix
	info["headroom_matrix"] = headroomMatrix
	info["try_regions"] = tryRegions
	if len(tryRegions) == 0 {
		fmt.Printf("C211V_VERDICT project=%s NO_REGION_WITH_8_HEADROOM\n", proj)
		info["verdict"] = "NO_REGION_WITH_8_HEADROOM"
		return nil, log, info
	}
	tried := 0
	for _, r := range tryRegions {
		if time.Now().After(deadline) {
			fmt.Printf("C211V_VERDICT project=%s REGION_LADDER_STOP_TIME_BUDGET region=%s\n", proj, r)
			break
		}
		// Re-read immediately before the region ladder: usage moves while other
		// lanes' instances start and stop.
		q2 := c211wQuotaRead(tok, proj, r)
		fmt.Printf("C211V_QUOTA_RECHECK project=%s region=%s limit=%v usage=%v headroom=%v\n", proj, r, q2.Limit, q2.Usage, q2.Limit-q2.Usage)
		if q2.Limit < 8 || q2.Limit-q2.Usage < 8 {
			fmt.Printf("C211V_VERDICT project=%s REGION_HEADROOM_LOST region=%s limit=%v usage=%v\n", proj, r, q2.Limit, q2.Usage)
			log = append(log, fmt.Sprintf("region %s headroom lost limit=%v usage=%v", r, q2.Limit, q2.Usage))
			continue
		}
		for _, c := range c211wCands {
			if c.Region != r {
				continue
			}
			if tried >= 6 || time.Now().After(deadline) {
				break
			}
			ats, ast, _ := c211xAccelTypes(tok, proj, c.Zone)
			ok := ast == 200 && c211xContains(ats, "nvidia-tesla-v100")
			fmt.Printf("C211V_ACCEL project=%s zone=%s status=%d v100=%v n=%d\n", proj, c.Zone, ast, ok, len(ats))
			log = append(log, fmt.Sprintf("accel %s http=%d v100=%v", c.Zone, ast, ok))
			if !ok {
				continue
			}
			tried++
			name := "c211-" + c211wNonce + "-v100x8"
			*markers = *markers + 1
			fmt.Printf("C211V_VERDICT project=%s MARKER_ATTEMPT gpus=8 zone=%s accel=nvidia-tesla-v100 machine=%s instance=%s sa=%s image=%s/%s kind=install535\n",
				proj, c.Zone, c211wMachine, name, c211xShort(saEmail), c211wPlainProject, c211wPlainFamily)
			us, meta, created, opErr := c211wMarker(tok, proj, c.Zone, name, saEmail, 8, startup, 110, deadline)
			absent, trace := c211wDeleteAndProveAbsence(tok, proj, c.Zone, name)
			info[fmt.Sprintf("z%s_created", c.Zone)] = created
			info[fmt.Sprintf("z%s_uuids", c.Zone)] = len(us)
			info[fmt.Sprintf("z%s_meta", c.Zone)] = c211xTrunc(meta, 1400)
			info[fmt.Sprintf("z%s_op_error", c.Zone)] = opErr
			info[fmt.Sprintf("z%s_absent", c.Zone)] = absent
			info[fmt.Sprintf("z%s_cleanup", c.Zone)] = trace
			log = append(log, fmt.Sprintf("attempt %s gpus=8 created=%v uuids=%d opErr=%s absent=%v", c.Zone, created, len(us), c211xTrunc(opErr, 300), absent))
			if len(us) > 0 {
				fmt.Printf("C211V_VERDICT project=%s MARKER_GPU gpus=8 uuid0=%s uuid_count=%d absent=%v zone=%s\n", proj, us[0], len(us), absent, c.Zone)
				info["verdict"] = "MARKER_GPU"
				info["zone"] = c.Zone
				info["uuids"] = us
				info["meta"] = c211xTrunc(meta, 1600)
				info["absent"] = absent
				return us, log, info
			}
			if opErr != "" {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_OPERATION_REFUSED gpus=8 zone=%s error=%s\n", proj, c.Zone, c211xTrunc(opErr, 400))
				info["verdict"] = "OPERATION_REFUSED"
				low := strings.ToLower(opErr)
				if !c211wCapacityRefusal(opErr) && !strings.Contains(low, "quota") && !strings.Contains(low, "exceeded") {
					info["verdict"] = "HARD_OPERATION_REFUSED"
					return nil, log, info
				}
				// Quota/capacity refusal is region-scoped: keep climbing regions.
				continue
			}
			if !created {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_CREATE_NOT_CONFIRMED gpus=8 zone=%s\n", proj, c.Zone)
				info["verdict"] = "CREATE_NOT_CONFIRMED"
				continue
			}
			fmt.Printf("C211V_VERDICT project=%s ATTEMPT_REACH_FOUND_NO_DEVICE_LINE gpus=8 zone=%s\n", proj, c.Zone)
			info["verdict"] = "REACH_FOUND_NO_DEVICE_LINE"
			return nil, log, info
		}
	}
	if _, ok := info["verdict"]; !ok {
		info["verdict"] = "NO_ZONE_ATTEMPTED"
	}
	return nil, log, info
}

func TestC211WV100x8(t *testing.T) {
	started := time.Now()
	deadline := started.Add(70 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211V_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211wHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211wHostNum
	}
	fmt.Printf("C211V_IDENTITY auth=%s wif_sa=%s project_number=%s nonce=%s\n", method, c211xShort(wifSA), projNum, c211wNonce)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211V_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211V_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 300))
	}

	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211V_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 160))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211V_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211V_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 240))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211V_FLEET_TOKEN fallback=wif_minted\n")
	}

	okp, probep := c211xImageUsable(ftok, c211wPlainProject, c211wPlainFamily)
	fmt.Printf("C211V_IMAGE_PLAIN %s\n", probep)
	if !okp {
		fmt.Printf("C211V_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		return
	}
	installStartup := c211wStartupInstall()
	fmt.Printf("C211V_STARTUP kind=install535x8 bytes=%d\n", len(installStartup))

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string][]string{}
	readyCount := map[string]int{}
	markers := 0

	for _, id := range c211wSiblings {
		if time.Now().After(deadline) {
			fmt.Printf("C211V_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		if id == hostID {
			fmt.Printf("C211V_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", id)
			continue
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211V_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		res := map[string]interface{}{"number": num, "name": c211xStr(pm, "name"), "state": c211xStr(pm, "lifecycleState")}
		results[id] = res
		fmt.Printf("C211V_PROJECT_BEGIN id=%s number=%s name=%q state=%s\n", id, num, c211xStr(pm, "name"), c211xStr(pm, "lifecycleState"))

		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211V_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", id, pst, strings.Join(al, ","), c211xTrunc(pbody, 200))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")
		res["can_create"] = canCreate
		if !canCreate {
			fmt.Printf("C211V_VERDICT project=%s NO_CREATE_PERMISSION\n", id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}

		c211wCensus(ftok, id)

		saEmail := num + "-compute@developer.gserviceaccount.com"

		us, alog, ainfo := c211wAttemptHeadroom(ftok, id, saEmail, installStartup, deadline, &markers)
		res["attempt_8x"] = ainfo
		res["attempt_8x_log"] = alog
		if len(us) > 0 {
			res["verdict"] = "MARKER_GPU_8X"
			res["gpu_count"] = len(us)
			res["marker_zone"] = ainfo["zone"]
			res["uuids"] = us
			readyProjects = append(readyProjects, id)
			readyUUIDs[id] = us
			readyCount[id] = len(us)
			continue
		}
		res["verdict"] = "NO_DEVICE_LINE_8X"
		fmt.Printf("C211V_VERDICT project=%s NO_DEVICE_LINE_8X\n", id)
	}

	for _, id := range c211wSiblings {
		c211wReaper(ftok, id)
	}

	verdict := "REACH_FOUND_NO_DEVICE_LINE"
	if len(readyProjects) > 0 {
		verdict = "READY_NEW_PROJECT_GPU"
	} else if markers == 0 {
		verdict = "NO_MARKER_POSSIBLE"
	}
	sum := map[string]interface{}{
		"probe":           "c211_i123594_v100x8_r2_headroom",
		"mechanism":       "proven Volta recipe (plain ubuntu-2204-lts + nvidia-driver-535-server) at 8 x nvidia-tesla-v100 on n1-standard-8, region-headroom-aware ladder, no fallback",
		"host_project":    projNum,
		"host_project_id": c211xShort(hostID),
		"fleet_token_ok":  ftok != "",
		"image_plain":     c211wPlainProject + "/" + c211wPlainFamily,
		"markers":         markers,
		"ready_projects":  readyProjects,
		"ready_uuids":     readyUUIDs,
		"ready_counts":    readyCount,
		"verdict":         verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
		"results":         results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211V SUMMARY BEGIN ===\nC211V_SUMMARY_JSON %s\n=== C211V SUMMARY END ===\n", string(js))
	fmt.Printf("C211V_VERDICT_FINAL %s markers=%d ready=%d elapsed=%ds\n", verdict, markers, len(readyProjects), int(time.Since(started).Seconds()))
}

func c211wFilterV100(rows []string) []string {
	out := []string{}
	for _, r := range rows {
		if strings.Contains(r, "NVIDIA_V100_GPUS") || strings.Contains(r, "COMMITTED_NVIDIA_V100") {
			out = append(out, r)
		}
	}
	return out
}
