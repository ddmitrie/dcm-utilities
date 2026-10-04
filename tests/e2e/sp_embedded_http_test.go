//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// When a service type is only available as an agent-embedded SP, there is no
// :8082/:8083/:8081 SP HTTP server. Workload calls go through the control
// plane (catalog → agent). OpenAPI-specific SP contract tests still skip.

var (
	embeddedCatalogMu    sync.Mutex
	embeddedCatalogItem  = map[string]string{} // serviceType → catalog item uid
	embeddedCatalogPol   = map[string]string{} // serviceType → policy id
	embeddedSTIToInst    = map[string]string{} // service-type-instance id → catalog-item-instance uid
	embeddedAgentSeenAt  = time.Now()
)

func skipUnlessDirectContainerSP() {
	GinkgoHelper()
	initContainerSP()
	if containerSPReady {
		return
	}
	Skip("standalone container SP HTTP contract required (not exposed when the SP is agent-embedded)")
}

func skipUnlessDirectKubevirtSP() {
	GinkgoHelper()
	initKubevirtSP()
	if kubevirtStandaloneReady {
		return
	}
	Skip("standalone KubeVirt SP HTTP contract required (not exposed when vm is agent-embedded)")
}

func skipUnlessDirectAcmClusterSP() {
	GinkgoHelper()
	initAcmClusterSP()
	if acmClusterSPReady {
		return
	}
	Skip("standalone ACM Cluster SP HTTP contract required (not exposed when cluster is agent-embedded)")
}

func jsonHTTPResponse(status int, payload interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(status)
	_, _ = rec.Write(body)
	return rec.Result(), nil
}

func emptyHTTPResponse(status int) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(status)
	return rec.Result(), nil
}

func splitSPPath(path string) (clean string, query url.Values) {
	u, err := url.Parse(path)
	if err != nil {
		return path, url.Values{}
	}
	return u.Path, u.Query()
}

func stiStatusAsSP(status string) string {
	if status == "" {
		return status
	}
	return strings.ToUpper(status)
}

func firstGlobalPolicyID() string {
	resp, err := doRequest(http.MethodGet, "/policies?max_page_size=100", "")
	if err != nil {
		return ""
	}
	var body map[string]interface{}
	decodeJSON(resp, &body)
	pols, _ := body["policies"].([]interface{})
	for _, raw := range pols {
		p, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		pt, _ := p["policy_type"].(string)
		id, _ := p["id"].(string)
		if strings.EqualFold(pt, "GLOBAL") && id != "" {
			return id
		}
	}
	return ""
}

func ensureEmbeddedCatalogRoute(serviceType string) (catalogItemID, agentName string) {
	GinkgoHelper()
	embeddedCatalogMu.Lock()
	defer embeddedCatalogMu.Unlock()

	if id, ok := embeddedCatalogItem[serviceType]; ok && id != "" {
		return id, discoverAgentByServiceType(serviceType, "")
	}

	agentName = discoverAgentByServiceType(serviceType, "")
	// Control plane allows only one GLOBAL policy per priority. Reuse an
	// existing GLOBAL route (typically local-agent) instead of 409ing.
	polID := firstGlobalPolicyID()
	if polID == "" {
		pkg := fmt.Sprintf("e2e_embed_%s_%d", strings.ReplaceAll(serviceType, "-", "_"), time.Now().UnixNano())
		polName := uniqueName("e2e-embed-pol-" + serviceType)
		polPayload := fmt.Sprintf(`{
		"display_name": %q,
		"policy_type": "GLOBAL",
		"priority": 50,
		"description": "E2E embedded SP route",
		"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
	}`, polName, pkg, agentName)
		resp, err := doRequest(http.MethodPost, "/policies", polPayload)
		Expect(err).NotTo(HaveOccurred())
		if resp.StatusCode == http.StatusConflict {
			_ = resp.Body.Close()
			polID = firstGlobalPolicyID()
		} else {
			Expect(resp.StatusCode).To(Equal(http.StatusCreated), "create routing policy for embedded %s", serviceType)
			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			polID, _ = polBody["id"].(string)
		}
	}
	Expect(polID).NotTo(BeEmpty(), "need a GLOBAL routing policy for embedded %s", serviceType)
	embeddedCatalogPol[serviceType] = polID

	catName := uniqueName("e2e-embed-cat-" + serviceType)
	var catPayload string
	switch serviceType {
	case "vm":
		catPayload = fmt.Sprintf(`{
			"api_version": "v1alpha1",
			"display_name": %q,
			"spec": {"resources": [{"name": "main", "service_type": "vm", "fields": [
				{"path": "metadata.name", "display_name": "Name", "editable": true, "default": %q},
				{"path": "guest_os.type", "editable": true, "default": "linux"},
				{"path": "vcpu.count", "editable": false, "default": 1},
				{"path": "memory.size", "editable": false, "default": "1GB"},
				{"path": "storage.disks", "editable": false, "default": [{"name":"boot","capacity":"10GB"}]}
			]}]}
		}`, catName, catName)
	case "cluster":
		catPayload = fmt.Sprintf(`{
			"api_version": "v1alpha1",
			"display_name": %q,
			"spec": {"resources": [{"name": "main", "service_type": "cluster", "fields": [
				{"path": "metadata.name", "display_name": "Name", "editable": true, "default": %q}
			]}]}
		}`, catName, catName)
	default:
		catPayload = fmt.Sprintf(`{
			"api_version": "v1alpha1",
			"display_name": %q,
			"spec": {"resources": [{"name": "main", "service_type": "container", "fields": [
				{"path": "metadata.name", "display_name": "Container Name", "editable": true, "default": %q},
				{"path": "image.reference", "display_name": "Image", "editable": true, "default": "docker.io/library/nginx:alpine"},
				{"path": "resources.cpu.min", "editable": false, "default": "1"},
				{"path": "resources.cpu.max", "editable": false, "default": "1"},
				{"path": "resources.memory.min", "editable": false, "default": "128MB"},
				{"path": "resources.memory.max", "editable": false, "default": "256MB"}
			]}]}
		}`, catName, catName)
	}

	resp, err := doRequest(http.MethodPost, "/catalog-items", catPayload)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated), "create catalog item for embedded %s", serviceType)
	var catBody map[string]interface{}
	decodeJSON(resp, &catBody)
	catalogItemID, _ = catBody["uid"].(string)
	Expect(catalogItemID).NotTo(BeEmpty())
	embeddedCatalogItem[serviceType] = catalogItemID
	return catalogItemID, agentName
}

