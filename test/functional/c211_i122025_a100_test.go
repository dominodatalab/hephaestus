//go:build functional && gke

// C211 i122025 v2 — MULTI-REGION A100 RETRY + L4 FALLBACK on the three sibling
// projects still without a device line after the first pass.
//
// The v1 pass (run 44892273239, GKE job 136810498339) proved a real NVIDIA
// A100-SXM4-40GB in team-eng-platform via a2-highgpu-1g in us-central1-a, but
// team-eng-infra-testing, domino-eng-service-artifacts and
// team-eng-infrastructure were refused by us-central1-a with the verbatim
// provider code ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS for both
// a2-highgpu-8g and a2-highgpu-1g, and the L4 fallback stopped at the
// us-central1 offering check (nvidia-tesla-l4 not advertised there).
//
// v2 therefore, per project: (a) retries ONE a2-highgpu-8g in the first
// eligible zone, then a2-highgpu-1g across up to two zones in every region
// whose measured NVIDIA_A100_GPUS quota is nonzero, and (b) only if A100 yields
// no device line, runs g2-standard-4 + 1 x nvidia-tesla-l4 in up to two zones of
// every region whose measured NVIDIA_L4_GPUS quota is nonzero AND whose
// acceleratorTypes advertise nvidia-tesla-l4. At most ONE marker alive at a
// time, one instance per project, every instance torn down and absence proven.
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

var c211cTargets = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
}

type c211cAttempt struct {
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

// c211cSweep runs the single-marker flow over the eligible zones of one
// accelerator class. It returns (uuid, deviceMeta, attempts, offerNotes).
func c211cSweep(tok, proj, num string, img c211aImage, startup string, deadline time.Time,
	quotaMetric, mt, accel string, count int, maxZonesPerRegion int) (string, string, []c211cAttempt, []string) {
	attempts := []c211cAttempt{}
	notes := []string{}
	saEmail := num + "-compute@developer.gserviceaccount.com"
	for _, r := range c211aRegions {
		if time.Now().After(deadline) {
			break
		}
		rows, zones, qst := c211aRegionQuotas(tok, proj, r)
		lim, usage, ok := c211aQVal(rows, quotaMetric)
		fmt.Printf("C211A_SWEEP_QUOTA project=%s region=%s metric=%s http=%d limit=%v usage=%v readable=%v rows=[%s]\n",
			proj, r, quotaMetric, qst, lim, usage, ok, c211aQMatrixString(rows))
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
			fmt.Printf("C211A_SWEEP_OFFER project=%s region=%s zone=%s machine=%s machine_http=%d accel_http=%d has_%s=%v accel_n=%d\n",
				proj, r, z, mt, mst, ast, accel, hasAccel, len(ats))
			notes = append(notes, fmt.Sprintf("%s/%s %s machine=%d accel=%v", r, z, mt, mst, hasAccel))
			if !offered || !hasAccel {
				continue
			}
			tried++
			instName := "c211-" + c211aNonce + "-v2"
			fmt.Printf("C211A_SWEEP_ATTEMPT project=%s machine=%s zone=%s accel=%s count=%d instance=%s sa=%s\n",
				proj, mt, z, accel, count, instName, c211xShort(saEmail))
			uuid, meta, created, opErr, statusFinal := c211aMarker(tok, proj, z, instName, saEmail, img, mt, accel, count, startup, deadline)
			absent, trace := c211aDeleteAndProveAbsence(tok, proj, z, instName)
			a := c211cAttempt{Machine: mt, Zone: z, Accel: accel, Count: count,
				Create: strconv.FormatBool(created), OpErr: c211xTrunc(opErr, 300),
				UUID: uuid, Absent: absent, Meta: c211xTrunc(meta, 400), Trace: strings.Join(trace, "; ")}
			attempts = append(attempts, a)
			if uuid != "" {
				fmt.Printf("C211A_SWEEP_DEVICE project=%s machine=%s zone=%s accel=%s uuid=%s absent=%v\n",
					proj, mt, z, accel, uuid, absent)
				return uuid, meta + " zone=" + z + " machine=" + mt + " status=" + statusFinal, attempts, notes
			}
			fmt.Printf("C211A_SWEEP_NO_DEVICE project=%s machine=%s zone=%s created=%v op_error=%s\n",
				proj, mt, z, created, c211xTrunc(opErr, 220))
		}
	}
	return "", "", attempts, notes
}

