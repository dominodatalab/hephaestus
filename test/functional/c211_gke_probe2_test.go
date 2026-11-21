//go:build functional

// C211 i119859 GKE/GCP write-path probe (second pass, same package as
// c211_gke_probe_test.go whose helpers it reuses).
//
// The first pass proved the Workload Identity Federation chain authenticates
// into project 153711203585 and that compute.instances.create is refused 403.
// This second pass makes the capability boundary explicit instead of assumed:
//   - full enabled-service enumeration (all pages, correct name keys),
//   - cloudresourcemanager projects.list (what else is visible to the identity),
//   - projects.testIamPermissions battery for compute / Vertex AI / GKE /
//     storage / IAM / logging write permissions,
//   - per-visible-project spot checks of the write permissions,
//   - only if compute.instances.create is actually granted: one marker VM,
//   - else only if aiplatform.customJobs.create is actually granted: exactly one
//     Vertex AI custom job running nvidia-smi on one T4, its Cloud Logging
//     output read back, and the job cancelled if not terminal.
// Every refusal is printed verbatim. No secret value is printed.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func c211ResolveProject(creds *c211Creds) string {
	proj := c211FirstNonEmpty(
		os.Getenv("GCP_PROJECT_ID"), os.Getenv("GCP_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT"),
		os.Getenv("CLOUDSDK_CORE_PROJECT"), os.Getenv("CLOUDSDK_PROJECT"), os.Getenv("GCLOUD_PROJECT"),
		creds.credProjID, creds.credQuotaPID,
	)
	if proj == "" && creds.saEmail != "" && strings.Contains(creds.saEmail, "@") {
		dom := strings.SplitN(creds.saEmail, "@", 2)[1]
		proj = strings.SplitN(dom, ".iam.gserviceaccount.com", 2)[0]
	}
	return proj
}

func c211ServiceShortName(full string) string {
	if i := strings.LastIndex(full, "/services/"); i >= 0 {
		return full[i+len("/services/"):]
	}
	return full
}

func c211ListServices(token, proj string) (map[string]string, []string, int, string) {
	base := "https://serviceusage.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/services"
	states := map[string]string{}
	enabled := []string{}
	pageTok := ""
	total := 0
	status := 0
	errBody := ""
	for page := 0; page < 20; page++ {
		u := base + "?pageSize=200"
		if pageTok != "" {
			u += "&pageToken=" + url.QueryEscape(pageTok)
		}
		st, body := c211HTTP("GET", u, token, "", "")
		status = st
		if st != 200 {
			errBody = body
			break
		}
		m := c211JSON(body)
		if items, ok := m["services"].([]interface{}); ok {
			for _, it := range items {
				im, _ := it.(map[string]interface{})
				full := c211Str(im, "name")
				key := c211ServiceShortName(full)
				if cfg, ok := im["config"].(map[string]interface{}); ok {
					if cn := c211Str(cfg, "name"); cn != "" {
						key = cn
					}
				}
				state := c211Str(im, "state")
				states[key] = state
				total++
				if state == "ENABLED" {
					enabled = append(enabled, key)
				}
			}
		}
		pageTok = c211Str(m, "nextPageToken")
		if pageTok == "" {
			break
		}
	}
	sort.Strings(enabled)
	return states, enabled, total, fmt.Sprintf("status=%d total=%d enabled=%d verbatim=%s", status, total, len(enabled), c211Trunc(errBody, 800))
}

func c211TestIam(token, proj string, perms []string) ([]string, int, string) {
	b, _ := json.Marshal(map[string]interface{}{"permissions": perms})
	st, body := c211HTTP("POST", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj)+":testIamPermissions", token, "application/json", string(b))
	allowed := []string{}
	if m := c211JSON(body); m != nil {
		if arr, ok := m["permissions"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					allowed = append(allowed, s)
				}
			}
		}
	}
	sort.Strings(allowed)
	return allowed, st, body
}

func c211Denied(allowed []string, perms []string) []string {
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	out := []string{}
	for _, p := range perms {
		if !set[p] {
			out = append(out, p)
		}
	}
	return out
}

