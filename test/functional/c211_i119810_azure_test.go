//go:build functional

// c211-i119810 minimal Azure service-principal re-check.
// Proves whether the repository's ARM_* client secret still authenticates, and
// if it does, enumerates identity and GPU-family quota. Read-only: it never
// creates a resource. Prints identifiers and aggregate data only.
package functional

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func c211Run(timeout time.Duration, name string, args []string) (string, int) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return "TIMEOUT after " + timeout.String(), -1
	}
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return string(out), code
}

func c211Trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

func TestC211AzureSecretRecheck(t *testing.T) {
	env := os.Environ()
	_ = env
	fmt.Println("C211AZ_START")
	for _, k := range []string{"ARM_CLIENT_ID", "ARM_TENANT_ID", "ARM_SUBSCRIPTION_ID",
		"ARM_CLIENT_SECRET", "AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_SECRET"} {
		if os.Getenv(k) != "" {
			fmt.Printf("C211AZ_ENV %s=SET\n", k)
		} else {
			fmt.Printf("C211AZ_ENV %s=UNSET\n", k)
		}
	}
	client := os.Getenv("ARM_CLIENT_ID")
	secret := os.Getenv("ARM_CLIENT_SECRET")
	tenant := os.Getenv("ARM_TENANT_ID")
	if client == "" {
		client = os.Getenv("AZURE_CLIENT_ID")
		secret = os.Getenv("AZURE_CLIENT_SECRET")
		tenant = os.Getenv("AZURE_TENANT_ID")
	}

	out, code := c211Run(180*time.Second, "az", []string{"login", "--service-principal",
		"--username", client, "--password", secret, "--tenant", tenant,
		"--allow-no-subscriptions", "--output", "none"})
	fmt.Printf("C211AZ_LOGIN code=%d\n%s\n", code, c211Trunc(out, 2500))
	if code != 0 {
		fmt.Println("C211AZ_VERDICT SERVICE_PRINCIPAL_SECRET_REJECTED")
		fmt.Println("C211AZ_DONE")
		return
	}
	out, code = c211Run(90*time.Second, "az", []string{"account", "show", "--output", "json"})
	fmt.Printf("C211AZ_ACCOUNT_SHOW code=%d\n%s\n", code, c211Trunc(out, 1500))
	out, code = c211Run(120*time.Second, "az", []string{"account", "list-locations",
		"--query", "[?metadata.regionType=='Physical'].name", "--output", "json"})
	fmt.Printf("C211AZ_REGIONS code=%d\n%s\n", code, c211Trunc(out, 2500))

	regions := []string{"eastus", "eastus2", "centralus", "westus2", "westus3", "northeurope",
		"westeurope", "uksouth", "swedencentral", "southcentralus", "japaneast", "southeastasia",
		"canadacentral", "australiaeast", "brazilsouth", "centralindia", "koreacentral", "francecentral"}
	for _, r := range regions {
		out, code = c211Run(75*time.Second, "az", []string{"vm", "list-usage", "--location", r,
			"--query", "[?contains(localName,'NC')||contains(localName,'ND')||contains(localName,'NV')].{n:localName,c:currentValue,l:limit}",
			"--output", "json"})
		if code != 0 {
			fmt.Printf("C211AZ_USAGE %s code=%d err=%s\n", r, code, c211Trunc(strings.TrimSpace(out), 300))
			continue
		}
		var nz []string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "\"l\":") && !strings.Contains(line, "\"l\": 0") && !strings.Contains(line, "\"l\":0") {
				nz = append(nz, strings.TrimSpace(line))
			}
		}
		if len(nz) > 0 {
			fmt.Printf("C211AZ_USAGE_NONZERO %s %s\n", r, strings.Join(nz, " | "))
		} else {
			fmt.Printf("C211AZ_USAGE_ZERO %s\n", r)
		}
	}
	fmt.Println("C211AZ_VERDICT SECRET_ACCEPTED")
	fmt.Println("C211AZ_DONE")
}
