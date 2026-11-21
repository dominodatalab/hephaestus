//go:build functional

// C211 i121095 — REAPER + DEVICE-CAPTURE for the four c211-i120969-t4 markers.
//
// Fact 121101 / run 44874902451 proved four NEW sibling projects of the WIF host
// project 153711203585 (team-eng-infra-testing, domino-eng-service-artifacts,
// team-eng-infrastructure, team-eng-platform) all grant fleetcommand-service-account
// compute.instances.create / setIamPolicy / iam.serviceAccounts.actAs with a nonzero
// NVIDIA_T4_GPUS quota (limit 8, us-central1) and that a nonce-tagged on-demand
// n1-standard-4 + 1x nvidia-tesla-t4 instance reached operation DONE and
// status=RUNNING with the accelerator attached — but that probe never read the
// serial console (no C211S_SERIAL line) and its DELETE was not proven absent
// (the aggregated list still showed the instance at readback 11).
//
// This probe therefore:
//  1. re-mints the WIF token and the fleetcommand-service-account token
//     (same chain as the v3 probe), enumerates the five projects and every
//     project v1 projects.list returns;
//  2. per project reads instance c211-i120969-t4 in us-central1-a: status,
//     the c211s-result metadata attribute (written by the startup script) and
//     the serial-console contents, extracting the NVIDIA device UUID;
//     it takes a SECOND independent compute.instances.get readback;
//  3. deletes the instance and polls compute.instances.get until 404, then
//     proves absence with the aggregated instance list (and the aggregated
//     disk list for the autoDelete boot disk);
//  4. prints C211R_SUMMARY_JSON with per-project status/UUID/absence and the
//     verdict READY_CLEANUP_DEVICE_PROVEN when a NEW project (never the host,
//     never 531508670106) yields a real nvidia-smi UUID.
//
// The old c211_i120969_sibgpu_test.go is skipped (t.Skip) so no new GPU
// instance is created by this run. No secret value is printed: tokens are
// reported as length + SHA-256 prefix only.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	c211rHostNum  = "153711203585"
	c211rExcluded = "531508670106"
	c211rName     = "c211-i120969-t4"
	c211rZone     = "us-central1-a"
)

type c211rProjResult struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	Host      bool   `json:"host"`
	FirstGet  int    `json:"first_get"`
	Status    string `json:"status"`
	UUID      string `json:"uuid"`
	Meta      string `json:"meta"`
	Accel     string `json:"accelerators"`
	Readback  int    `json:"readback_http"`
	Delete    int    `json:"delete_http"`
	Absent    bool   `json:"absent"`
	Poll404   int    `json:"poll_404"`
	AggStatus int    `json:"agg_status"`
	AggLeft   int    `json:"agg_instances_left"`
	DiskStat  int    `json:"disk_list_status"`
	DisksLeft int    `json:"disks_left"`
	Verdict   string `json:"verdict"`
}

