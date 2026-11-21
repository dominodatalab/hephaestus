//go:build functional

// C211 i120026 GKE/GCP Vertex AI write-path probe (third pass; reuses the
// helpers of c211_gke_probe_test.go / c211_gke_probe2_test.go in this package).
//
// The second pass invalidated its own permission test: Cloud Resource Manager
// projects.testIamPermissions rejects the WHOLE batch with INVALID_ARGUMENT when
// any single permission name is not valid for the resource
// ("aiplatform.notebookRuntimes.create is not valid for this resource"), so the
// empty ALLOWED set said nothing about aiplatform.customJobs.create.
//
// This pass therefore calls testIamPermissions ONE PERMISSION AT A TIME with
// names that are valid on a project, records each verdict verbatim, and then:
//   - if aiplatform.customJobs.create is granted: submit EXACTLY ONE Vertex AI
//     CustomJob with one NVIDIA_TESLA_T4 running `nvidia-smi -L`, poll it to a
//     terminal state, read its container output back from Cloud Logging, then
//     cancel (if still running) and DELETE it and prove absence with a GET 404;
//   - else if aiplatform.pipelineJobs.create is granted: the same single GPU
//     task as a one-task PipelineJob;
//   - else: exact GATE with every verbatim refusal.
// No secret value is printed; tokens are reported as length + sha256 prefix.
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

// c211MintAccessToken performs the full WIF chain with the Go standard library
// and falls back to gcloud ADC only if the standard-library path fails.
func c211MintAccessToken() (string, string, string) {
	creds, credLog := c211LoadCreds()
	if creds == nil {
		return "", "", "credential file not found: " + credLog
	}
	var log strings.Builder
	fmt.Fprintf(&log, "creds=%s\n", credLog)
	subject, subjMsg := c211FetchSubjectToken(creds)
	fmt.Fprintf(&log, "subject: %s\n", subjMsg)
	if subject != "" {
		if fed, stsMsg := c211STSExchange(creds, subject); fed != "" {
			fmt.Fprintf(&log, "sts: %s\n", stsMsg)
			if final, impMsg := c211Impersonate(creds, fed); final != "" {
				fmt.Fprintf(&log, "impersonate: %s\n", impMsg)
				return final, "stdlib_wif_sts", log.String()
			} else {
				fmt.Fprintf(&log, "impersonate FAILED: %s\n", impMsg)
			}
		} else {
			fmt.Fprintf(&log, "sts FAILED: %s\n", stsMsg)
		}
	}
	tok, gMsg := c211GcloudToken()
	fmt.Fprintf(&log, "gcloud fallback: %s\n", gMsg)
	if tok != "" {
		return tok, "gcloud_adc", log.String()
	}
	return "", "", log.String()
}

