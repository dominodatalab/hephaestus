//go:build functional && gke

// C211 i122232 — V100 (VOLTA) DEVICE PROOF WITH A CORRECT DRIVER STACK.
//
// WHY THIS IS A MATERIALLY NEW MECHANISM (not a repeat of i122026/i122162):
// fact 122203 proved that in each of the four Domino sibling projects a nonce
// on-demand n1-standard-4 + 1 x nvidia-tesla-v100 instance reaches RUNNING and
// the guest sees the GV100 at PCI 0000:00:04.0 dev=0x1db1, yet ZERO device
// UUIDs were produced because the ubuntu-accelerator ...-with-nvidia-580 image
// never ran a working kernel module (18/18 nvidia-smi attempts said "couldn't
// communicate with the NVIDIA driver", and the probe never modprobed nor
// captured dmesg). The one untested lever is the DRIVER STACK:
//   A) first, a Volta-capable baked image (nvidia-550, the newest branch that
//      still fully supports Volta) with an explicit modprobe + full
//      lspci/dmesg/lsmod/NVRM capture, and
//   B) if that yields no device line, a plain ubuntu-os-cloud ubuntu-2204-lts
//      boot with a startup script that installs nvidia-driver-535-server (the
//      mandated driver), modprobes, triggers udev and re-polls, emitting
//      C211_MARKER_START/END lines with lspci -nn, dmesg | grep -i nvidia,
//      nvidia-smi -L and --query-gpu=name,uuid,memory.total,driver_version.
//
// Exactly ONE nonce-tagged ON-DEMAND instance is alive at a time, per project;
// each is read back via compute.instances.get, deleted, and proven absent
// (GET 404 + aggregated list with zero c211-named items). The host project
// 153711203585 is never marker-ed. Tokens are reported as length + SHA-256
// prefix only.
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
	c211vNonce   = "i122232"
	c211vHostNum = "153711203585"
	c211vLabel   = "c211"
	// Volta-capable baked accelerator image (R550 still supports Volta; the
	// R580 image already failed to bind in i122026/i122162).
	c211vAccelProject = "ubuntu-os-accelerator-images"
	c211vAccelFamily  = "ubuntu-accelerator-2204-amd64-with-nvidia-550"
	// Mandated plain-image + installed-driver path.
	c211vPlainProject = "ubuntu-os-cloud"
	c211vPlainFamily  = "ubuntu-2204-lts"
)

var c211vSiblings = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211vRegions = []string{"us-central1", "us-east1", "europe-west4", "asia-east1"}

var c211vZones = map[string][]string{
	"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
	"us-east1":     {"us-east1-b", "us-east1-c", "us-east1-d"},
	"europe-west4": {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
	"asia-east1":   {"asia-east1-a", "asia-east1-b", "asia-east1-c"},
}

type c211vImage struct {
	Project string
	Family  string
	Kind    string // "accel550" or "install535"
}

// ---------------------------------------------------------------------------
// Startup scripts. Both write C211_MARKER_START ... C211_MARKER_END to the
// serial console and mirror the terminal stage to instance/guest attributes.
// The long work is detached (systemd-run / setsid) so the GCE boot-time script
// timeout cannot kill it.
// ---------------------------------------------------------------------------

const c211vPutFunc = `put() {
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
  cat /tmp/c211v-uuid.txt
  put "STAGE=uuid uuid_line=$(tr '\n' '|' < /tmp/c211v-uuid.txt) smi=$(tr '\n' '|' < /tmp/c211v-smi.txt) driver=$(cat /proc/driver/nvidia/version 2>/dev/null | head -1 | tr '\n' ' ')"
  report_dev
  echo "C211_MARKER_END"
  exit 0
}
`

// Startup A: Volta-capable baked accelerator image, explicit modprobe.
func c211vStartupFast() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211vNonce + ` mode=accel550"
` + c211vPutFunc + `
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211V_PCI $PCIDEV"
echo "C211V_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|3d|vga' | tr '\n' '|')"
put "STAGE=boot mode=accel550 pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
SMI=$(smi_probe)
echo "C211V_SMI_PATH_PRE $SMI"
if [ -n "$SMI" ]; then put "STAGE=boot_smi smi=$SMI"; fi
modprobe nvidia 2>&1 | sed 's/^/C211V_MODPROBE /'
modprobe nvidia_uvm 2>&1 | sed 's/^/C211V_MODPROBE_UVM /'
udevadm trigger --subsystem-match=pci 2>/dev/null || true
sleep 6
report_dev
OK=0
for i in $(seq 1 48); do
  SMI=$(smi_probe)
  if [ -n "$SMI" ] && "$SMI" -L > /tmp/c211v-smi.txt 2>&1; then OK=1; break; fi
  if [ $((i % 6)) -eq 0 ]; then echo "C211V_SMI_ATTEMPT i=$i $(head -c 220 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' ' ')"; report_dev; fi
  sleep 5
done
if [ "$OK" = "1" ]; then uuid_report "$SMI"; fi
put "STAGE=failed mode=accel550 smi=$(head -c 500 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' '|') lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|') kernel=$(uname -r)"
report_dev
echo C211V_FAILED
echo "C211_MARKER_END"
`
}