func createEmbeddedInstance(serviceType, displayName string, userValues []map[string]interface{}) (resourceID, instanceID string, status int, raw map[string]interface{}) {
	GinkgoHelper()
	catalogItemID, _ := ensureEmbeddedCatalogRoute(serviceType)
	if displayName == "" {
		displayName = uniqueName("e2e-embed-inst")
	}
	valuesJSON, err := json.Marshal(userValues)
	Expect(err).NotTo(HaveOccurred())
	payload := fmt.Sprintf(`{
		"api_version": "v1alpha1",
		"display_name": %q,
		"spec": {
			"catalog_item_id": %q,
			"user_values": %s
		}
	}`, displayName, catalogItemID, string(valuesJSON))

	before := listServiceTypeInstanceIDs()
	resp, err := doRequest(http.MethodPost, "/catalog-item-instances", payload)
	Expect(err).NotTo(HaveOccurred())
	status = resp.StatusCode
	raw = map[string]interface{}{}
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(data, &raw)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", "", status, raw
	}
	instanceID, _ = raw["uid"].(string)
	resourceID = resolveResourceIDAfterCreate(raw, before)
	if resourceID != "" && instanceID != "" {
		embeddedCatalogMu.Lock()
		embeddedSTIToInst[resourceID] = instanceID
		embeddedCatalogMu.Unlock()
	}
	return resourceID, instanceID, status, raw
}

func getSTI(id string) (int, map[string]interface{}) {
	resp, err := doRequest(http.MethodGet, "/service-type-instances/"+id, "")
	if err != nil {
		return 0, nil
	}
	status := resp.StatusCode
	var body map[string]interface{}
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(data, &body)
	}
	return status, body
}

func listSTIs(serviceType string, query url.Values) (int, map[string]interface{}) {
	q := url.Values{}
	q.Set("service_type", serviceType)
	if v := query.Get("max_page_size"); v != "" {
		q.Set("max_page_size", v)
	}
	if v := query.Get("page_token"); v != "" {
		q.Set("page_token", v)
	}
	resp, err := doRequest(http.MethodGet, "/service-type-instances?"+q.Encode(), "")
	if err != nil {
		return 0, nil
	}
	status := resp.StatusCode
	var body map[string]interface{}
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(data, &body)
	}
	return status, body
}

func deleteEmbeddedResource(resourceID string) int {
	embeddedCatalogMu.Lock()
	instID := embeddedSTIToInst[resourceID]
	embeddedCatalogMu.Unlock()
	path := "/service-type-instances/" + resourceID
	if instID != "" {
		path = "/catalog-item-instances/" + instID
	}
	resp, err := doRequest(http.MethodDelete, path, "")
	if err != nil {
		return 0
	}
	if resp.Body != nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	code := resp.StatusCode
	if code == http.StatusOK || code == http.StatusNoContent || code == http.StatusAccepted {
		return http.StatusNoContent
	}
	return code
}