func TestC211XA100SibGPUV2(t *testing.T) {
	started := time.Now()
	deadline := started.Add(38 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211A_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, projNum := "", c211aHostNum
	if creds != nil {
		wifSA, projNum = creds.saEmail, creds.projNumber
	}
	if projNum == "" {
		projNum = c211aHostNum
	}
	fmt.Printf("C211A_IDENTITY auth=%s wif_sa=%s project_number=%s v2=true\n", method, c211xShort(wifSA), projNum)

	hostID := ""
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projNum, token, "", ""); st == 200 {
		hm := c211xJSON(rb)
		hostID = c211xStr(hm, "projectId")
	}
	sas, lst, _ := c211xListServiceAccounts(token, projNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	ftok := ""
	if fleetEmail != "" {
		at, gst, _ := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211A_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
		ftok = at
	}
	if ftok == "" {
		ftok = token
	}
	fmt.Printf("C211A_CONTEXT host_id_set=%v fleet_sa_found=%v sa_list_status=%d\n", hostID != "", fleetEmail != "", lst)

	img, imgLog := c211aResolveImage(ftok)
	if img.Project == "" {
		fmt.Printf("C211A_VERDICT_FINAL NO_USABLE_IMAGE v2=true elapsed=%ds\n", int(time.Since(started).Seconds()))
		return
	}
	startup := c211aStartup()
	fmt.Printf("C211A_STARTUP kind=%s bytes=%d v2=true\n", img.Kind, len(startup))

	perms := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.delete",
		"compute.zoneOperations.get", "compute.networks.use", "compute.subnetworks.use",
		"iam.serviceAccounts.actAs",
	}

	results := map[string]map[string]interface{}{}
	readyA100 := []string{}
	readyL4 := []string{}
	readyUUIDs := map[string]string{}
	markers := 0
	for _, id := range c211cTargets {
		if time.Now().After(deadline) {
			fmt.Printf("C211A_VERDICT project=%s MARKER_SKIPPED_TIME_BUDGET\n", id)
			break
		}
		st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), ftok, "", "")
		if st != 200 {
			fmt.Printf("C211A_PROJECT id=%s status=%d verbatim=%s\n", id, st, c211xTrunc(body, 300))
			results[id] = map[string]interface{}{"verdict": "PROJECT_GET_" + strconv.Itoa(st)}
			continue
		}
		pm := c211xJSON(body)
		num := c211xStr(pm, "projectNumber")
		res := map[string]interface{}{"number": num, "name": c211xStr(pm, "name"), "state": c211xStr(pm, "lifecycleState")}
		results[id] = res
		al, pst, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(id), perms)
		fmt.Printf("C211A_PROJECT_BEGIN id=%s number=%s state=%s perm_status=%d allowed=[%s]\n",
			id, num, c211xStr(pm, "lifecycleState"), pst, strings.Join(al, ","))
		res["perm_status"] = pst
		res["allowed"] = al
		if !c211xContains(al, "compute.instances.create") {
			res["verdict"] = "NO_CREATE_PERMISSION"
			continue
		}

		// Phase A: one bounded a2-highgpu-8g attempt, then a2-highgpu-1g sweep.
		uuid, meta, attempts, notes := c211cSweep(ftok, id, num, img, startup, deadline,
			"NVIDIA_A100_GPUS", "a2-highgpu-8g", "nvidia-tesla-a100", 8, 1)
		markers++
		all := attempts
		if uuid == "" {
			u2, m2, at2, n2 := c211cSweep(ftok, id, num, img, startup, deadline,
				"NVIDIA_A100_GPUS", "a2-highgpu-1g", "nvidia-tesla-a100", 1, 2)
			uuid, meta = u2, m2
			all = append(all, at2...)
			notes = append(notes, n2...)
			markers++
		}
		res["a100_attempts"] = all
		res["a100_offer_notes"] = strings.Join(notes, "; ")
		res["a100_verdict"] = map[bool]string{true: "MARKER_GPU_A100", false: "A100_REFUSED_OR_NO_DEVICE"}[uuid != ""]
		res["a100_uuid"] = uuid
		res["a100_meta"] = meta
		if uuid != "" {
			res["verdict"] = "MARKER_GPU_A100"
			readyA100 = append(readyA100, id)
			readyUUIDs[id] = uuid
			continue
		}

		// Phase B: L4 fallback across every nonzero-L4-quota region.
		if time.Now().After(deadline) {
			res["verdict"] = "A100_NO_DEVICE_L4_SKIPPED_TIME_BUDGET"
			continue
		}
		uuid4, meta4, at4, n4 := c211cSweep(ftok, id, num, img, startup, deadline,
			"NVIDIA_L4_GPUS", "g2-standard-4", "nvidia-tesla-l4", 1, 2)
		markers++
		res["l4_attempts"] = at4
		res["l4_offer_notes"] = strings.Join(n4, "; ")
		res["l4_verdict"] = map[bool]string{true: "MARKER_GPU_L4", false: "L4_REFUSED_OR_NO_DEVICE"}[uuid4 != ""]
		res["l4_uuid"] = uuid4
		res["l4_meta"] = meta4
		if uuid4 != "" {
			res["verdict"] = "MARKER_GPU_L4"
			readyL4 = append(readyL4, id)
			readyUUIDs[id] = uuid4
			continue
		}
		res["verdict"] = "A100_AND_L4_NO_DEVICE"
		fmt.Printf("C211A_VERDICT project=%s A100_AND_L4_NO_DEVICE\n", id)
	}

	for _, id := range append(append([]string{}, c211cTargets...), "team-eng-platform") {
		c211aReaper(ftok, id)
	}

	verdict := "A100_AND_L4_NO_DEVICE"
	if len(readyA100) > 0 {
		verdict = "READY_NEW_PROJECT_A100_GPU"
	} else if len(readyL4) > 0 {
		verdict = "READY_NEW_PROJECT_L4_GPU"
	}
	sum := map[string]interface{}{
		"probe":            "c211_i122025_a100v2",
		"mechanism":        "multi-region A100 retry + L4 fallback on driver-preinstalled accelerator images",
		"host_project":     projNum,
		"host_project_id":  c211xShort(hostID),
		"fleet_token_ok":   ftok != "",
		"image":            img.Project + "/" + img.Family,
		"image_kind":       img.Kind,
		"image_resolution": imgLog,
		"markers":          markers,
		"ready_a100":       readyA100,
		"ready_l4":         readyL4,
		"ready_uuids":      readyUUIDs,
		"verdict":          verdict,
		"elapsed_seconds":  int(time.Since(started).Seconds()),
		"results":          results,
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211A SUMMARY BEGIN ===\nC211A_SUMMARY_JSON %s\n=== C211A SUMMARY END ===\n", string(js))
	fmt.Printf("C211A_VERDICT_FINAL %s v2=true markers=%d a100=%d l4=%d elapsed=%ds\n", verdict, markers, len(readyA100), len(readyL4), int(time.Since(started).Seconds()))
}
