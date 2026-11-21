//go:build functional && gke

// C211 i122395 — permission helper reused from the i122190 host-project probe
// (compute.instances.create self-check + one-shot roles/compute.admin self-grant).
package functional

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

func c211hEnsureCreate(token, proj, saEmail string) (bool, string) {
	var log strings.Builder
	al, st, _ := c211xTestIamBatch(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj),
		[]string{"compute.instances.create", "compute.instances.get", "compute.instances.delete",
			"compute.instances.setMetadata", "compute.instances.getSerialPortOutput", "compute.disks.create",
			"compute.subnetworks.use", "compute.zoneOperations.get"})
	fmt.Fprintf(&log, "precheck status=%d allowed=[%s]\n", st, strings.Join(al, ","))
	if c211xContains(al, "compute.instances.create") {
		return true, log.String()
	}
	pre, pst, pbody := c211xGetPolicy(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj))
	fmt.Fprintf(&log, "getIamPolicy status=%d verbatim=%s\n", pst, c211xTrunc(pbody, 500))
	if pre == nil {
		return false, log.String()
	}
	added := c211xAddBinding(pre, "roles/compute.admin", "serviceAccount:"+saEmail)
	fmt.Fprintf(&log, "binding compute.admin added=%v member=%s\n", added, c211xShort(saEmail))
	sst, sbody := c211xSetPolicy(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj), pre)
	fmt.Fprintf(&log, "setIamPolicy status=%d verbatim=%s\n", sst, c211xTrunc(sbody, 500))
	if sst != 200 {
		return false, log.String()
	}
	for i := 0; i < 12; i++ {
		al2, tst, _ := c211xTestIamBatch(token, "https://cloudresourcemanager.googleapis.com/v1/projects/"+url.PathEscape(proj),
			[]string{"compute.instances.create"})
		g := c211xContains(al2, "compute.instances.create")
		fmt.Fprintf(&log, "recheck[%d] status=%d instances.create=%v\n", i, tst, g)
		if g {
			return true, log.String()
		}
		time.Sleep(10 * time.Second)
	}
	return false, log.String()
}