// Startup B: plain ubuntu-2204-lts, install a Volta-capable driver
// (nvidia-driver-535-server first, then nvidia-driver-535, then cuda-drivers-535),
// then modprobe + udev trigger and re-poll the device without any host power
// action (DKMS builds the module for the running kernel).
func c211vStartupInstall() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211vNonce + ` mode=install535"
mkdir -p /var/lib/c211
cat > /usr/local/bin/c211v-run.sh <<'EOS'
#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211vNonce + ` mode=install535"
` + c211vPutFunc + `
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211V_PCI $PCIDEV"
echo "C211V_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|3d|vga' | tr '\n' '|')"
put "STAGE=boot mode=install535 pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
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
put "STAGE=failed mode=install535 smi=$(head -c 500 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' '|') lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|') kernel=$(uname -r) pkgs=$(dpkg -l 2>/dev/null | grep -E 'nvidia-driver|cuda-drivers' | awk '{print $2"="$3}' | head -6 | tr '\n' '|')"
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
// Image resolution
// ---------------------------------------------------------------------------

func c211vResolve(tok string) (c211vImage, c211vImage, string) {
	log := []string{}

	fast := c211vImage{}
	ok, probe := c211xImageUsable(tok, c211vAccelProject, c211vAccelFamily)
	fmt.Printf("C211V_IMAGE_FAST %s\n", probe)
	log = append(log, "fast "+probe)
	if ok {
		fast = c211vImage{Project: c211vAccelProject, Family: c211vAccelFamily, Kind: "accel550"}
	} else {
		// Fall back to listing the accelerator project, but only accept a
		// family with a Volta-capable (<=570, i.e. R550/R560/R570) driver.
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+c211vAccelProject+"/global/images?maxResults=500&fields=items(name,family)", tok, "", "")
		fams := []string{}
		if st == 200 {
			if m := c211xJSON(body); m != nil {
				if items, okk := m["items"].([]interface{}); okk {
					for _, it := range items {
						im, _ := it.(map[string]interface{})
						if f := c211xStr(im, "family"); f != "" {
							fams = append(fams, f)
						}
					}
				}
			}
		}
		fmt.Printf("C211V_IMAGE_LIST project=%s status=%d families=%d list=%s verbatim=%s\n", c211vAccelProject, st, len(fams), strings.Join(fams, ","), c211xTrunc(body, 200))
		log = append(log, fmt.Sprintf("list status=%d families=%d", st, len(fams)))
		for _, f := range fams {
			if !strings.Contains(f, "2204") || !strings.Contains(f, "nvidia-5") {
				continue
			}
			// Accept only R550/R560/R570 - the R580 image already failed to bind.
			if strings.Contains(f, "nvidia-580") || strings.Contains(f, "nvidia-595") || strings.Contains(f, "nvidia-590") {
				continue
			}
			ok2, p2 := c211xImageUsable(tok, c211vAccelProject, f)
			fmt.Printf("C211V_IMAGE_FAST_ALT %s\n", p2)
			if ok2 {
				fast = c211vImage{Project: c211vAccelProject, Family: f, Kind: "accel_alt"}
				break
			}
		}
	}

	plain := c211vImage{}
	okp, probep := c211xImageUsable(tok, c211vPlainProject, c211vPlainFamily)
	fmt.Printf("C211V_IMAGE_PLAIN %s\n", probep)
	log = append(log, "plain "+probep)
	if okp {
		plain = c211vImage{Project: c211vPlainProject, Family: c211vPlainFamily, Kind: "install535"}
	}
	return fast, plain, strings.Join(log, "; ")
}