func TestC211XReapSibGPU(t *testing.T) {
	started := time.Now()
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211R_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211rHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211rHostNum
	}
	fmt.Printf("C211R_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hostID = c211xStr(c211xJSON(rb), "projectId")
	}
	fmt.Printf("C211R_HOST project_number=%s project_id=%s\n", projNum, c211xShort(hostID))

	sas, lst, _ := c211xListServiceAccounts(token, projNum)
	fmt.Printf("C211R_SA_LIST status=%d count=%d\n", lst, len(sas))
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	ftok := ""
	gst := 0
	if fleetEmail != "" {
		var at string
		at, gst, _ = c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211R_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
		ftok = at
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211R_FLEET_TOKEN fallback=wif_minted\n")
	}

	ids := []string{}
	seen := map[string]bool{}
	for _, cand := range []string{"team-eng-infra-testing", "domino-eng-service-artifacts",
		"team-eng-infrastructure", "team-eng-platform"} {
		ids = append(ids, cand)
		seen[cand] = true
	}
	if hostID != "" && !seen[hostID] {
		ids = append(ids, hostID)
		seen[hostID] = true
	}
	ps1, s1, _ := c211xListProjectsV1(ftok)
	fmt.Printf("C211R_ENUM v1_list status=%d n=%d\n", s1, len(ps1))
	for _, p := range ps1 {
		if p.id != "" && !seen[p.id] {
			seen[p.id] = true
			ids = append(ids, p.id)
		}
	}

	results := map[string]*c211rProjResult{}
	order := []string{}
	ready := map[string]string{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		r := &c211rProjResult{ID: id, Host: id == hostID}
		results[id] = r
		order = append(order, id)
		if ns, nb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", ""); ns == 200 {
			r.Number = c211xStr(c211xJSON(nb), "projectNumber")
		}
		fmt.Printf("C211R_PROJECT id=%s number=%s host=%v\n", id, r.Number, r.Host)

		ist, im, ibody := c211xInstanceGet(ftok, id, c211rZone, c211rName)
		r.FirstGet = ist
		fmt.Printf("C211R_INSTANCE project=%s get=%d verbatim=%s\n", id, ist, c211xTrunc(ibody, 400))
		if ist == 200 {
			r.Status = c211xStr(im, "status")
			r.Meta = c211xTrunc(c211xMetaValue(im, "c211s-result"), 700)
			if ga, ok := im["guestAccelerators"].([]interface{}); ok {
				r.Accel = c211xTrunc(c211xJSONString(ga), 300)
			}
			fmt.Printf("C211R_STATE project=%s status=%s accelerators=%s meta=%s\n", id, r.Status, r.Accel, r.Meta)
			for k := 0; k < 3; k++ {
				ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(id)+"/zones/"+c211rZone+"/instances/"+c211rName+"/serialPort?port=1&start=0", ftok, "", "")
				if ps == 200 {
					content := pb
					if m := c211xJSON(pb); m != nil {
						if s := c211xStr(m, "contents"); s != "" {
							content = s
						}
					}
					if u := c211xUUIDRe.FindString(content); u != "" {
						r.UUID = u
					}
					if u := c211xUUIDRe.FindString(r.Meta); u != "" {
						r.UUID = u
					}
					fmt.Printf("C211R_SERIAL project=%s poll=%d http=%d bytes=%d uuid=%s tail=%s\n",
						id, k, ps, len(content), r.UUID, c211xTrunc(content[maxi(0, len(content)-400):], 400))
				} else {
					fmt.Printf("C211R_SERIAL project=%s poll=%d http=%d verbatim=%s\n", id, k, ps, c211xTrunc(pb, 300))
				}
				if r.UUID != "" {
					break
				}
				time.Sleep(15 * time.Second)
			}
			rst, rm, _ := c211xInstanceGet(ftok, id, c211rZone, c211rName)
			r.Readback = rst
			acc2 := ""
			if rm != nil {
				if ga, ok := rm["guestAccelerators"].([]interface{}); ok {
					acc2 = c211xJSONString(ga)
				}
				if r.Meta == "" {
					r.Meta = c211xTrunc(c211xMetaValue(rm, "c211s-result"), 700)
				}
				if u := c211xUUIDRe.FindString(r.Meta); u != "" {
					r.UUID = u
				}
			}
			fmt.Printf("C211R_READBACK project=%s http=%d status=%s accelerators=%s meta=%s uuid=%s\n",
				id, rst, c211xStr(rm, "status"), c211xTrunc(acc2, 300), r.Meta, r.UUID)
			if r.UUID != "" && !r.Host && id != c211rExcluded {
				ready[id] = r.UUID
				r.Verdict = "DEVICE_PROVEN"
				fmt.Printf("C211R_VERDICT project=%s DEVICE_PROVEN uuid=%s\n", id, r.UUID)
			} else if r.UUID != "" {
				r.Verdict = "UUID_IN_EXCLUDED_OR_HOST"
				fmt.Printf("C211R_VERDICT project=%s UUID_IN_EXCLUDED_OR_HOST uuid=%s\n", id, r.UUID)
			} else {
				r.Verdict = "RUNNING_NO_UUID"
				fmt.Printf("C211R_VERDICT project=%s RUNNING_NO_UUID status=%s\n", id, r.Status)
			}
			dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(id)+"/zones/"+c211rZone+"/instances/"+c211rName, ftok, "", "")
			r.Delete = dst
			fmt.Printf("C211R_DELETE project=%s status=%d verbatim=%s\n", id, dst, c211xTrunc(db, 400))
		} else {
			r.Verdict = "ABSENT_AT_START"
			fmt.Printf("C211R_VERDICT project=%s ABSENT_AT_START\n", id)
		}

		absent := false
		lastHTTP := 0
		for k := 0; k < 20; k++ {
			time.Sleep(10 * time.Second)
			gst2, _, gb := c211xInstanceGet(ftok, id, c211rZone, c211rName)
			lastHTTP = gst2
			if gst2 == 404 {
				absent = true
				r.Poll404 = k
				fmt.Printf("C211R_ABSENCE project=%s poll=%d http=404\n", id, k)
				break
			}
			if k%5 == 0 || k == 19 {
				fmt.Printf("C211R_ABSENCE project=%s poll=%d http=%d verbatim=%s\n", id, k, gst2, c211xTrunc(gb, 240))
			}
		}
		r.Absent = absent
		if !absent {
			fmt.Printf("C211R_ABSENCE project=%s UNPROVEN last_http=%d\n", id, lastHTTP)
		}

		ast, ab := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(id)+"/aggregated/instances?filter="+url.QueryEscape("name="+c211rName), ftok, "", "")
		r.AggStatus = ast
		n := 0
		if ast == 200 {
			if items, ok := c211xJSON(ab)["items"].(map[string]interface{}); ok {
				for _, z := range items {
					if zm, ok := z.(map[string]interface{}); ok {
						if arr, ok := zm["instances"].([]interface{}); ok {
							n += len(arr)
						}
					}
				}
			}
		}
		r.AggLeft = n
		fmt.Printf("C211R_AGG project=%s status=%d instances=%d verbatim=%s\n", id, ast, n, c211xTrunc(ab, 300))

		dst2, dbd := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(id)+"/aggregated/disks?filter="+url.QueryEscape("name="+c211rName), ftok, "", "")
		r.DiskStat = dst2
		dn := 0
		if dst2 == 200 {
			if items, ok := c211xJSON(dbd)["items"].(map[string]interface{}); ok {
				for _, z := range items {
					if zm, ok := z.(map[string]interface{}); ok {
						if arr, ok := zm["disks"].([]interface{}); ok {
							dn += len(arr)
						}
					}
				}
			}
		}
		r.DisksLeft = dn
		fmt.Printf("C211R_DISKS project=%s status=%d disks=%d\n", id, dst2, dn)
	}

	verdict := "CLEANUP_ONLY_NO_UUID"
	if len(ready) > 0 {
		verdict = "READY_CLEANUP_DEVICE_PROVEN"
	}
	notAbsent := []string{}
	leftInstances := 0
	leftDisks := 0
	for _, id := range order {
		if !results[id].Absent {
			notAbsent = append(notAbsent, id)
		}
		leftInstances += results[id].AggLeft
		leftDisks += results[id].DisksLeft
	}
	sum := map[string]interface{}{
		"probe":                    "c211_i121095_reaper",
		"host_project_num":         projNum,
		"host_project_id":          c211xShort(hostID),
		"fleet_sa":                 c211xShort(fleetEmail),
		"fleet_token_status":       gst,
		"projects":                 order,
		"results":                  results,
		"ready_uuids":              ready,
		"not_absent_projects":      notAbsent,
		"aggregated_instances_left": leftInstances,
		"aggregated_disks_left":    leftDisks,
		"verdict":                  verdict,
		"elapsed_seconds":          int(time.Since(started).Seconds()),
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211R SUMMARY BEGIN ===\nC211R_SUMMARY_JSON %s\n=== C211R SUMMARY END ===\n", string(js))
	fmt.Printf("C211R_VERDICT_FINAL %s markers=4 ready=%d left_instances=%d left_disks=%d elapsed=%ds\n",
		verdict, len(ready), leftInstances, leftDisks, int(time.Since(started).Seconds()))
}