func embeddedHealthPayload(serviceType string) map[string]interface{} {
	status := "unhealthy"
	if agentEmbeds(serviceType) {
		status = "healthy"
	}
	uptime := time.Since(embeddedAgentSeenAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	return map[string]interface{}{
		"status":  status,
		"type":    "environment-agent.dcm.io/health",
		"path":    "health",
		"version": "embedded",
		"uptime":  uptime,
	}
}

func userValuesFromContainerSpec(spec map[string]interface{}) (displayName string, values []map[string]interface{}) {
	displayName = uniqueName("e2e-embed-ctr")
	image := "docker.io/library/nginx:alpine"
	if meta, ok := spec["metadata"].(map[string]interface{}); ok {
		if n, _ := meta["name"].(string); n != "" {
			displayName = n
		}
	}
	if img, ok := spec["image"].(map[string]interface{}); ok {
		if ref, _ := img["reference"].(string); ref != "" {
			image = ref
		}
	}
	values = []map[string]interface{}{
		{"path": "metadata.name", "value": displayName, "resource": "main"},
		{"path": "image.reference", "value": image, "resource": "main"},
	}
	return displayName, values
}

func userValuesFromVMSpec(spec map[string]interface{}) (displayName string, values []map[string]interface{}) {
	displayName = uniqueName("e2e-embed-vm")
	if meta, ok := spec["metadata"].(map[string]interface{}); ok {
		if n, _ := meta["name"].(string); n != "" {
			displayName = n
		}
	}
	values = []map[string]interface{}{
		{"path": "metadata.name", "value": displayName, "resource": "main"},
	}
	return displayName, values
}

func parseSpecBody(body string) map[string]interface{} {
	var wrapped map[string]interface{}
	if err := json.Unmarshal([]byte(body), &wrapped); err != nil {
		return nil
	}
	spec, _ := wrapped["spec"].(map[string]interface{})
	return spec
}

func doEmbeddedContainerSPRequest(method, path, body string) (*http.Response, error) {
	clean, query := splitSPPath(path)
	switch {
	case method == http.MethodGet && strings.HasSuffix(clean, "/containers/health"):
		payload := embeddedHealthPayload("container")
		payload["path"] = "/api/v1alpha1/health"
		return jsonHTTPResponse(http.StatusOK, payload)
	case method == http.MethodPost && strings.TrimSuffix(clean, "/") == "/containers":
		spec := parseSpecBody(body)
		if spec == nil {
			return jsonHTTPResponse(http.StatusBadRequest, map[string]interface{}{"title": "invalid argument"})
		}
		if _, hasMeta := spec["metadata"]; !hasMeta {
			if _, hasImage := spec["image"]; !hasImage {
				return jsonHTTPResponse(http.StatusBadRequest, map[string]interface{}{"title": "invalid argument"})
			}
		}
		name, values := userValuesFromContainerSpec(spec)
		resourceID, _, status, raw := createEmbeddedInstance("container", name, values)
		if status != http.StatusCreated && status != http.StatusOK {
			return jsonHTTPResponse(status, raw)
		}
		return jsonHTTPResponse(http.StatusCreated, map[string]interface{}{"id": resourceID})
	case method == http.MethodGet && strings.TrimSuffix(clean, "/") == "/containers":
		st, raw := listSTIs("container", query)
		if st != http.StatusOK {
			return jsonHTTPResponse(st, raw)
		}
		items, _ := raw["instances"].([]interface{})
		out := make([]interface{}, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]interface{})
			if !ok {
				continue
			}
			if s, _ := m["status"].(string); s != "" {
				m["status"] = stiStatusAsSP(s)
			}
			out = append(out, m)
		}
		resp := map[string]interface{}{"containers": out}
		if tok, _ := raw["next_page_token"].(string); tok != "" {
			resp["next_page_token"] = tok
		}
		return jsonHTTPResponse(http.StatusOK, resp)
	case method == http.MethodGet && strings.HasPrefix(clean, "/containers/"):
		id := strings.TrimPrefix(clean, "/containers/")
		st, raw := getSTI(id)
		if raw == nil {
			raw = map[string]interface{}{}
		}
		if s, _ := raw["status"].(string); s != "" {
			raw["status"] = stiStatusAsSP(s)
		}
		if _, ok := raw["id"]; !ok {
			raw["id"] = id
		}
		return jsonHTTPResponse(st, raw)
	case method == http.MethodDelete && strings.HasPrefix(clean, "/containers/"):
		id := strings.TrimPrefix(clean, "/containers/")
		return emptyHTTPResponse(deleteEmbeddedResource(id))
	default:
		return nil, fmt.Errorf("unsupported embedded container SP path %s %s", method, path)
	}
}

