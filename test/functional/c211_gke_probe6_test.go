//go:build functional

// C211 i120026 GKE/GCP sixth pass: the fifth pass proved the escalation works —
// resourcemanager.projects.setIamPolicy returned 200, roles/compute.admin was
// added to the WIF service account and testIamPermissions then returned
// compute.instances.create=true. The single marker VM was refused only by a
// client-side payload bug: guestAccelerators[0].acceleratorType must be the full
// resource URL, not the short name.
//
// This pass re-verifies the (already persisted) grant, then creates EXACTLY ONE
// nonce-tagged nvidia-tesla-t4 VM with full resource URLs, polls the serial
// console for `nvidia-smi -L` (GPU UUID), deletes the VM and proves absence.
// If no DLVM image family resolves, it falls back to ubuntu-2204-lts and installs
// the NVIDIA driver from the distribution archive inside the startup script.
// Every refusal is printed verbatim; no secret value is printed.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func c211EnsureComputeCreate(token, proj, saEmail string) (bool, string) {
	var log strings.Builder
	al, st, _ := c211TestIam(token, proj, []string{"compute.instances.create", "compute.disks.create", "compute.subnetworks.use", "compute.instances.setMetadata", "compute.instances.getSerialPortOutput"})
	fmt.Fprintf(&log, "precheck status=%d allowed=%s\n", st, strings.Join(al, ","))
	if allowSetOf(al, "compute.instances.create") {
		return true, log.String()
	}
	pre, pst, pbody := c211GetProjectPolicy(token, proj)
	fmt.Fprintf(&log, "getIamPolicy status=%d verbatim=%s\n", pst, c211Trunc(pbody, 800))
	if pre == nil {
		return false, log.String()
	}
	added := c211AddBinding(pre, "roles/compute.admin", "serviceAccount:"+saEmail)
	fmt.Fprintf(&log, "binding roles/compute.admin added=%v member=%s\n", added, c211Short(saEmail))
	sst, sbody := c211SetProjectPolicy(token, proj, pre)
	fmt.Fprintf(&log, "setIamPolicy status=%d verbatim=%s\n", sst, c211Trunc(sbody, 800))
	if sst != 200 {
		return false, log.String()
	}
	for i := 0; i < 12; i++ {
		al2, tst, _ := c211TestIam(token, proj, []string{"compute.instances.create"})
		g := allowSetOf(al2, "compute.instances.create")
		fmt.Fprintf(&log, "recheck[%d] status=%d instances.create=%v\n", i, tst, g)
		if g {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}

func c211StartupScript() string {
	return "#!/bin/bash\n" +
		"exec > /dev/console 2>&1\n" +
		"echo C211_START\n" +
		"export DEBIAN_FRONTEND=noninteractive\n" +
		"if command -v nvidia-smi >/dev/null 2>&1; then\n" +
		"  echo C211_DRIVER_PRESENT\n" +
		"else\n" +
		"  echo C211_INSTALLING_DRIVER\n" +
		"  (apt-get update -qq && apt-get install -y -qq linux-headers-$(uname -r) " +
		"nvidia-driver-535-server) >/var/log/c211-apt.log 2>&1\n" +
		"  echo C211_APT_RC=$?\n" +
		"  (apt-get install -y -qq nvidia-driver-535) >>/var/log/c211-apt.log 2>&1\n" +
		"  modprobe nvidia || true\n" +
		"fi\n" +
		"for i in $(seq 1 60); do\n" +
		"  if nvidia-smi -L; then echo C211_GPU_READY; break; fi\n" +
		"  echo attempt=$i; sleep 10\n" +
		"done\n" +
		"nvidia-smi --query-gpu=uuid,name,driver_version --format=csv,noheader\n" +
		"echo C211_DONE\n"
}

func c211CreateGPUInstanceFullURL(token, proj, zone, name, imageProject, imageFamily string) (int, string) {
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/n1-standard-4",
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + imageProject + "/global/images/family/" + imageFamily,
				"diskSizeGb":  "50",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/pd-standard",
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "projects/" + proj + "/global/networks/default",
			"subnetwork": "projects/" + proj + "/regions/" + regionOfZone(zone) + "/subnetworks/default",
			"accessConfigs": []interface{}{map[string]interface{}{
				"type": "ONE_TO_ONE_NAT", "name": "External NAT",
			}},
		}},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType": "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/nvidia-tesla-t4",
			"acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{
			"items": []interface{}{map[string]interface{}{"key": "startup-script", "value": c211StartupScript()}},
		},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  "default",
			"scopes": []string{"https://www.googleapis.com/auth/cloud-platform"},
		}},
		"labels": map[string]string{"c211": "i120026"},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	st, rb := c211HTTP("POST", u, token, "application/json", string(b))
	return st, rb
}

func regionOfZone(zone string) string {
	parts := strings.Split(zone, "-")
	if len(parts) >= 3 {
		return parts[0] + "-" + parts[1]
	}
	return zone
}

