//go:build functional && gke

// C211 i123641 — OPEN THE UNEXPLORED NVIDIA L4 DENSITY CELL IN THE FOUR PROVEN
// DOMINO SIBLING PROJECTS.
//
// Fact 121979 measured NVIDIA_L4_GPUS limit=16 usage=0 in every queried region
// of team-eng-infra-testing (210628133505), domino-eng-service-artifacts
// (1072394045651), team-eng-infrastructure (463640210929) and team-eng-platform
// (1074386228880); fact 122072 proves neither handoff tree holds any
// gcp-wif-hephaestus-sibgpu A100/L4 row and the only L4 marker ever made was the
// host-project 1x x122497; fact 123406 proves the dominodatalab/hephaestus
// '/functional-test' GKE WIF vehicle end-to-end.
//
// Per project: re-read the per-region NVIDIA_L4_GPUS limit/usage, then walk the
// mandated shape ladder g2-standard-48 (4x L4) -> g2-standard-96 (8x L4) ->
// g2-standard-4 (1x L4). A shape is eligible only when the zone offers BOTH the
// g2 machine type AND nvidia-tesla-l4 AND the measured per-region L4 headroom
// (limit - usage) covers the GPU count. EXACTLY ONE nonce-tagged ON-DEMAND
// instance is created per project on the first eligible shape/zone, booted from
// the proven plain ubuntu-os-cloud/ubuntu-2204-lts + startup apt-get install
// nvidia-driver-535-server recipe with serial-console and guest-attribute
// capture of lspci -nn / nvidia-smi -L / --query-gpu. An independent
// compute.instances.get readback records status + acceleratorCount, then the
// instance is DELETEd and absence is proven (GET 404 + aggregated list with zero
// c211-named items) before the next project. Strictly one own instance alive at
// any moment; tokens are reported as length + SHA-256 prefix only.
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
	c211wNonce        = "i123748"
	c211wHostNum      = "153711203585"
	c211wLabel        = "c211"
	c211wPlainProject = "ubuntu-os-cloud"
	c211wPlainFamily  = "ubuntu-2204-lts"
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

// Preference order for the L4 zones: us-central1 first (where every prior
// sibling marker succeeded), then europe-west4, us-east1 and asia-east1.
var c211wCands = []c211wZoneCand{
	{"us-central1", "us-central1-a"},
	{"us-central1", "us-central1-b"},
	{"us-central1", "us-central1-c"},
	{"us-central1", "us-central1-f"},
	{"europe-west4", "europe-west4-b"},
	{"europe-west4", "europe-west4-a"},
	{"europe-west4", "europe-west4-c"},
	{"us-east1", "us-east1-b"},
	{"us-east1", "us-east1-c"},
	{"us-east1", "us-east1-d"},
	{"asia-east1", "asia-east1-b"},
	{"asia-east1", "asia-east1-c"},
	{"asia-east1", "asia-east1-a"},
}

// Mandated L4 shape ladder, largest first.
type c211wShape struct {
	Machine string
	GPUs    int
	Label   string
}

var c211wShapes = []c211wShape{
	{"g2-standard-48", 4, "4x"},
	{"g2-standard-96", 8, "8x"},
	{"g2-standard-4", 1, "1x"},
}