func doEmbeddedKubevirtRequest(method, path, payload string) (*http.Response, error) {
	clean, query := splitSPPath(path)
	switch {
	case method == http.MethodGet && strings.HasSuffix(clean, "/vms/health"):
		h := embeddedHealthPayload("vm")
		h["path"] = "/api/v1alpha1/health"
		return jsonHTTPResponse(http.StatusOK, h)
	case method == http.MethodPost && strings.TrimSuffix(clean, "/") == "/vms":
		spec := parseSpecBody(payload)
		if spec == nil {
			return jsonHTTPResponse(http.StatusBadRequest, map[string]interface{}{"title": "invalid argument"})
		}
		name, values := userValuesFromVMSpec(spec)
		resourceID, _, status, raw := createEmbeddedInstance("vm", name, values)
		if status != http.StatusCreated && status != http.StatusOK {
			return jsonHTTPResponse(status, raw)
		}
		id := resourceID
		return jsonHTTPResponse(http.StatusCreated, map[string]interface{}{
			"path": "/api/v1alpha1/vms/" + id,
			"spec": spec,
			"id":   id,
		})
	case method == http.MethodGet && strings.TrimSuffix(clean, "/") == "/vms":
		st, raw := listSTIs("vm", query)
		if st != http.StatusOK {
			return jsonHTTPResponse(st, raw)
		}
		items, _ := raw["instances"].([]interface{})
		vms := make([]interface{}, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]interface{})
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			vms = append(vms, map[string]interface{}{
				"path": "/api/v1alpha1/vms/" + id,
				"spec": m["spec"],
				"id":   id,
			})
		}
		return jsonHTTPResponse(http.StatusOK, map[string]interface{}{"vms": vms})
	case method == http.MethodGet && strings.HasPrefix(clean, "/vms/"):
		id := strings.TrimPrefix(clean, "/vms/")
		st, raw := getSTI(id)
		if st != http.StatusOK {
			return jsonHTTPResponse(st, raw)
		}
		spec, _ := raw["spec"].(map[string]interface{})
		if spec == nil {
			spec = map[string]interface{}{}
		}
		if s, _ := raw["status"].(string); s != "" {
			spec["status"] = s
		}
		return jsonHTTPResponse(http.StatusOK, map[string]interface{}{
			"path": "/api/v1alpha1/vms/" + id,
			"spec": spec,
			"id":   id,
		})
	case method == http.MethodDelete && strings.HasPrefix(clean, "/vms/"):
		id := strings.TrimPrefix(clean, "/vms/")
		return emptyHTTPResponse(deleteEmbeddedResource(id))
	default:
		return nil, fmt.Errorf("unsupported embedded kubevirt SP path %s %s", method, path)
	}
}

func doEmbeddedAcmClusterSPRequest(method, path, body string) (*http.Response, error) {
	clean, query := splitSPPath(path)
	switch {
	case method == http.MethodGet && strings.HasSuffix(clean, "/clusters/health"):
		h := embeddedHealthPayload("cluster")
		h["path"] = "health"
		h["type"] = "acm-cluster-service-provider.dcm.io/health"
		return jsonHTTPResponse(http.StatusOK, h)
	case method == http.MethodPost && strings.TrimSuffix(clean, "/") == "/clusters":
		spec := parseSpecBody(body)
		if spec == nil {
			return jsonHTTPResponse(http.StatusBadRequest, map[string]interface{}{"title": "invalid argument"})
		}
		name := uniqueName("e2e-embed-cluster")
		resourceID, _, status, raw := createEmbeddedInstance("cluster", name, []map[string]interface{}{
			{"path": "metadata.name", "value": name, "resource": "main"},
		})
		if status != http.StatusCreated && status != http.StatusOK {
			return jsonHTTPResponse(status, raw)
		}
		return jsonHTTPResponse(http.StatusCreated, map[string]interface{}{"id": resourceID})
	case method == http.MethodGet && strings.TrimSuffix(clean, "/") == "/clusters":
		st, raw := listSTIs("cluster", query)
		if st != http.StatusOK {
			return jsonHTTPResponse(st, raw)
		}
		items, _ := raw["instances"].([]interface{})
		return jsonHTTPResponse(http.StatusOK, map[string]interface{}{"clusters": items})
	case method == http.MethodGet && strings.HasPrefix(clean, "/clusters/"):
		id := strings.TrimPrefix(clean, "/clusters/")
		st, raw := getSTI(id)
		return jsonHTTPResponse(st, raw)
	case method == http.MethodDelete && strings.HasPrefix(clean, "/clusters/"):
		id := strings.TrimPrefix(clean, "/clusters/")
		return emptyHTTPResponse(deleteEmbeddedResource(id))
	default:
		return nil, fmt.Errorf("unsupported embedded ACM SP path %s %s", method, path)
	}
}