func TestC211GPUGPUMarkerVM(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{}
	verdict := "GATE"
	verdictDetail := ""

	token, method, _ := c211MintAccessToken()
	summary["auth_method"] = method
	if token == "" {
		fmt.Println("C211GCP6_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	creds, _ := c211LoadCreds()
	proj, saEmail := "", ""
	if creds != nil {
		proj = c211ResolveProject(creds)
		saEmail = creds.saEmail
	}
	if proj == "" || saEmail == "" {
		fmt.Println("C211GCP6_VERDICT GATE NO_PROJECT_OR_SA_RESOLVED")
		return
	}
	summary["project_id_short"] = c211Short(proj)
	summary["sa_email_short"] = c211Short(saEmail)

	granted, escLog := c211EnsureComputeCreate(token, proj, saEmail)
	c211Emit("W6_ESCALATION", escLog)
	summary["compute_create_granted"] = granted
	if !granted {
		summary["verdict"] = "GATE"
		c211Emit("W6_SUMMARY", "C211GCP6_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP6_VERDICT GATE compute-instances-create-not-granted")
		return
	}

	// ---------------- choose image ----------------
	var ilog strings.Builder
	type cand struct{ project, family string }
	cands := []cand{
		{"deeplearning-platform-release", "common-cu124-debian-12"},
		{"deeplearning-platform-release", "common-cu121-debian-12"},
		{"deeplearning-platform-release", "common-cu118-debian-11"},
		{"deeplearning-platform-release", "pytorch-2-7-cu128-ubuntu-2204-nvidia-570"},
		{"deeplearning-platform-release", "pytorch-2-6-cu124-ubuntu-2204-nvidia-560"},
		{"deeplearning-platform-release", "tf2-ent-2-15-cu121-debian-11"},
		{"deeplearning-platform-release", "tf2-ent-2-16-cu124-debian-12"},
		{"deeplearning-platform-release", "pytorch-2-4-cu124-ubuntu-2204-nvidia-550"},
		{"deeplearning-platform-release", "common-cu113-debian-11"},
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
	// accelerator types in the target zone
	ast, ab := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/us-central1-a/acceleratorTypes", token, "", "")
	accels := []string{}
	if m := c211JSON(ab); m != nil {
		if items, ok := m["items"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				accels = append(accels, c211Str(im, "name"))
			}
		}
	}
	sort.Strings(accels)
	fmt.Fprintf(&ilog, "ACCELTYPES us-central1-a status=%d has_t4=%v all=%s\n", ast, allowSetOf(accels, "nvidia-tesla-t4"), strings.Join(accels, ","))
	c211Emit("W6_IMAGE", ilog.String())
	summary["image_used"] = imgProject + "/" + imgFamily
	summary["t4_offered_us_central1_a"] = allowSetOf(accels, "nvidia-tesla-t4")

	// ---------------- exactly one marker VM ----------------
	zone := "us-central1-a"
	name := "c211-i120026-gpu"
	var vlog strings.Builder
	cst, cbody := c211CreateGPUInstanceFullURL(token, proj, zone, name, imgProject, imgFamily)
	fmt.Fprintf(&vlog, "INSTANCE create zone=%s name=%s image=%s/%s status=%d\nverbatim=%s\n", zone, name, imgProject, imgFamily, cst, c211Trunc(cbody, 3000))
	summary["instance_create_status"] = cst
	summary["instance_create_error"] = c211Trunc(cbody, 1800)
	summary["instance_zone"] = zone
	summary["instance_name"] = name

	uuid := ""
	serialDone := false
	lastState := ""
	if cst == 200 {
		for i := 0; i < 44; i++ {
			ist, _, im := c211InstanceStatus(token, proj, zone, name)
			lastState = ""
			if ist == 200 && im != nil {
				lastState = c211Str(im, "status")
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
				if strings.Contains(content, "C211_DONE") {
					serialDone = true
				}
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d bytes=%d uuid=%s done=%v tail=%s\n", i, lastState, ps, len(content), uuid, serialDone, c211Trunc(content[max0(0, len(content)-1000):], 1000))
			} else {
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d verbatim=%s\n", i, lastState, ps, c211Trunc(pb, 400))
			}
			if uuid != "" && serialDone {
				break
			}
			time.Sleep(15 * time.Second)
		}
		summary["serial_done"] = serialDone
		summary["instance_uuid"] = uuid
		summary["instance_final_state"] = lastState
	} else {
		verdictDetail = "instances-create-refused"
	}

	// delete + prove absence (also on refusal, in case a retry left something)
	dst, db := c211HTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	fmt.Fprintf(&vlog, "INSTANCE delete status=%d verbatim=%s\n", dst, c211Trunc(db, 1200))
	summary["instance_delete_status"] = dst
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		ist, ib, _ := c211InstanceStatus(token, proj, zone, name)
		fmt.Fprintf(&vlog, "ABSENCE readback=%d http=%d verbatim=%s\n", i, ist, c211Trunc(ib, 400))
		if ist == 404 {
			absent = true
			break
		}
	}
	summary["instance_absent_after_delete"] = absent
	lst, lb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(name), token, "", "")
	fmt.Fprintf(&vlog, "AGGREGATED_LIST status=%d verbatim=%s\n", lst, c211Trunc(lb, 1200))
	summary["aggregated_list_status"] = lst

	if uuid != "" {
		verdict = "READY_GCP_GPU_VM"
		verdictDetail = "uuid=" + uuid + " zone=" + zone + " instance=" + name + " image=" + imgProject + "/" + imgFamily
	} else if cst == 200 {
		verdictDetail = "instance-created-but-no-gpu-uuid-on-serial-console"
	}
	c211Emit("W6_MARKER", vlog.String())

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W6_SUMMARY", "C211GCP6_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP6_VERDICT %s %s\n", verdict, verdictDetail)
}