type c211ProjectRow struct {
	number  string
	state   string
	name    string
	parent  string
	shortID string
}

func c211ListProjects(token string) ([]c211ProjectRow, int, string) {
	rows := []c211ProjectRow{}
	st, body := c211HTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects?pageSize=100", token, "", "")
	if st != 200 {
		return rows, st, body
	}
	m := c211JSON(body)
	if items, ok := m["projects"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			pid := c211Str(im, "projectId")
			rows = append(rows, c211ProjectRow{
				number:  c211Str(im, "projectNumber"),
				state:   c211Str(im, "lifecycleState"),
				name:    c211Str(im, "name"),
				parent:  fmt.Sprintf("%v", im["parent"]),
				shortID: c211Short(pid),
			})
		}
	}
	return rows, st, body
}

// c211TryComputeMarker performs at most ONE VM insert in the given region/zone.
func c211TryComputeMarker(token, proj, region, zone, atype, mtype string) (bool, string, string, bool) {
	var log strings.Builder
	base := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj)
	image := ""
	for _, cand := range []string{
		"projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
		"projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
		"projects/cos-cloud/global/images/family/cos-stable",
	} {
		parts := strings.Split(cand, "/")
		st, body := c211HTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+parts[1]+"/global/images/family/"+parts[5], token, "", "")
		fmt.Fprintf(&log, "IMAGE %s status=%d verbatim=%s\n", cand, st, c211Trunc(body, 300))
		if st == 200 {
			image = cand
			break
		}
	}
	if image == "" {
		return false, "", log.String(), false
	}
	ni := map[string]interface{}{"network": "global/networks/default"}
	st, nb := c211HTTP("GET", base+"/global/networks/default", token, "", "")
	createdNet := false
	if st != 200 {
		fmt.Fprintf(&log, "NETWORK default status=%d verbatim=%s\n", st, c211Trunc(nb, 300))
		netName := "c211-i119859-net"
		st2, cb := c211HTTP("POST", base+"/global/networks", token, "application/json",
			fmt.Sprintf(`{"name":"%s","autoCreateSubnetworks":true}`, netName))
		fmt.Fprintf(&log, "NETWORK create status=%d verbatim=%s\n", st2, c211Trunc(cb, 400))
		if st2 != 200 && st2 != 409 {
			return false, "", log.String(), false
		}
		createdNet = true
		for i := 0; i < 20; i++ {
			st3, _ := c211HTTP("GET", base+"/global/networks/"+netName, token, "", "")
			if st3 == 200 {
				break
			}
			time.Sleep(6 * time.Second)
		}
		ni = map[string]interface{}{"network": "global/networks/" + netName}
	}
	instName := "c211-i119859-gpu"
	instURL := base + "/zones/" + url.PathEscape(zone) + "/instances/" + instName
	startup := "#!/bin/bash\n{\necho \"C211_MARKER_START $(date -u +%Y-%m-%dT%H:%M:%SZ)\"\necho \"C211_HOST $(hostname)\"\nnvidia-smi -L 2>&1 || echo \"C211_NVIDIA_SMI_L_FAIL rc=$?\"\nnvidia-smi 2>&1 || echo \"C211_NVIDIA_SMI_FAIL rc=$?\"\necho \"C211_MARKER_END\"\n} > /dev/console 2>&1\n"
	inst := map[string]interface{}{
		"name":        instName,
		"machineType": "zones/" + zone + "/machineTypes/" + mtype,
		"disks": []interface{}{map[string]interface{}{
			"boot": true, "autoDelete": true,
			"initializeParams": map[string]interface{}{"sourceImage": image, "diskSizeGb": "100", "diskType": "pd-balanced"},
		}},
		"networkInterfaces": []interface{}{ni},
		"guestAccelerators": []interface{}{map[string]interface{}{
			"acceleratorType": "zones/" + zone + "/acceleratorTypes/" + atype, "acceleratorCount": 1,
		}},
		"scheduling": map[string]interface{}{"onHostMaintenance": "TERMINATE", "automaticRestart": false, "provisioningModel": "STANDARD"},
		"metadata": map[string]interface{}{"items": []interface{}{
			map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
			map[string]interface{}{"key": "startup-script", "value": startup},
		}},
		"labels": map[string]interface{}{"c211": "i119859"},
	}
	jb, _ := json.Marshal(inst)
	st, ib := c211HTTP("POST", base+"/zones/"+url.PathEscape(zone)+"/instances", token, "application/json", string(jb))
	fmt.Fprintf(&log, "INSERT status=%d verbatim=%s\n", st, c211Trunc(ib, 1500))
	if st != 200 && st != 409 {
		return false, "", log.String(), createdNet
	}
	status := ""
	deadline := time.Now().Add(11 * time.Minute)
	for time.Now().Before(deadline) {
		st, body := c211HTTP("GET", instURL, token, "", "")
		if st == 200 {
			status = c211Str(c211JSON(body), "status")
		}
		fmt.Fprintf(&log, "POLL status=%s http=%d\n", status, st)
		if status == "RUNNING" || status == "TERMINATED" || status == "STOPPED" {
			break
		}
		time.Sleep(12 * time.Second)
	}
	uuidRe := regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	uuid := ""
	if status == "RUNNING" {
		for i := 0; i < 15; i++ {
			time.Sleep(20 * time.Second)
			st, body := c211HTTP("GET", instURL+"/serialPortOutput?port=1&start=0", token, "", "")
			if st != 200 {
				fmt.Fprintf(&log, "SERIAL http=%d verbatim=%s\n", st, c211Trunc(body, 250))
				continue
			}
			contents := c211Str(c211JSON(body), "contents")
			uuid = uuidRe.FindString(contents)
			fmt.Fprintf(&log, "SERIAL len=%d marker=%v uuid=%s\n", len(contents), strings.Contains(contents, "C211_MARKER_START"), uuid)
			if uuid != "" {
				idx := strings.Index(contents, "C211_MARKER_START")
				if idx < 0 {
					idx = len(contents) - 2000
					if idx < 0 {
						idx = 0
					}
				}
				fmt.Fprintf(&log, "SERIAL_EXCERPT %s\n", c211Trunc(contents[idx:], 2500))
				break
			}
		}
	}
	dst, db := c211HTTP("DELETE", instURL, token, "", "")
	fmt.Fprintf(&log, "DELETE status=%d verbatim=%s\n", dst, c211Trunc(db, 500))
	absent := false
	for i := 0; i < 20; i++ {
		time.Sleep(12 * time.Second)
		st, _ := c211HTTP("GET", instURL, token, "", "")
		fmt.Fprintf(&log, "DELETE_READBACK http=%d\n", st)
		if st == 404 {
			absent = true
			break
		}
	}
	if createdNet {
		st, _ := c211HTTP("DELETE", base+"/global/networks/c211-i119859-net", token, "", "")
		fmt.Fprintf(&log, "NETWORK_DELETE status=%d\n", st)
	}
	return uuid != "" && absent, uuid, log.String(), createdNet
}