func c211NumOrStr(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return c211FmtFloat(x)
	case nil:
		return "<nil>"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// c211VertexWorkerPool builds the single-GPU worker pool used by both the
// CustomJob and the PipelineJob fallback.
func c211VertexWorkerPool() map[string]interface{} {
	return map[string]interface{}{
		"machineSpec": map[string]interface{}{
			"machineType":      "n1-standard-4",
			"acceleratorType":  "NVIDIA_TESLA_T4",
			"acceleratorCount": 1,
		},
		"replicaCount": 1,
		"containerSpec": map[string]interface{}{
			"imageUri": "nvidia/cuda:12.2.0-base-ubuntu22.04",
			"command":  []string{"/bin/sh", "-c"},
			"args": []string{
				"echo C211_VERTEX_START; " +
					"nvidia-smi -L; " +
					"nvidia-smi --query-gpu=uuid,name,driver_version,memory.total --format=csv,noheader; " +
					"echo C211_VERTEX_DONE",
			},
		},
	}
}

var c211GPUUUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// c211ReadVertexLogs reads Cloud Logging entries for a Vertex job and returns
// (uuid, verbatimLog, status, filter).
func c211ReadVertexLogs(token, proj, jobID, jobName string) (string, string, int, string) {
	filters := []string{
		fmt.Sprintf(`resource.type="ml_job" AND labels.job_id="%s"`, jobID),
		fmt.Sprintf(`logName="projects/%s/logs/ml.googleapis.com%%2F%s"`, proj, jobID),
		fmt.Sprintf(`logName:"ml.googleapis.com" AND labels.job_id="%s"`, jobID),
	}
	var all strings.Builder
	lastStatus := 0
	lastFilter := ""
	uuid := ""
	for _, f := range filters {
		lastFilter = f
		for attempt := 0; attempt < 3; attempt++ {
			lb, _ := json.Marshal(map[string]interface{}{
				"resourceNames": []string{"projects/" + proj},
				"filter":        f,
				"pageSize":      100,
				"orderBy":       "timestamp asc",
			})
			st, body := c211HTTP("POST", "https://logging.googleapis.com/v2/entries:list", token, "application/json", string(lb))
			lastStatus = st
			if uuid == "" {
				uuid = c211GPUUUIDRe.FindString(body)
			}
			fmt.Fprintf(&all, "FILTER(%d) %s\nHTTP %d body=%s\n", attempt, f, st, c211Trunc(body, 6000))
			if st == 200 && (uuid != "" || strings.Contains(body, "C211_VERTEX_DONE")) {
				return uuid, all.String(), st, f
			}
			if st == 200 && body == "" {
				break // empty page, next filter
			}
			if st != 200 {
				break // permission/argument error: next filter
			}
			time.Sleep(10 * time.Second)
		}
		if uuid != "" || strings.Contains(all.String(), "C211_VERTEX_DONE") {
			break
		}
	}
	return uuid, all.String(), lastStatus, lastFilter
}

func TestC211GCPVertexJobProbe(t *testing.T) {
	started := time.Now()
	summary := map[string]interface{}{}
	verdict := "GATE"
	verdictDetail := ""

	token, method, authLog := c211MintAccessToken()
	c211Emit("W3_AUTH", authLog)
	summary["auth_method"] = method
	summary["access_token"] = c211Short(token)
	if token == "" {
		summary["verdict"] = "GATE"
		c211Emit("W3_SUMMARY", "C211GCP3_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP3_VERDICT GATE WIF_TOKEN_EXCHANGE_FAILED")
		return
	}

	st, ti := c211HTTP("GET", "https://oauth2.googleapis.com/tokeninfo", token, "", "")
	fmt.Fprintf(os.Stdout, "tokeninfo status=%d %s\n", st, c211Trunc(ti, 600))
	if m := c211JSON(ti); m != nil {
		summary["tokeninfo_status"] = st
		summary["tokeninfo_azp"] = c211Str(m, "azp")
		summary["tokeninfo_scope"] = c211Str(m, "scope")
	}

	creds, _ := c211LoadCreds()
	proj := ""
	if creds != nil {
		proj = c211ResolveProject(creds)
	}
	if proj == "" {
		proj = c211FirstNonEmpty(os.Getenv("GCP_PROJECT_ID"), os.Getenv("GCP_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if proj == "" {
		summary["verdict"] = "GATE"
		c211Emit("W3_SUMMARY", "C211GCP3_SUMMARY_JSON "+c211JSONString(summary))
		fmt.Println("C211GCP3_VERDICT GATE NO_PROJECT_ID_RESOLVED")
		return
	}
	summary["project_id_short"] = c211Short(proj)
	fmt.Printf("\nC211GCP3 project=%s (%s)\n", proj, c211Short(proj))

	// ---------------- per-permission testIamPermissions ----------------
	perms := []string{
		"aiplatform.customJobs.create",
		"aiplatform.customJobs.get",
		"aiplatform.customJobs.cancel",
		"aiplatform.customJobs.list",
		"aiplatform.locations.list",
		"aiplatform.pipelineJobs.create",
		"aiplatform.batchPredictionJobs.create",
		"aiplatform.endpoints.create",
		"aiplatform.models.upload",
		"aiplatform.trainingPipelines.create",
		"aiplatform.schedules.create",
		"logging.logEntries.list",
		"logging.privateLogEntries.list",
		"compute.instances.create",
		"iam.serviceAccounts.actAs",
		"resourcemanager.projects.get",
	}
	allowSet := map[string]bool{}
	invalidSet := map[string]bool{}
	var ilog strings.Builder
	allowedList := []string{}
	for _, p := range perms {
		al, pst, pbody := c211TestIam(token, proj, []string{p})
		granted := false
		for _, a := range al {
			if a == p {
				granted = true
			}
		}
		if granted {
			allowSet[p] = true
			allowedList = append(allowedList, p)
		}
		if pst == 400 && strings.Contains(pbody, "not valid for this resource") {
			invalidSet[p] = true
		}
		fmt.Fprintf(&ilog, "PERM %s status=%d granted=%v allowed=%s verbatim=%s\n",
			p, pst, granted, strings.Join(al, ","), c211Trunc(pbody, 500))
	}
	sort.Strings(allowedList)
	c211Emit("W3_TESTIAM", ilog.String())
	summary["allowed"] = allowedList
	summary["allowed_count"] = len(allowedList)
	summary["testiam_per_call"] = true
	fmt.Printf("C211GCP3 allowed(%d)=%s\n", len(allowedList), strings.Join(allowedList, ","))

	// ---------------- exactly one GPU job ----------------
	if allowSet["aiplatform.customJobs.create"] || allowSet["aiplatform.pipelineJobs.create"] {
		region := "us-central1"
		usePipeline := !allowSet["aiplatform.customJobs.create"]
		var vlog strings.Builder
		jobName := ""
		httpCreate := 0
		createBody := ""
		if !usePipeline {
			jobID := "c211-i120026-t4"
			body := map[string]interface{}{
				"displayName": "c211-i120026-vertex-gpu-t4",
				"labels":      map[string]string{"c211": "i120026"},
				"jobSpec": map[string]interface{}{
					"workerPoolSpecs": []interface{}{c211VertexWorkerPool()},
				},
			}
			jb, _ := json.Marshal(body)
			u := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + url.PathEscape(proj) +
				"/locations/" + region + "/customJobs?customJobId=" + url.QueryEscape(jobID)
			st, rb := c211HTTP("POST", u, token, "application/json", string(jb))
			httpCreate, createBody = st, rb
			jobName = "projects/" + proj + "/locations/" + region + "/customJobs/" + jobID
			fmt.Fprintf(&vlog, "CUSTOMJOB create status=%d url=%s\nverbatim=%s\n", st, u, c211Trunc(rb, 3000))
		} else {
			jobID := "c211-i120026-pipe-t4"
			body := map[string]interface{}{
				"displayName": "c211-i120026-pipe-gpu-t4",
				"pipelineSpec": map[string]interface{}{
					"pipelineInfo": map[string]interface{}{"name": "c211-i120026-pipe"},
					"root": map[string]interface{}{
						"dag": map[string]interface{}{
							"tasks": map[string]interface{}{
								"gpu": map[string]interface{}{
									"taskSpec": map[string]interface{}{
										"workerPoolSpecs": []interface{}{c211VertexWorkerPool()},
									},
								},
							},
						},
					},
				},
			}
			jb, _ := json.Marshal(body)
			u := "https://" + region + "-aiplatform.googleapis.com/v1/projects/" + url.PathEscape(proj) +
				"/locations/" + region + "/pipelineJobs?pipelineJobId=" + url.QueryEscape(jobID)
			st, rb := c211HTTP("POST", u, token, "application/json", string(jb))
			httpCreate, createBody = st, rb
			jobName = "projects/" + proj + "/locations/" + region + "/pipelineJobs/" + jobID
			fmt.Fprintf(&vlog, "PIPELINEJOB create status=%d url=%s\nverbatim=%s\n", st, u, c211Trunc(rb, 3000))
		}
		summary["vertex_create_status"] = httpCreate
		summary["vertex_create_error"] = c211Trunc(createBody, 1500)
		summary["vertex_job_name"] = jobName

		if httpCreate == 200 || httpCreate == 409 {
			jobID := jobName[strings.LastIndex(jobName, "/")+1:]
			terminal := false
			jobState := ""
			for i := 0; i < 36; i++ {
				st, body := c211HTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
				jobState = ""
				if st == 200 {
					jobState = c211Str(c211JSON(body), "state")
				}
				fmt.Fprintf(&vlog, "JOB poll=%d http=%d state=%s\n", i, st, jobState)
				if st != 200 {
					fmt.Fprintf(&vlog, "JOB poll verbatim=%s\n", c211Trunc(body, 800))
				}
				if strings.HasPrefix(jobState, "JOB_STATE_SUCCEEDED") || strings.HasPrefix(jobState, "JOB_STATE_FAILED") ||
					strings.HasPrefix(jobState, "JOB_STATE_CANCELLED") || strings.HasPrefix(jobState, "JOB_STATE_EXPIRED") ||
					strings.HasPrefix(jobState, "PIPELINE_STATE_SUCCEEDED") || strings.HasPrefix(jobState, "PIPELINE_STATE_FAILED") ||
					strings.HasPrefix(jobState, "PIPELINE_STATE_CANCELLED") {
					terminal = true
					if jm := c211JSON(body); jm != nil {
						if errv, ok := jm["error"]; ok {
							eb, _ := json.Marshal(errv)
							fmt.Fprintf(&vlog, "JOB error=%s\n", c211Trunc(string(eb), 1500))
							summary["vertex_job_error"] = c211Trunc(string(eb), 1200)
						}
					}
					break
				}
				time.Sleep(15 * time.Second)
			}
			summary["vertex_job_state"] = jobState
			summary["vertex_terminal"] = terminal

			if !terminal {
				cst, cb := c211HTTP("POST", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName+":cancel", token, "application/json", "{}")
				fmt.Fprintf(&vlog, "JOB cancel status=%d verbatim=%s\n", cst, c211Trunc(cb, 1000))
				summary["vertex_cancel_status"] = cst
				for i := 0; i < 8; i++ {
					time.Sleep(10 * time.Second)
					st, body := c211HTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
					jobState = ""
					if st == 200 {
						jobState = c211Str(c211JSON(body), "state")
					}
					fmt.Fprintf(&vlog, "JOB cancel_readback=%d http=%d state=%s\n", i, st, jobState)
					if strings.Contains(jobState, "CANCELLED") || strings.Contains(jobState, "FAILED") || strings.Contains(jobState, "SUCCEEDED") {
						break
					}
				}
				summary["vertex_job_state_after_cancel"] = jobState
			}

			uuid, lv, lst, lfilter := c211ReadVertexLogs(token, proj, jobID, jobName)
			fmt.Fprintf(&vlog, "LOGGING last_status=%d last_filter=%s\n%s\n", lst, lfilter, lv)
			summary["vertex_log_status"] = lst
			summary["vertex_log_filter"] = lfilter
			summary["vertex_gpu_uuid"] = uuid
			summary["vertex_log_done_marker"] = strings.Contains(lv, "C211_VERTEX_DONE")

			// delete and prove absence
			dst, db := c211HTTP("DELETE", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
			fmt.Fprintf(&vlog, "JOB delete status=%d verbatim=%s\n", dst, c211Trunc(db, 1200))
			summary["vertex_delete_status"] = dst
			absent := false
			for i := 0; i < 8; i++ {
				time.Sleep(8 * time.Second)
				st, body := c211HTTP("GET", "https://"+region+"-aiplatform.googleapis.com/v1/"+jobName, token, "", "")
				fmt.Fprintf(&vlog, "JOB absence_readback=%d http=%d verbatim=%s\n", i, st, c211Trunc(body, 500))
				if st == 404 {
					absent = true
					break
				}
			}
			summary["vertex_absent_after_delete"] = absent

			if uuid != "" {
				verdict = "READY_VERTEX_GPU_JOB"
				verdictDetail = "uuid=" + uuid + " job=" + jobName + " state=" + jobState
			} else {
				verdict = "GATE"
				verdictDetail = "vertex-job-created-state=" + jobState + "-but-no-gpu-uuid-in-logs"
			}
		} else {
			verdict = "GATE"
			verdictDetail = "vertex-job-create-refused"
		}
		c211Emit("W3_VERTEX", vlog.String())
	} else {
		verdictDetail = "customJobs.create-and-pipelineJobs.create-denied"
	}

	summary["verdict"] = verdict
	summary["elapsed_seconds"] = int(time.Since(started).Seconds())
	c211Emit("W3_SUMMARY", "C211GCP3_SUMMARY_JSON "+c211JSONString(summary))
	fmt.Printf("C211GCP3_VERDICT %s %s\n", verdict, verdictDetail)
}
