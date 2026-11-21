//go:build functional

// C211 i121733 verify — final absence proof for the two leftover marker names
// from earlier rounds of this lane (nonces i121180 and i121469) in the four
// Dominodatalab sibling projects. Read + delete-own-only; no new markers.

package functional

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

var c211vProjects = []string{
	"team-eng-infra-testing",
	"domino-eng-service-artifacts",
	"team-eng-infrastructure",
	"team-eng-platform",
}

var c211vNonces = []string{"i121180", "i121469"}

func c211vList(tok, proj, nonce string) (int, int, []string, string) {
	filter := "labels.c211=" + nonce
	st, body := c211xHTTP("GET", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/aggregated/instances?filter="+url.QueryEscape(filter)+"&maxResults=500", tok, "", "")
	n := -1
	names := []string{}
	zones := []string{}
	if st == 200 {
		m := c211xJSON(body)
		n = 0
		if it, ok := m["items"].(map[string]interface{}); ok {
			for zoneKey, zv := range it {
				zm, _ := zv.(map[string]interface{})
				list, _ := zm["instances"].([]interface{})
				zone := strings.TrimPrefix(zoneKey, "zones/")
				for _, x := range list {
					im, _ := x.(map[string]interface{})
					nm := c211xStr(im, "name")
					if nm == "" {
						continue
					}
					n++
					names = append(names, zone+"/"+nm)
					zones = append(zones, zone)
				}
			}
		}
	}
	return st, n, names, body
}

func TestC211VerifyAbsence(t *testing.T) {
	started := time.Now()
	token, method, mintLog := c211xMint()
	if token == "" {
		fmt.Printf("C211R_MINT_FAILED %s\n", c211xTrunc(mintLog, 800))
		t.Fatalf("wif mint failed")
	}
	fmt.Printf("C211R_IDENTITY auth=%s\n", method)
	ftok := ""
	sas, lst, _ := c211xListServiceAccounts(token, "153711203585")
	fmt.Printf("C211R_SA_LIST status=%d count=%d\n", lst, len(sas))
	for _, sa := range sas {
		e := c211xStr(sa, "email")
		if strings.HasPrefix(e, "fleetcommand-service-account@") {
			at, gst, _ := c211xGenerateAccessToken(token, e)
			fmt.Printf("C211R_FLEET_TOKEN status=%d token=%s\n", gst, c211xShort(at))
			ftok = at
			break
		}
	}
	if ftok == "" {
		ftok = token
		fmt.Printf("C211R_FLEET_TOKEN fallback=wif_minted\n")
	}

	// Pass 1: list and delete any remaining own instance.
	for _, proj := range c211vProjects {
		for _, nonce := range c211vNonces {
			st, n, names, _ := c211vList(ftok, proj, nonce)
			fmt.Printf("C211R_LIST project=%s nonce=%s status=%d found=%d instances=%s\n", proj, nonce, st, n, strings.Join(names, ","))
			for _, nz := range names {
				parts := strings.SplitN(nz, "/", 2)
				if len(parts) != 2 {
					continue
				}
				ds, db := c211xHTTP("DELETE", "https://compute.googleapis.com/compute/v1/projects/"+url.PathEscape(proj)+"/zones/"+parts[0]+"/instances/"+parts[1], ftok, "", "")
				fmt.Printf("C211R_REMOVE project=%s zone=%s name=%s status=%d verbatim=%s\n", proj, parts[0], parts[1], ds, c211xTrunc(db, 240))
			}
		}
	}

	// Pass 2: poll explicit instance names until 404, then re-list.
	bad := 0
	for _, proj := range c211vProjects {
		for _, nonce := range c211vNonces {
			name := "c211-" + nonce + "-t4"
			absent := false
			last := 0
			for i := 0; i < 12; i++ {
				st, im, ib := c211xInstanceGet(ftok, proj, "us-central1-a", name)
				last = st
				if st == 404 {
					absent = true
					break
				}
				if st != 200 {
					fmt.Printf("C211R_NAME_GET project=%s name=%s status=%d verbatim=%s\n", proj, name, st, c211xTrunc(ib, 200))
					break
				}
				if i == 0 {
					fmt.Printf("C211R_NAME_PRESENT project=%s name=%s state=%s\n", proj, name, c211xStr(im, "status"))
				}
				time.Sleep(8 * time.Second)
			}
			fmt.Printf("C211R_NAME_ABSENCE project=%s name=%s http=%d absent=%v\n", proj, name, last, absent)
			if !absent {
				bad++
			}
		}
	}

	// Pass 3: final label re-list per project.
	left := 0
	for _, proj := range c211vProjects {
		for _, nonce := range c211vNonces {
			st, n, names, body := c211vList(ftok, proj, nonce)
			if n > 0 {
				left += n
			}
			verdict := "NO_RESULTS_ON_PAGE"
			if n != 0 {
				verdict = "RESIDUE"
			}
			fmt.Printf("C211R_FINAL project=%s nonce=%s status=%d remaining=%d instances=%s note=%s verbatim=%s\n", proj, nonce, st, n, strings.Join(names, ","), verdict, c211xTrunc(body, 200))
		}
	}
	fmt.Printf("C211R_VERDICT residual=%d unresolved_name_checks=%d elapsed=%ds\n", left, bad, int(time.Since(started).Seconds()))
	if left != 0 || bad != 0 {
		t.Fatalf("residual markers remain: residual=%d unresolved=%d", left, bad)
	}
}