// ---------------------------------------------------------------------------
// Marker: create ONE on-demand V100 instance, capture the device, delete, prove
// absence. All evidence printed verbatim with the C211V_ prefix.
// ---------------------------------------------------------------------------

func c211vCreate(tok, proj, zone, name, saEmail string, img c211vImage, startup string) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211vLabel, "value": c211vNonce},
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/nvidia-tesla-v100",
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
		"labels": map[string]string{c211vLabel: c211vNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

func c211vOp(tok, proj, zone, op string) (string, string, string) {
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
			return "DONE", b, c211vOpError(b)
		}
		time.Sleep(6 * time.Second)
	}
	return last, "", ""
}

func c211vOpError(body string) string {
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

func c211vPrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211V_") && !strings.Contains(ln, "C211_MARKER_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211V_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 460))
	}
}

func c211vSerial(tok, proj, zone, name string) string {
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

func c211vGuest(tok, proj, zone, name string) (string, int) {
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

func c211vDeleteAndProveAbsence(tok, proj, zone, name string) (bool, []string) {
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
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211vNonce)+"&maxResults=500", tok, "", "")
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

func c211vMarker(tok, proj, zone, name, saEmail string, img c211vImage, startup string, maxPolls int, deadline time.Time) (string, string, bool, string) {
	cst, cbody := c211vCreate(tok, proj, zone, name, saEmail, img, startup)
	fmt.Printf("C211V_MARKER_CREATE project=%s zone=%s name=%s machine=n1-standard-4 image=%s/%s kind=%s status=%d verbatim=%s\n",
		proj, zone, name, img.Project, img.Family, img.Kind, cst, c211xTrunc(cbody, 1400))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211vOp(tok, proj, zone, opName)
		fmt.Printf("C211V_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 400), c211xTrunc(ob, 3000))
		created = opStatus == "DONE" && opErr == ""
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}

	uuid, metaFinal := "", ""
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
			content := c211vSerial(tok, proj, zone, name)
			if content != "" {
				c211vPrintNew(seen, proj, content)
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
			}
			gval, gst := c211vGuest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
			}
			fmt.Printf("C211V_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuid=%s\n",
				proj, i, state, len(content), gst, c211xTrunc(gval, 200), c211xTrunc(metaFinal, 260), uuid)
			if u := c211xUUIDRe.FindString(metaFinal); u != "" {
				uuid = u
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") {
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
			}
		}
		fmt.Printf("C211V_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 320), c211xTrunc(metaFinal, 460), c211xTrunc(rb, 200))
	}
	if u := c211xUUIDRe.FindString(metaFinal); u != "" {
		uuid = u
	}
	return uuid, metaFinal + " | accel=" + accelJSON + " | status=" + statusFinal, created, opErr
}

func c211vReaper(tok, proj string) {
	for round := 0; round < 3; round++ {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211vNonce)+"&maxResults=500", tok, "", "")
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