// ---------------------------------------------------------------------------
// Startup script: the exact proven recipe (plain ubuntu-2204-lts, install
// nvidia-driver-535-server, modprobe, udev trigger, re-poll) with full
// multi-GPU capture. Long work is detached so the GCE boot-script timeout
// cannot kill it. Mirrors the terminal stage to instance/guest attributes as
// well as the serial console.
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
echo "C211_MARKER_START nonce=` + c211wNonce + ` mode=install535-l4"
mkdir -p /var/lib/c211
cat > /usr/local/bin/c211v-run.sh <<'EOS'
#!/bin/bash
exec >/dev/console 2>&1
echo "C211_MARKER_START nonce=` + c211wNonce + ` mode=install535-l4"
` + c211wPutFunc + `
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo "C211V_PCI $PCIDEV"
echo "C211V_LSPCI $(lspci -nn 2>/dev/null | grep -i -E 'nvidia|3d|vga' | tr '\n' '|')"
put "STAGE=boot mode=install535-l4 pci=$PCIDEV host=$(hostname) kernel=$(uname -r)"
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
put "STAGE=failed mode=install535-l4 smi=$(head -c 500 /tmp/c211v-smi.txt 2>/dev/null | tr '\n' '|') lsmod=$(lsmod 2>/dev/null | grep -i nvidia | tr '\n' '|') kernel=$(uname -r) pkgs=$(dpkg -l 2>/dev/null | grep -E 'nvidia-driver|cuda-drivers' | awk '{print $2"="$3}' | head -6 | tr '\n' '|')"
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
// L4 helpers
// ---------------------------------------------------------------------------

// c211l4QuotaOne returns every NVIDIA row for the region plus the CPUS
// limit/usage (for headroom context).
func c211l4QuotaOne(token, proj, region string) ([]string, float64, float64, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + region
	st, body := c211xHTTP("GET", u, token, "", "")
	rows := []string{}
	cpuLimit, cpuUsage := 0.0, 0.0
	if st != 200 {
		return rows, cpuLimit, cpuUsage, st, body
	}
	m := c211xJSON(body)
	quotas, _ := m["quotas"].([]interface{})
	for _, q := range quotas {
		qm, _ := q.(map[string]interface{})
		metric := c211xStr(qm, "metric")
		limit, _ := qm["limit"].(float64)
		usage, _ := qm["usage"].(float64)
		if metric == "CPUS" {
			cpuLimit, cpuUsage = limit, usage
			continue
		}
		if !strings.Contains(metric, "NVIDIA") || limit <= 0 {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s %s limit=%v usage=%v", region, metric, limit, usage))
	}
	return rows, cpuLimit, cpuUsage, st, body
}

func c211l4MachineType(token, proj, zone, mt string) (int, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/machineTypes/" + url.PathEscape(mt) + "?fields=name,guestCpus,memoryMb"
	st, body := c211xHTTP("GET", u, token, "", "")
	cpus := 0
	if st == 200 {
		if m := c211xJSON(body); m != nil {
			if v, ok := m["guestCpus"].(float64); ok {
				cpus = int(v)
			}
		}
	}
	return st, cpus, body
}

// c211l4AccelPick chooses the real Compute API accelerator-type name for an
// L4 GPU. GCP publishes L4 as "nvidia-l4" (the "nvidia-tesla-l4" spelling does
// not appear in any of the probed zone lists); the legacy spelling is kept as
// a fallback only.
func c211l4AccelPick(ats []string) string {
	for _, want := range []string{"nvidia-l4", "nvidia-tesla-l4"} {
		for _, a := range ats {
			if a == want {
				return want
			}
		}
	}
	return ""
}

// c211l4Residue is the valid-filter variant of the shared residue probe
// ("labels.c211" alone is rejected verbatim 400 Invalid list filter
// expression; "labels.c211=<nonce>" is accepted).
func c211l4Residue(tok, proj string) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211wNonce)+"&maxResults=500", tok, "", "")
	fmt.Printf("C211L4_RESIDUE project=%s status=%d verbatim=%s\n", proj, st, c211xTrunc(body, 700))
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
			if n == "" {
				continue
			}
			ds, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+n, tok, "", "")
			fmt.Printf("C211L4_RESIDUE_DELETE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, zone, n, ds, c211xTrunc(db, 280))
		}
	}
}

// c211l4Create creates ONE on-demand instance with the chosen accelerator name.
func c211l4Create(tok, proj, zone, name, saEmail, machine, accel string, count int, startup string) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211wLabel, "value": c211wNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + machine,
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
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

