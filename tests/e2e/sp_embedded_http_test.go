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
	embeddedCatalogMu       sync.Mutex
	// embeddedCreateResolveMu serializes snapshot→POST→resolve so concurrent
	// embedded creates cannot share the same global before/after STI diff.
	embeddedCreateResolveMu sync.Mutex
	embeddedCatalogItem     = map[string]string{} // serviceType → catalog item uid (created by this run)
	embeddedCatalogPol      = map[string]string{} // serviceType → policy id (created or reused)
	embeddedCreatedPolicies = map[string]bool{}   // policy ids this run created (not reused)
	embeddedSTIToInst       = map[string]string{} // service-type-instance id → catalog-item-instance uid
	embeddedAgentSeenAt     = time.Now()
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
	if status < 100 {
		return nil, fmt.Errorf("embedded SP adapter: invalid HTTP status %d (control-plane request failed)", status)
	}
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
	if status < 100 {
		return nil, fmt.Errorf("embedded SP adapter: invalid HTTP status %d (control-plane request failed)", status)
	}
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

// policySelectsAgent reports whether rego_code selects agentName via
// selected_agent (placement routing result).
func policySelectsAgent(regoCode, agentName string) bool {
	if agentName == "" || regoCode == "" {
		return false
	}
	return strings.Contains(regoCode, fmt.Sprintf(`"selected_agent": "%s"`, agentName)) ||
		strings.Contains(regoCode, fmt.Sprintf(`"selected_agent":"%s"`, agentName))
}

// findGlobalPolicySelectingAgent returns a GLOBAL policy whose rego selects
// agentName. Non-200 list responses and GLOBAL policies for other agents are
// ignored so embedded workloads are not mis-routed.
func findGlobalPolicySelectingAgent(agentName string) string {
	resp, err := doRequest(http.MethodGet, "/policies?max_page_size=100", "")
	if err != nil || resp == nil {
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		GinkgoWriter.Printf("Warning: list policies returned %d; not reusing GLOBAL for agent %s\n",
			resp.StatusCode, agentName)
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
		rego, _ := p["rego_code"].(string)
		if !strings.EqualFold(pt, "GLOBAL") || id == "" {
			continue
		}
		if policySelectsAgent(rego, agentName) {
			return id
		}
	}
	return ""
}

// createEmbeddedRoutingPolicy creates a GLOBAL policy that selects agentName.
// Unique priorities avoid 409 against an existing GLOBAL that routes elsewhere.
// Returns (id, createdByUs).
func createEmbeddedRoutingPolicy(serviceType, agentName string) (string, bool) {
	GinkgoHelper()
	for attempt := 0; attempt < 8; attempt++ {
		priority := 50
		if attempt > 0 {
			priority = 200 + int(time.Now().UnixNano()%7000) + attempt
		}
		pkg := fmt.Sprintf("e2e_embed_%s_%d", strings.ReplaceAll(serviceType, "-", "_"), time.Now().UnixNano())
		polName := uniqueName("e2e-embed-pol-" + serviceType)
		polPayload := fmt.Sprintf(`{
		"display_name": %q,
		"policy_type": "GLOBAL",
		"priority": %d,
		"description": "E2E embedded SP route for %s",
		"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
	}`, polName, priority, agentName, pkg, agentName)

		resp, err := doRequest(http.MethodPost, "/policies", polPayload)
		Expect(err).NotTo(HaveOccurred())
		if resp.StatusCode == http.StatusConflict {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if id := findGlobalPolicySelectingAgent(agentName); id != "" {
				return id, false
			}
			continue
		}
		Expect(resp.StatusCode).To(Equal(http.StatusCreated),
			"create routing policy for embedded %s → agent %s", serviceType, agentName)
		var polBody map[string]interface{}
		decodeJSON(resp, &polBody)
		polID, _ := polBody["id"].(string)
		Expect(polID).NotTo(BeEmpty())
		return polID, true
	}
	Fail(fmt.Sprintf("could not create GLOBAL routing policy for embedded %s → agent %s", serviceType, agentName))
	return "", false
}