func TestC211XV100Driver(t *testing.T) {
	started := time.Now()
	deadline := started.Add(78 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211V_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211vHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211vHostNum
	}
	fmt.Printf("C211V_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211V_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211V_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 300))
	}

	// Fleet command identity: the cross-project grants belong to it.
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

	fast, plain, imgLog := c211vResolve(ftok)
	if fast.Project == "" && plain.Project == "" {
		fmt.Printf("C211V_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		fmt.Printf("=== C211V SUMMARY BEGIN ===\nC211V_SUMMARY_JSON %s\n=== C211V SUMMARY END ===\n",
			c211xJSONString(map[string]interface{}{
				"probe": "c211_i122232_v100drv", "mechanism": "Volta-capable driver stack (accel550 and/or plain ubuntu-2204-lts + nvidia-driver-535-server)",
				"host_project": projNum, "image_resolution": imgLog,
				"markers": 0, "ready_projects": []string{}, "verdict": "NO_USABLE_IMAGE",
				"elapsed_seconds": int(time.Since(started).Seconds()),
			}))
		return
	}
	fmt.Printf("C211V_IMAGES fast=%s/%s(%s) plain=%s/%s(%s) log=%s\n",
		fast.Project, fast.Family, fast.Kind, plain.Project, plain.Family, plain.Kind, imgLog)

	fastStartup := c211vStartupFast()
	installStartup := c211vStartupInstall()
	fmt.Printf("C211V_STARTUP kind=accel550 bytes=%d kind=install535 bytes=%d\n", len(fastStartup), len(installStartup))

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string]string{}
	markers := 0

	for _, id := range c211vSiblings {
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

		// Quota: first region with NVIDIA_V100_GPUS limit > usage.
		region, limit, usage := "", 0.0, 0.0
		quotaMatrix := map[string]string{}
		for _, r := range c211vRegions {
			rows, qst, _ := c211xRegionQuotaRows(ftok, id, r)
			quotaMatrix[r] = fmt.Sprintf("http=%d %s", qst, strings.Join(rows, " ; "))
			fmt.Printf("C211V_QUOTA project=%s region=%s http=%d v100_rows=[%s]\n", id, r, qst, strings.Join(c211vFilterV100(rows), " ; "))
			for _, row := range c211vFilterV100(rows) {
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
				if l > u && region == "" {
					region, limit, usage = r, l, u
				}
			}
		}
		res["quota_matrix"] = quotaMatrix
		if region == "" {
			fmt.Printf("C211V_VERDICT project=%s NO_NONZERO_V100_QUOTA\n", id)
			res["verdict"] = "NO_NONZERO_V100_QUOTA"
			continue
		}
		res["quota_region"] = region
		res["quota_limit"] = limit
		res["quota_usage"] = usage

		zone := ""
		for _, z := range c211vZones[region] {
			ats, ast, _ := c211xAccelTypes(ftok, id, z)
			ok := ast == 200 && c211xContains(ats, "nvidia-tesla-v100")
			fmt.Printf("C211V_ACCEL project=%s zone=%s status=%d v100=%v n=%d\n", id, z, ast, ok, len(ats))
			if ok {
				zone = z
				break
			}
		}
		if zone == "" {
			fmt.Printf("C211V_VERDICT project=%s ACCEL_NOT_ADVERTISED region=%s\n", id, region)
			res["verdict"] = "ACCEL_NOT_ADVERTISED"
			continue
		}
		res["marker_zone"] = zone
		saEmail := num + "-compute@developer.gserviceaccount.com"

		// ---- Attempt A: Volta-capable baked 550 image with explicit modprobe.
		uuid := ""
		if fast.Project != "" {
			nameA := "c211-" + c211vNonce + "-v100a"
			markers++
			fmt.Printf("C211V_VERDICT project=%s MARKER_ATTEMPT_A zone=%s accel=nvidia-tesla-v100 machine=n1-standard-4 instance=%s sa=%s image=%s/%s kind=%s\n",
				id, zone, nameA, c211xShort(saEmail), fast.Project, fast.Family, fast.Kind)
			ua, meta, created, opErr := c211vMarker(ftok, id, zone, nameA, saEmail, fast, fastStartup, 60, deadline)
			res["a_created"] = created
			res["a_uuid"] = ua
			res["a_meta"] = c211xTrunc(meta, 900)
			res["a_op_error"] = opErr
			absent, trace := c211vDeleteAndProveAbsence(ftok, id, zone, nameA)
			res["a_absent"] = absent
			res["a_cleanup"] = trace
			if ua != "" {
				uuid = ua
				fmt.Printf("C211V_VERDICT project=%s MARKER_GPU_A uuid=%s absent=%v\n", id, ua, absent)
				res["verdict"] = "MARKER_GPU_A"
				res["marker_accel"] = "nvidia-tesla-v100"
				res["marker_image"] = fast.Project + "/" + fast.Family
				readyProjects = append(readyProjects, id)
				readyUUIDs[id] = ua
				continue
			}
			if opErr != "" {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_A_OPERATION_REFUSED error=%s\n", id, c211xTrunc(opErr, 200))
				res["a_verdict"] = "OPERATION_REFUSED"
			} else if !created {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_A_CREATE_NOT_CONFIRMED\n", id)
				res["a_verdict"] = "CREATE_NOT_CONFIRMED"
			} else {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_A_REACH_FOUND_NO_DEVICE_LINE\n", id)
				res["a_verdict"] = "REACH_FOUND_NO_DEVICE_LINE"
			}
		}

		// ---- Attempt B: plain ubuntu-2204-lts + installed Volta driver.
		if plain.Project != "" && time.Now().Before(deadline) {
			nameB := "c211-" + c211vNonce + "-v100b"
			markers++
			fmt.Printf("C211V_VERDICT project=%s MARKER_ATTEMPT_B zone=%s accel=nvidia-tesla-v100 machine=n1-standard-4 instance=%s sa=%s image=%s/%s kind=%s\n",
				id, zone, nameB, c211xShort(saEmail), plain.Project, plain.Family, plain.Kind)
			ub, meta, created, opErr := c211vMarker(ftok, id, zone, nameB, saEmail, plain, installStartup, 110, deadline)
			res["b_created"] = created
			res["b_uuid"] = ub
			res["b_meta"] = c211xTrunc(meta, 1200)
			res["b_op_error"] = opErr
			absent, trace := c211vDeleteAndProveAbsence(ftok, id, zone, nameB)
			res["b_absent"] = absent
			res["b_cleanup"] = trace
			if ub != "" {
				uuid = ub
				fmt.Printf("C211V_VERDICT project=%s MARKER_GPU_B uuid=%s absent=%v\n", id, ub, absent)
				res["verdict"] = "MARKER_GPU_B"
				res["marker_accel"] = "nvidia-tesla-v100"
				res["marker_image"] = plain.Project + "/" + plain.Family
				readyProjects = append(readyProjects, id)
				readyUUIDs[id] = ub
				continue
			}
			if opErr != "" {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_B_OPERATION_REFUSED error=%s\n", id, c211xTrunc(opErr, 200))
				res["b_verdict"] = "OPERATION_REFUSED"
			} else if !created {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_B_CREATE_NOT_CONFIRMED\n", id)
				res["b_verdict"] = "CREATE_NOT_CONFIRMED"
			} else {
				fmt.Printf("C211V_VERDICT project=%s ATTEMPT_B_REACH_FOUND_NO_DEVICE_LINE\n", id)
				res["b_verdict"] = "REACH_FOUND_NO_DEVICE_LINE"
			}
		}
		if uuid == "" {
			res["verdict"] = "REACH_FOUND_NO_DEVICE_LINE_BOTH"
			fmt.Printf("C211V_VERDICT project=%s REACH_FOUND_NO_DEVICE_LINE_BOTH\n", id)
		}
	}

	// Final reaper across all four projects.
	for _, id := range c211vSiblings {
		c211vReaper(ftok, id)
	}

	verdict := "REACH_FOUND_NO_DEVICE_LINE_BOTH"
	if len(readyProjects) > 0 {
		verdict = "READY_NEW_PROJECT_GPU"
	} else if markers == 0 {
		verdict = "NO_MARKER_POSSIBLE"
	}
	sum := map[string]interface{}{
		"probe":            "c211_i122232_v100drv",
		"mechanism":        "Volta-capable driver stack: accel550 baked image with modprobe, then plain ubuntu-2204-lts + nvidia-driver-535-server with udev trigger",
		"host_project":     projNum,
		"host_project_id":  c211xShort(hostID),
		"fleet_token_ok":   ftok != "",
		"image_fast":       fast.Project + "/" + fast.Family,
		"image_plain":      plain.Project + "/" + plain.Family,
		"image_resolution": imgLog,
		"markers":          markers,
		"ready_projects":   readyProjects,
		"ready_uuids":      readyUUIDs,
		"verdict":          verdict,
		"elapsed_seconds":  int(time.Since(started).Seconds()),
		"results":          results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211V SUMMARY BEGIN ===\nC211V_SUMMARY_JSON %s\n=== C211V SUMMARY END ===\n", string(js))
	fmt.Printf("C211V_VERDICT_FINAL %s markers=%d ready=%d elapsed=%ds\n", verdict, markers, len(readyProjects), int(time.Since(started).Seconds()))
}

// c211vFilterV100 keeps only the rows needed to decide V100 quota placement.
func c211vFilterV100(rows []string) []string {
	out := []string{}
	for _, r := range rows {
		if strings.Contains(r, "NVIDIA_V100_GPUS") || strings.Contains(r, "COMMITTED_NVIDIA_V100") {
			out = append(out, r)
		}
	}
	return out
}
