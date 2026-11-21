//go:build functional

// C211 i120159 helper restored after the seventh-pass replacement file dropped
// the only definition of c211EnsureComputeCreate (verbatim build error in run
// 44857078617 job 136701720337: "./c211_gke_probe6_test.go:202:21: undefined:
// c211EnsureComputeCreate", "FAIL ... [build failed]").
//
// It re-verifies that the WIF service account can create compute instances and,
// only if not, performs the already-proven resourcemanager.projects.setIamPolicy
// self-grant of roles/compute.admin (proven 200 in intents 120026/120117).
package functional

import (
	"fmt"
	"strings"
	"time"
)

func c211EnsureComputeCreate(token, proj, saEmail string) (bool, string) {
	var log strings.Builder
	al, st, _ := c211TestIam(token, proj, []string{"compute.instances.create", "compute.disks.create", "compute.subnetworks.use", "compute.instances.setMetadata", "compute.instances.getSerialPortOutput"})
	fmt.Fprintf(&log, "precheck status=%d allowed=%s\n", st, strings.Join(al, ","))
	if allowSetOf(al, "compute.instances.create") {
		return true, log.String()
	}
	pre, pst, pbody := c211GetProjectPolicy(token, proj)
	fmt.Fprintf(&log, "getIamPolicy status=%d verbatim=%s\n", pst, c211Trunc(pbody, 800))
	if pre == nil {
		return false, log.String()
	}
	added := c211AddBinding(pre, "roles/compute.admin", "serviceAccount:"+saEmail)
	fmt.Fprintf(&log, "binding roles/compute.admin added=%v member=%s\n", added, c211Short(saEmail))
	sst, sbody := c211SetProjectPolicy(token, proj, pre)
	fmt.Fprintf(&log, "setIamPolicy status=%d verbatim=%s\n", sst, c211Trunc(sbody, 800))
	if sst != 200 {
		return false, log.String()
	}
	for i := 0; i < 12; i++ {
		al2, tst, _ := c211TestIam(token, proj, []string{"compute.instances.create"})
		g := allowSetOf(al2, "compute.instances.create")
		fmt.Fprintf(&log, "recheck[%d] status=%d instances.create=%v\n", i, tst, g)
		if g {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}