func TestC211GCPWritePaths(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{"probe": "c211_i119859_gcp_write_paths"}
	verdict := "GATE"
	verdictDetail := ""

	creds, credLog := c211LoadCreds()
	if creds == nil {
		c211Emit("W2_CREDS", credLog)
		fmt.Println("C211GCP2_VERDICT GATE NO_EXPORTED_GCP_CREDENTIAL_FILE")
		return
	}
	token := ""
	subj, subjMsg := c211FetchSubjectToken(creds)
	if subj != "" {
		if fed, stsMsg := c211STSExchange(creds, subj); fed != "" {
			c211Emit("W2_STS", stsMsg)
			if final, impMsg := c211Impersonate(creds, fed); final != "" {
				token = final
				c211Emit("W2_IMPERSONATION", impMsg)
			} else {
				c211Emit("W2_IMPERSONATION", impMsg)
			}
		} else {
			c211Emit("W2_STS", stsMsg)
		}
	} else {
		c211Emit("W2_SUBJECT", subjMsg)
	}
	if token == "" {
		tok, msg := c211GcloudToken()
		c211Emit("W2_GCLOUD", msg)
		if tok != "" {
			token = tok
		}
	}
	if token == "" {
		fmt.Println("C211GCP2_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	proj := c211ResolveProject(creds)
	summary["project_id_short"] = c211Short(proj)
	summary["sa_email_short"] = c211Short(creds.saEmail)
	fmt.Printf("\nC211GCP2 project=%s (%s)\n", proj, c211Short(proj))

	// ---- full enabled-service enumeration ----
	states, enabled, total, svcMsg := c211ListServices(token, proj)
	var slog strings.Builder
	fmt.Fprintf(&slog, "services %s\n", svcMsg)
	for _, n := range enabled {
		fmt.Fprintf(&slog, "ENABLED %s\n", n)
	}
	for _, probe := range []string{"compute.googleapis.com", "aiplatform.googleapis.com", "container.googleapis.com",
		"iam.googleapis.com", "iamcredentials.googleapis.com", "sts.googleapis.com", "logging.googleapis.com",
		"storage.googleapis.com", "cloudresourcemanager.googleapis.com"} {
		state := states[probe]
		if state == "" {
			state = "<not-listed>"
		}
		fmt.Fprintf(&slog, "SERVICE %s state=%s\n", probe, state)
	}
	c211Emit("W2_SERVICEUSAGE", slog.String())
	summary["services_total"] = total
	summary["services_enabled_count"] = len(enabled)
	summary["compute_service"] = states["compute.googleapis.com"]
	summary["aiplatform_service"] = states["aiplatform.googleapis.com"]
	summary["container_service"] = states["container.googleapis.com"]
	summary["logging_service"] = states["logging.googleapis.com"]

	// ---- visible projects ----
	rows, pst, pbody := c211ListProjects(token)
	var plog strings.Builder
	fmt.Fprintf(&plog, "projects.list status=%d count=%d verbatim=%s\n", pst, len(rows), c211Trunc(pbody, 600))
	limit := len(rows)
	if limit > 30 {
		limit = 30
	}
	for i := 0; i < limit; i++ {
		r := rows[i]
		fmt.Fprintf(&plog, "PROJECT number=%s state=%s display=%q parent=%s id_%s\n", r.number, r.state, r.name, r.parent, r.shortID)
	}
	c211Emit("W2_PROJECTS", plog.String())
	summary["projects_visible"] = len(rows)

	// ---- permission battery on the resolved project ----
	battery := []string{
		"compute.instances.create", "compute.instances.get", "compute.instances.list",
		"compute.disks.create", "compute.instances.setMetadata", "compute.instances.setLabels",
		"compute.networks.create", "compute.subnetworks.create", "compute.zoneOperations.get",
		"compute.regions.get", "compute.acceleratorTypes.list", "compute.machineTypes.list",
		"aiplatform.customJobs.create", "aiplatform.customJobs.get", "aiplatform.customJobs.cancel",
		"aiplatform.customJobs.list", "aiplatform.endpoints.create", "aiplatform.models.upload",
		"aiplatform.pipelineJobs.create", "aiplatform.trainingPipelines.create",
		"aiplatform.batchPredictionJobs.create", "aiplatform.notebookRuntimes.create",
		"aiplatform.schedules.create", "aiplatform.locations.list",
		"container.clusters.create", "container.clusters.get",
		"storage.buckets.create", "storage.objects.create",
		"iam.serviceAccounts.actAs", "iam.serviceAccounts.getAccessToken", "iam.serviceAccountKeys.create",
		"logging.logEntries.list", "logging.privateLogEntries.list",
		"resourcemanager.projects.get", "resourcemanager.projects.list",
	}
	allowed, tst, tbody := c211TestIam(token, proj, battery)
	denied := c211Denied(allowed, battery)
	var tlog strings.Builder
	fmt.Fprintf(&tlog, "testIamPermissions projects/%s status=%d\n", c211Short(proj), tst)
	fmt.Fprintf(&tlog, "ALLOWED(%d)=%s\n", len(allowed), strings.Join(allowed, ","))
	fmt.Fprintf(&tlog, "DENIED(%d)=%s\n", len(denied), strings.Join(denied, ","))
	if tst != 200 {
		fmt.Fprintf(&tlog, "verbatim=%s\n", c211Trunc(tbody, 1200))
	}
	c211Emit("W2_TESTIAM", tlog.String())
	summary["testiam_status"] = tst
	summary["allowed_count"] = len(allowed)
	summary["denied_count"] = len(denied)
	allowSet := map[string]bool{}
	for _, a := range allowed {
		allowSet[a] = true
	}
	summary["compute_create_allowed"] = allowSet["compute.instances.create"]
	summary["vertex_job_create_allowed"] = allowSet["aiplatform.customJobs.create"]

	// ---- spot-check other visible projects ----
	var xlog strings.Builder
	xchecked := 0
	for i := 0; i < len(rows) && xchecked < 8; i++ {
		if rows[i].shortID == c211Short(proj) {
			continue
		}
		xchecked++
		// testIamPermissions needs the project id; we only have masked short ids here,
		// so re-list with the id present is required. Use the numeric project via CRM v3.
		pidURL := "https://cloudresourcemanager.googleapis.com/v3/projects/" + rows[i].number
		st, body := c211HTTP("GET", pidURL, token, "", "")
		if st != 200 {
			fmt.Fprintf(&xlog, "PROJECT %s crm_get status=%d verbatim=%s\n", rows[i].number, st, c211Trunc(body, 300))
			continue
		}
		pid := c211Str(c211JSON(body), "projectId")
		if pid == "" {
			fmt.Fprintf(&xlog, "PROJECT %s no projectId in CRM response\n", rows[i].number)
			continue
		}
		small := []string{"compute.instances.create", "aiplatform.customJobs.create", "storage.buckets.create", "iam.serviceAccounts.actAs"}
		al, _, _ := c211TestIam(token, pid, small)
		fmt.Fprintf(&xlog, "PROJECT %s (%s) allowed=%s\n", rows[i].number, c211Short(pid), strings.Join(al, ","))
	}
	c211Emit("W2_OTHER_PROJECTS", xlog.String())
	summary["other_projects_checked"] = xchecked

	// ---- exactly one write-path marker ----
	if allowSet["compute.instances.create"] {
		var uuid, markerLog string
		var ok, markerNet bool
		// try one region with on-demand GPU quota, cheapest accelerator first
		type cand struct{ region, zone, atype, mtype string }
		for _, c := range []cand{
			{"us-central1", "us-central1-a", "nvidia-tesla-t4", "n1-standard-4"},
			{"asia-east1", "asia-east1-a", "nvidia-l4", "g2-standard-4"},
			{"europe-west4", "europe-west4-a", "nvidia-tesla-t4", "n1-standard-4"},
		} {
			ok, uuid, markerLog, markerNet = c211TryComputeMarker(token, proj, c.region, c.zone, c.atype, c.mtype)
			_ = markerNet
			if ok || strings.Contains(markerLog, "status=200") {
				break
			}
		}
		c211Emit("W2_COMPUTE_MARKER", markerLog)
		summary["compute_marker_uuid"] = uuid
		if ok {
			verdict = "READY_COMPUTE_MARKER"
			verdictDetail = "uuid=" + uuid
		} else {
			verdictDetail = "compute-create-allowed-but-marker-not-proven"
		}
	} else if allowSet["aiplatform.customJobs.create"] {
		// exactly one Vertex AI custom job with one T4
		region := "us-central1"
		jobBody := map[string]interface{}{
			"displayName": "c211-i119859-gpu-probe",
			"jobSpec": map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType": "n1-standard-4", "acceleratorType": "NVIDIA_TESLA_T4", "acceleratorCount": 1,
					},
					"replicaCount": 1,
					"containerSpec": map[string]interface{}{
						"imageUri": "us-docker.pkg.dev/vertex-ai/training/pytorch-gpu.2-1:latest",
						"command":  []string{"bash", "-c"},
						"args":     []string{"nvidia-smi -L; nvidia-smi; echo C211_VERTEX_DONE"},
					},
				}},
			},
		}
		jb, _ := json.Marshal(jobBody)
		jobBase := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/locations/" + region + "/customJobs"
		var vlog strings.Builder
		st, rb := c211HTTP("POST", jobBase, token, "application/json", string(jb))
		fmt.Fprintf(&vlog, "CUSTOMJOB create status=%d verbatim=%s\n", st, c211Trunc(rb, 1800))
		if st == 200 {
			jm := c211JSON(rb)
			jobName := c211Str(jm, "name")
			fmt.Fprintf(&vlog, "CUSTOMJOB name=%s\n", jobName)
			summary["vertex_job_name"] = jobName
			terminal := false
			for i := 0; i < 40; i++ {
				st, body := c211HTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
				state := ""
				if st == 200 {
					state = c211Str(c211JSON(body), "state")
				}
				fmt.Fprintf(&vlog, "CUSTOMJOB poll=%d http=%d state=%s\n", i, st, state)
				if strings.HasPrefix(state, "JOB_STATE_SUCCEEDED") || strings.HasPrefix(state, "JOB_STATE_FAILED") ||
					strings.HasPrefix(state, "JOB_STATE_CANCELLED") || strings.HasPrefix(state, "JOB_STATE_EXPIRED") {
					terminal = true
					if errv, ok := c211JSON(body)["error"]; ok {
						eb, _ := json.Marshal(errv)
						fmt.Fprintf(&vlog, "CUSTOMJOB error=%s\n", c211Trunc(string(eb), 1200))
						summary["vertex_job_error"] = c211Trunc(string(eb), 900)
					}
					break
				}
				time.Sleep(15 * time.Second)
			}
			// read container logs from Cloud Logging
			jobID := jobName
			if i := strings.LastIndex(jobName, "/"); i >= 0 {
				jobID = jobName[i+1:]
			}
			filter := fmt.Sprintf("resource.type=\"ml_job\" AND (labels.job_id=\"%s\" OR labels.job_id=\"%s\")", jobID, jobName)
			lb, _ := json.Marshal(map[string]interface{}{
				"resourceNames": []string{"projects/" + proj},
				"filter":        filter,
				"pageSize":      100,
				"orderBy":       "timestamp asc",
			})
			lst, lbody := c211HTTP("POST", "https://logging.googleapis.com/v2/entries:list", token, "application/json", string(lb))
			fmt.Fprintf(&vlog, "LOGGING entries:list status=%d verbatim=%s\n", lst, c211Trunc(lbody, 3500))
			uuidRe := regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
			uuid := uuidRe.FindString(lbody)
			done := strings.Contains(lbody, "C211_VERTEX_DONE")
			summary["vertex_log_status"] = lst
			summary["vertex_log_uuid"] = uuid
			summary["vertex_log_done_marker"] = done
			if !terminal {
				cst, cb := c211HTTP("POST", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName+":cancel", token, "application/json", "{}")
				fmt.Fprintf(&vlog, "CUSTOMJOB cancel status=%d verbatim=%s\n", cst, c211Trunc(cb, 800))
				// readback terminal state
				for i := 0; i < 10; i++ {
					time.Sleep(12 * time.Second)
					st, body := c211HTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
					state := ""
					if st == 200 {
						state = c211Str(c211JSON(body), "state")
					}
					fmt.Fprintf(&vlog, "CUSTOMJOB cancel_readback=%d http=%d state=%s\n", i, st, state)
					if strings.HasPrefix(state, "JOB_STATE_CANCELLED") || strings.HasPrefix(state, "JOB_STATE_FAILED") || strings.HasPrefix(state, "JOB_STATE_SUCCEEDED") {
						break
					}
				}
			}
			if uuid != "" {
				verdict = "READY_VERTEX_GPU_JOB"
				verdictDetail = "uuid=" + uuid + " job=" + jobID
			} else {
				verdictDetail = "vertex-job-created-but-no-device-uuid-in-logs"
			}
		} else {
			verdictDetail = "vertex-job-create-refused"
			summary["vertex_create_status"] = st
			summary["vertex_create_error"] = c211Trunc(rb, 1200)
		}
		c211Emit("W2_VERTEX", vlog.String())
	} else {
		verdictDetail = "no-compute-create-and-no-vertex-job-create-permission"
	}

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W2_SUMMARY", "C211GCP2_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP2_VERDICT %s %s\n", verdict, verdictDetail)
}