func ensureEmbeddedCatalogRoute(serviceType string) (catalogItemID, agentName string) {
	GinkgoHelper()
	embeddedCatalogMu.Lock()
	defer embeddedCatalogMu.Unlock()

	if id, ok := embeddedCatalogItem[serviceType]; ok && id != "" {
		return id, discoverAgentByServiceType(serviceType, "")
	}

	agentName = discoverAgentByServiceType(serviceType, "")
	// Reuse a GLOBAL only when its rego already selects this agent. Otherwise
	// create a dedicated policy so we do not reuse a route aimed elsewhere.
	polID := findGlobalPolicySelectingAgent(agentName)
	createdPolicy := false
	if polID == "" {
		polID, createdPolicy = createEmbeddedRoutingPolicy(serviceType, agentName)
	}
	Expect(polID).NotTo(BeEmpty(), "need a GLOBAL routing policy for embedded %s (agent %s)", serviceType, agentName)
	embeddedCatalogPol[serviceType] = polID
	if createdPolicy {
		embeddedCreatedPolicies[polID] = true
	}

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

// cleanupEmbeddedCatalogRoutes deletes catalog-item-instances, then catalog
// items created by this run, then only GLOBAL policies this run created
// (never reused policies shared with other suites).
func cleanupEmbeddedCatalogRoutes() {
	embeddedCatalogMu.Lock()
	instances := make([]string, 0, len(embeddedSTIToInst))
	for _, instID := range embeddedSTIToInst {
		if instID != "" {
			instances = append(instances, instID)
		}
	}
	catalogItems := make([]string, 0, len(embeddedCatalogItem))
	for _, id := range embeddedCatalogItem {
		if id != "" {
			catalogItems = append(catalogItems, id)
		}
	}
	createdPolicies := make([]string, 0, len(embeddedCreatedPolicies))
	for id := range embeddedCreatedPolicies {
		if id != "" {
			createdPolicies = append(createdPolicies, id)
		}
	}
	embeddedCatalogMu.Unlock()

	for _, instID := range instances {
		resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instID, "")
		if err != nil {
			GinkgoWriter.Printf("Warning: cleanup DELETE catalog-item-instance %s: %v\n", instID, err)
			continue
		}
		if resp != nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		// Wait for async teardown so catalog-item delete does not hit a dependency conflict.
		Eventually(func() int {
			r, e := doRequest(http.MethodGet, "/catalog-item-instances/"+instID, "")
			if e != nil {
				return 0
			}
			defer r.Body.Close()
			return r.StatusCode
		}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Equal(http.StatusNotFound))
	}

	for _, id := range catalogItems {
		resp, err := doRequest(http.MethodDelete, "/catalog-items/"+id, "")
		if err != nil {
			GinkgoWriter.Printf("Warning: cleanup DELETE catalog-item %s: %v\n", id, err)
			continue
		}
		if resp != nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}

	for _, id := range createdPolicies {
		resp, err := doRequest(http.MethodDelete, "/policies/"+id, "")
		if err != nil {
			GinkgoWriter.Printf("Warning: cleanup DELETE policy %s: %v\n", id, err)
			continue
		}
		if resp != nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}

	embeddedCatalogMu.Lock()
	embeddedSTIToInst = map[string]string{}
	embeddedCatalogItem = map[string]string{}
	embeddedCatalogPol = map[string]string{}
	embeddedCreatedPolicies = map[string]bool{}
	embeddedCatalogMu.Unlock()
}

var _ = AfterSuite(func() {
	cleanupEmbeddedCatalogRoutes()
})

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

	// Serialize the full snapshot→POST→resolve sequence. Without this, two
	// concurrent embedded creates can observe the same new STI ID via the
	// global before/after diff when run association is unavailable.
	embeddedCreateResolveMu.Lock()
	defer embeddedCreateResolveMu.Unlock()

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

func getSTI(id string) (int, map[string]interface{}, error) {
	resp, err := doRequest(http.MethodGet, "/service-type-instances/"+id, "")
	if err != nil {
		return 0, nil, err
	}
	status := resp.StatusCode
	var body map[string]interface{}
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(data, &body)
	}
	return status, body, nil
}

func listSTIs(serviceType string, query url.Values) (int, map[string]interface{}, error) {
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
		return 0, nil, err
	}
	status := resp.StatusCode
	var body map[string]interface{}
	if resp.Body != nil {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(data, &body)
	}
	return status, body, nil
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

func userValuesFromClusterSpec(spec map[string]interface{}) (displayName string, values []map[string]interface{}) {
	displayName = uniqueName("e2e-embed-cluster")
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
		st, raw, err := listSTIs("container", query)
		if err != nil {
			return nil, err
		}
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
		st, raw, err := getSTI(id)
		if err != nil {
			return nil, err
		}
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
		st, raw, err := listSTIs("vm", query)
		if err != nil {
			return nil, err
		}
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
		st, raw, err := getSTI(id)
		if err != nil {
			return nil, err
		}
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
		name, values := userValuesFromClusterSpec(spec)
		resourceID, _, status, raw := createEmbeddedInstance("cluster", name, values)
		if status != http.StatusCreated && status != http.StatusOK {
			return jsonHTTPResponse(status, raw)
		}
		return jsonHTTPResponse(http.StatusCreated, map[string]interface{}{"id": resourceID})
	case method == http.MethodGet && strings.TrimSuffix(clean, "/") == "/clusters":
		st, raw, err := listSTIs("cluster", query)
		if err != nil {
			return nil, err
		}
		if st != http.StatusOK {
			return jsonHTTPResponse(st, raw)
		}
		items, _ := raw["instances"].([]interface{})
		return jsonHTTPResponse(http.StatusOK, map[string]interface{}{"clusters": items})
	case method == http.MethodGet && strings.HasPrefix(clean, "/clusters/"):
		id := strings.TrimPrefix(clean, "/clusters/")
		st, raw, err := getSTI(id)
		if err != nil {
			return nil, err
		}
		return jsonHTTPResponse(st, raw)
	case method == http.MethodDelete && strings.HasPrefix(clean, "/clusters/"):
		id := strings.TrimPrefix(clean, "/clusters/")
		return emptyHTTPResponse(deleteEmbeddedResource(id))
	default:
		return nil, fmt.Errorf("unsupported embedded ACM SP path %s %s", method, path)
	}
}