func c211l4Op(tok, proj, zone, op string) (string, string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 80; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211L4_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		if stt != last {
			fmt.Printf("C211L4_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, m["progress"])
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b, c211wOpError(b)
		}
		time.Sleep(6 * time.Second)
	}
	return last, "", ""
}

func c211l4PrintNew(seen map[string]bool, proj, content string) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.Contains(ln, "C211V_") && !strings.Contains(ln, "C211_MARKER_") {
			continue
		}
		if seen[ln] {
			continue
		}
		seen[ln] = true
		fmt.Printf("C211L4_SERIAL_LINE project=%s line=%s\n", proj, c211xTrunc(ln, 1200))
	}
}

func c211l4Serial(tok, proj, zone, name string) string {
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

func c211l4Guest(tok, proj, zone, name string) (string, int) {
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

var c211l4UUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func c211l4AllUUIDs(s string, out map[string]bool) {
	for _, m := range c211l4UUIDRe.FindAllString(s, -1) {
		out[m] = true
	}
}

// c211l4Marker creates ONE on-demand instance of the given shape, captures the
// devices, deletes it and proves absence.
func c211l4Marker(tok, proj, zone, name, saEmail, machine, accel string, count int, startup string, maxPolls int, deadline time.Time) ([]string, string, bool, string) {
	cst, cbody := c211l4Create(tok, proj, zone, name, saEmail, machine, accel, count, startup)
	fmt.Printf("C211L4_MARKER_CREATE project=%s zone=%s name=%s machine=%s gpus=%d accel=%s image=%s/%s status=%d verbatim=%s\n",
		proj, zone, name, machine, count, accel, c211wPlainProject, c211wPlainFamily, cst, c211xTrunc(cbody, 1400))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211l4Op(tok, proj, zone, opName)
		fmt.Printf("C211L4_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 600), c211xTrunc(ob, 2500))
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
				fmt.Printf("C211L4_CAPTURE project=%s STOP_TIME_BUDGET poll=%d\n", proj, i)
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
			content := c211l4Serial(tok, proj, zone, name)
			if content != "" {
				c211l4PrintNew(seen, proj, content)
				c211l4AllUUIDs(content, uuids)
			}
			gval, gst := c211l4Guest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
				c211l4AllUUIDs(gval, uuids)
			}
			fmt.Printf("C211L4_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuids=%d\n",
				proj, i, state, len(content), gst, c211xTrunc(gval, 260), c211xTrunc(metaFinal, 320), len(uuids))
			c211l4AllUUIDs(metaFinal, uuids)
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

	accelJSON, statusFinal := "", ""
	if rst, rm, rb := c211xInstanceGet(tok, proj, zone, name); rst >= 0 {
		if rm != nil {
			statusFinal = c211xStr(rm, "status")
			if ga, ok := rm["guestAccelerators"].([]interface{}); ok {
				accelJSON = c211xJSONString(ga)
			}
			if v := c211xMetaValue(rm, "c211v-result"); v != "" {
				metaFinal = v
				c211l4AllUUIDs(v, uuids)
			}
		}
		fmt.Printf("C211L4_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 700), c211xTrunc(metaFinal, 700), c211xTrunc(rb, 200))
	}
	ulist := []string{}
	for u := range uuids {
		ulist = append(ulist, u)
	}
	return ulist, metaFinal + " | accel=" + accelJSON + " | status=" + statusFinal, created, opErr
}

// ---------------------------------------------------------------------------
// Main test
// ---------------------------------------------------------------------------

func TestC211L4(t *testing.T) {
	started := time.Now()
	deadline := started.Add(80 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211L4_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
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
	fmt.Printf("C211L4_IDENTITY auth=%s wif_sa=%s project_number=%s nonce=%s\n", method, c211xShort(wifSA), projNum, c211wNonce)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211L4_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))
	} else {
		fmt.Printf("C211L4_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 300))
	}

	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211L4_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 160))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211L4_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211L4_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 240))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211L4_FLEET_TOKEN fallback=wif_minted\n")
	}

	okp, probep := c211xImageUsable(ftok, c211wPlainProject, c211wPlainFamily)
	fmt.Printf("C211L4_IMAGE_PLAIN %s\n", probep)
	if !okp {
		fmt.Printf("C211L4_VERDICT_FINAL NO_USABLE_IMAGE markers=0 ready=0 elapsed=%ds\n", int(time.Since(started).Seconds()))
		return
	}
	installStartup := c211wStartupInstall()
	fmt.Printf("C211L4_STARTUP kind=install535-l4 bytes=%d\n", len(installStartup))

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string][]string{}
	readyCount := map[string]int{}
	readyShape := map[string]string{}
	markers := 0

	for _, id := range c211wSiblings {
		if time.Now().After(deadline) {
			fmt.Printf("C211L4_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		if id == hostID {
			fmt.Printf("C211L4_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", id)
			continue
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211L4_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		res := map[string]interface{}{"number": num, "name": c211xStr(pm, "name"), "state": c211xStr(pm, "lifecycleState")}
		results[id] = res
		fmt.Printf("C211L4_PROJECT_BEGIN id=%s number=%s name=%q state=%s\n", id, num, c211xStr(pm, "name"), c211xStr(pm, "lifecycleState"))

		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211L4_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", id, pst, strings.Join(al, ","), c211xTrunc(pbody, 200))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")
		res["can_create"] = canCreate
		if !canCreate {
			fmt.Printf("C211L4_VERDICT project=%s NO_CREATE_PERMISSION\n", id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}

		// Quota: per-region NVIDIA_L4_GPUS limit/usage + CPUS context.
		regionLimit := map[string]float64{}
		regionUsage := map[string]float64{}
		cpuMatrix := map[string]string{}
		quotaMatrix := map[string]string{}
		for _, r := range c211wRegions {
			rows, cl, cu, qst, _ := c211l4QuotaOne(ftok, id, r)
			l4rows := []string{}
			for _, row := range rows {
				if strings.Contains(row, " NVIDIA_L4_GPUS ") {
					l4rows = append(l4rows, row)
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
					regionLimit[r] = l
					regionUsage[r] = u
				}
			}
			quotaMatrix[r] = fmt.Sprintf("http=%d %s", qst, strings.Join(rows, " ; "))
			cpuMatrix[r] = fmt.Sprintf("CPUS limit=%v usage=%v", cl, cu)
			fmt.Printf("C211L4_QUOTA project=%s region=%s http=%d l4_rows=[%s] cpus=%s\n", id, r, qst, strings.Join(l4rows, " ; "), cpuMatrix[r])
			for _, row := range rows {
				if strings.Contains(row, "NVIDIA_L4_GPUS") || strings.Contains(row, "COMMITTED_NVIDIA_L4") || strings.Contains(row, "PREEMPTIBLE_NVIDIA_L4") {
					fmt.Printf("C211L4_QUOTA_ROW project=%s row=%s\n", id, row)
				}
			}
		}
		res["quota_matrix"] = quotaMatrix
		res["cpu_matrix"] = cpuMatrix
		res["region_limit"] = regionLimit
		res["region_usage"] = regionUsage

		c211wResidue(ftok, id)

		saEmail := num + "-compute@developer.gserviceaccount.com"

		// Attempt one shape across eligible zones; stop on first device proof
		// or hard refusal.
		attempt := func(sh c211wShape, maxZones int) ([]string, []string, map[string]interface{}) {
			log := []string{}
			info := map[string]interface{}{}
			cands := []c211wZoneCand{}
			for _, c := range c211wCands {
				if regionLimit[c.Region]-regionUsage[c.Region] >= float64(sh.GPUs) {
					cands = append(cands, c)
				}
			}
			info["candidates"] = len(cands)
			info["machine"] = sh.Machine
			info["gpus"] = sh.GPUs
			if len(cands) == 0 {
				fmt.Printf("C211L4_VERDICT project=%s NO_ZONE_WITH_HEADROOM_GE_%d machine=%s\n", id, sh.GPUs, sh.Machine)
				info["verdict"] = "NO_ZONE_WITH_HEADROOM"
				return nil, log, info
			}
			tried := 0
			for _, c := range cands {
				if tried >= maxZones || time.Now().After(deadline) {
					break
				}
				mst, mcpus, mbody := c211l4MachineType(ftok, id, c.Zone, sh.Machine)
				fmt.Printf("C211L4_MACHINE project=%s zone=%s machine=%s status=%d guest_cpus=%d verbatim=%s\n",
					id, c.Zone, sh.Machine, mst, mcpus, c211xTrunc(mbody, 200))
				log = append(log, fmt.Sprintf("machine %s %s http=%d cpus=%d", c.Zone, sh.Machine, mst, mcpus))
				if mst != 200 {
					continue
				}
				ats, ast, _ := c211xAccelTypes(ftok, id, c.Zone)
				accel := ""
				if ast == 200 {
					accel = c211l4AccelPick(ats)
				}
				ok := ast == 200 && accel != ""
				fmt.Printf("C211L4_ACCEL project=%s zone=%s status=%d l4=%v accel=%s n=%d\n", id, c.Zone, ast, ok, accel, len(ats))
				log = append(log, fmt.Sprintf("accel %s http=%d l4=%v accel=%s", c.Zone, ast, ok, accel))
				if !ok {
					continue
				}
				tried++
				name := "c211-" + c211wNonce + "-l4x" + strconv.Itoa(sh.GPUs)
				markers++
				fmt.Printf("C211L4_VERDICT project=%s MARKER_ATTEMPT gpus=%d zone=%s accel=%s machine=%s instance=%s sa=%s image=%s/%s kind=install535-l4\n",
					id, sh.GPUs, c.Zone, accel, sh.Machine, name, c211xShort(saEmail), c211wPlainProject, c211wPlainFamily)
				us, meta, created, opErr := c211l4Marker(ftok, id, c.Zone, name, saEmail, sh.Machine, accel, sh.GPUs, installStartup, 110, deadline)
				absent, trace := c211wDeleteAndProveAbsence(ftok, id, c.Zone, name)
				info[fmt.Sprintf("z%s_created", c.Zone)] = created
				info[fmt.Sprintf("z%s_uuids", c.Zone)] = len(us)
				info[fmt.Sprintf("z%s_meta", c.Zone)] = c211xTrunc(meta, 1400)
				info[fmt.Sprintf("z%s_op_error", c.Zone)] = opErr
				info[fmt.Sprintf("z%s_absent", c.Zone)] = absent
				info[fmt.Sprintf("z%s_cleanup", c.Zone)] = trace
				info[fmt.Sprintf("z%s_machine", c.Zone)] = sh.Machine
				log = append(log, fmt.Sprintf("attempt %s %s gpus=%d created=%v uuids=%d opErr=%s absent=%v", c.Zone, sh.Machine, sh.GPUs, created, len(us), c211xTrunc(opErr, 300), absent))
				if len(us) > 0 {
					fmt.Printf("C211L4_VERDICT project=%s MARKER_GPU gpus=%d uuid0=%s uuid_count=%d absent=%v zone=%s machine=%s\n", id, sh.GPUs, us[0], len(us), absent, c.Zone, sh.Machine)
					info["verdict"] = "MARKER_GPU"
					info["zone"] = c.Zone
					info["uuids"] = us
					info["meta"] = c211xTrunc(meta, 1600)
					info["absent"] = absent
					return us, log, info
				}
				if opErr != "" {
					fmt.Printf("C211L4_VERDICT project=%s ATTEMPT_OPERATION_REFUSED gpus=%d zone=%s machine=%s error=%s\n", id, sh.GPUs, c.Zone, sh.Machine, c211xTrunc(opErr, 400))
					info["verdict"] = "OPERATION_REFUSED"
					if c211wCapacityRefusal(opErr) {
						continue
					}
					// Hard refusal (bad machine/accelerator shape, quota 0,
					// permission): further zones will not help at this shape.
					return nil, log, info
				}
				if !created {
					fmt.Printf("C211L4_VERDICT project=%s ATTEMPT_CREATE_NOT_CONFIRMED gpus=%d zone=%s machine=%s\n", id, sh.GPUs, c.Zone, sh.Machine)
					info["verdict"] = "CREATE_NOT_CONFIRMED"
					continue
				}
				fmt.Printf("C211L4_VERDICT project=%s ATTEMPT_REACH_FOUND_NO_DEVICE_LINE gpus=%d zone=%s machine=%s\n", id, sh.GPUs, c.Zone, sh.Machine)
				info["verdict"] = "REACH_FOUND_NO_DEVICE_LINE"
				return nil, log, info
			}
			if _, ok := info["verdict"]; !ok {
				info["verdict"] = "NO_ZONE_ATTEMPTED"
			}
			return nil, log, info
		}

		got := false
		for _, sh := range c211wShapes {
			if time.Now().After(deadline) {
				break
			}
			us, slog, sinfo := attempt(sh, 4)
			res["attempt_"+sh.Label] = sinfo
			res["attempt_"+sh.Label+"_log"] = slog
			if len(us) > 0 {
				res["verdict"] = "MARKER_GPU_" + strings.ToUpper(sh.Label)
				res["gpu_count"] = sh.GPUs
				res["machine"] = sh.Machine
				res["marker_zone"] = sinfo["zone"]
				res["uuids"] = us
				readyProjects = append(readyProjects, id)
				readyUUIDs[id] = us
				readyCount[id] = sh.GPUs
				readyShape[id] = sh.Machine
				got = true
				break
			}
			v := fmt.Sprint(sinfo["verdict"])
			// A hard provider refusal at this shape only allows the smaller
			// mandated fallback shapes; a device-line-less reach is terminal.
			if v == "REACH_FOUND_NO_DEVICE_LINE" {
				break
			}
		}
		if !got {
			if _, ok := res["verdict"]; !ok {
				res["verdict"] = "NO_DEVICE_LINE"
				fmt.Printf("C211L4_VERDICT project=%s NO_DEVICE_LINE_LADDER\n", id)
			}
		}
	}

	for _, id := range c211wSiblings {
		c211wReaper(ftok, id)
	}

	verdict := "REACH_FOUND_NO_DEVICE_LINE"
	if len(readyProjects) > 0 {
		verdict = "READY_L4_DEVICE_PROVEN"
	} else if markers == 0 {
		verdict = "NO_MARKER_POSSIBLE"
	}
	sum := map[string]interface{}{
		"probe":           "c211_i123641_l4",
		"mechanism":       "proven plain ubuntu-2204-lts + nvidia-driver-535-server recipe at 4x g2-standard-48 / 8x g2-standard-96 / 1x g2-standard-4 nvidia-l4",
		"host_project":    projNum,
		"host_project_id": c211xShort(hostID),
		"fleet_token_ok":  ftok != "",
		"image_plain":     c211wPlainProject + "/" + c211wPlainFamily,
		"markers":         markers,
		"ready_projects":  readyProjects,
		"ready_uuids":     readyUUIDs,
		"ready_counts":    readyCount,
		"ready_shapes":    readyShape,
		"verdict":         verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
		"results":         results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211L4 SUMMARY BEGIN ===\nC211L4_SUMMARY_JSON %s\n=== C211L4 SUMMARY END ===\n", string(js))
	fmt.Printf("C211L4_VERDICT_FINAL %s markers=%d ready=%d elapsed=%ds\n", verdict, markers, len(readyProjects), int(time.Since(started).Seconds()))
}


// ---------------------------------------------------------------------------
// Shared teardown / residue helpers (verbatim proven logic from i122466)
// ---------------------------------------------------------------------------

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
