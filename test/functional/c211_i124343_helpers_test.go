-----------------------------
// Main probe
// ---------------------------------------------------------------------------


func c211xContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func c211xPolicyHas(pol map[string]interface{}, role, member string) bool {
	if pol == nil {
		return false
	}
	bindings, _ := pol["bindings"].([]interface{})
	for _, b := range bindings {
		bm, _ := b.(map[string]interface{})
		if c211xStr(bm, "role") != role {
			continue
		}
		members, _ := bm["members"].([]interface{})
		for _, m := range members {
			if s, ok := m.(string); ok && s == member {
				return true
			}
		}
	}
	return false
}

func c211xGetPolicyBody(token, resourceURL string) (int, string) {
	body := `{"options":{"requestedPolicyVersion":3}}`
	st, rb := c211xHTTP("POST", resourceURL+":getIamPolicy", token, "application/json", body)
	return st, rb
}

func c211xListRegions(token, proj string) ([]string, int, string) {
	u := "https://compute.googleapis.com/compute/v1/projects/" + url.PathEscape(proj) + "/regions?maxResults=100&fields=items/name"
	st, body := c211xHTTP("GET", u, token, "", "")
	out := []string{}
	if st != 200 {
		return out, st, body
	}
	m := c211xJSON(body)
	if items, ok := m["items"].([]interface{}); ok {
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if n := c211xStr(im, "name"); n != "" {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, st, body
}
