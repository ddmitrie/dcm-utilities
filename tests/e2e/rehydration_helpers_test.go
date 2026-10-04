//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dcm-project/utilities/tests/e2e/internal/resolve"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	provisionTimeout = 120 * time.Second
	rehydrateTimeout = 90 * time.Second
	cleanupTimeout   = 90 * time.Second
	healthTimeout    = 90 * time.Second
	pollInterval     = 2 * time.Second
)

// --- Types ----------------------------------------------------------------- //

type CatalogItemInstance struct {
	UID         string
	ResourceID  string
	RunID       string
	DisplayName string
	APIVersion  string
	CreateTime  string
	UpdateTime  string
	Spec        map[string]interface{}
	Raw         map[string]interface{}
}

type ThreeTierProvider struct {
	Name          string
	HealthStatus  string
	ServiceType   string
	ContainerName string
	Namespace     string
}

// --- Catalog item setup ---------------------------------------------------- //

var rehydrationCatalogItemID string
var rehydrationResourceName string

func initRehydrationCatalogItem() {
	catalogItemID := os.Getenv("DCM_CATALOG_ITEM_ID")
	if catalogItemID != "" {
		rehydrationCatalogItemID = catalogItemID
		rehydrationResourceName = os.Getenv("DCM_RESOURCE_NAME")
		if rehydrationResourceName == "" {
			rehydrationResourceName = "pet-clinic"
		}
		GinkgoWriter.Printf("Using catalog item ID from env: %s (resource=%s)\n", catalogItemID, rehydrationResourceName)
		return
	}

	resp, err := doRequest(http.MethodGet, "/catalog-items", "")
	if err != nil {
		GinkgoWriter.Printf("Failed to list catalog items: %v\n", err)
		return
	}

	var listBody map[string]interface{}
	decodeJSON(resp, &listBody)
	results, _ := listBody["results"].([]interface{})
	for _, r := range results {
		item, _ := r.(map[string]interface{})
		spec, _ := item["spec"].(map[string]interface{})

		// Check multi-resource schema (spec.resources[].service_type)
		if resources, ok := spec["resources"].([]interface{}); ok {
			for _, res := range resources {
				resource, _ := res.(map[string]interface{})
				st, _ := resource["service_type"].(string)
				if st == "three-tier-app-demo" || st == "three_tier_app_demo" {
					rehydrationCatalogItemID = stringField(item, "uid")
					rehydrationResourceName = stringField(resource, "name")
					GinkgoWriter.Printf("Rehydration catalog item ID: %s (service_type=%s, display_name=%s, resource=%s)\n",
						rehydrationCatalogItemID, st, stringField(item, "display_name"), rehydrationResourceName)
					return
				}
			}
		}
	}

	GinkgoWriter.Println("No three-tier-app-demo catalog item found — creating one")
	payload := map[string]interface{}{
		"display_name": "Rehydration Pet Clinic",
		"api_version":  "v1alpha1",
		"spec": map[string]interface{}{
			"resources": []map[string]interface{}{
				{
					"name":         "pet-clinic",
					"service_type": "three-tier-app-demo",
					"fields": []map[string]interface{}{
						{"path": "database.engine", "display_name": "Database engine", "editable": true, "default": "postgres"},
						{"path": "database.version", "display_name": "Database version", "editable": true, "default": "18"},
						{"path": "app.image", "display_name": "App image", "default": "docker.io/springcommunity/spring-framework-petclinic:6.1.2"},
						{"path": "web.image", "display_name": "Web image", "default": "docker.io/library/nginx:alpine"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(payload)
	createResp, err := doRequest(http.MethodPost, "/catalog-items", string(data))
	Expect(err).NotTo(HaveOccurred(), "failed to create rehydration catalog item")
	Expect(createResp.StatusCode).To(SatisfyAny(
		Equal(http.StatusOK),
		Equal(http.StatusCreated),
	), "catalog item creation returned unexpected status %d", createResp.StatusCode)

	var body map[string]interface{}
	decodeJSON(createResp, &body)
	rehydrationCatalogItemID = stringField(body, "uid")
	rehydrationResourceName = "pet-clinic"
	Expect(rehydrationCatalogItemID).NotTo(BeEmpty(), "catalog item creation returned empty UID")
	Expect(rehydrationResourceName).NotTo(BeEmpty(), "catalog item resource name not resolved")
	GinkgoWriter.Printf("Rehydration catalog item ID: %s (resource=%s)\n", rehydrationCatalogItemID, rehydrationResourceName)
}

// --- Provider discovery ---------------------------------------------------- //

var (
	threeTierProviders     []ThreeTierProvider
	threeTierSPReady       bool
	multiProviderAvailable bool
	threeProviderAvailable bool
	providersByRegion      map[string][]ThreeTierProvider
)

func initThreeTierSP() {
	resp, err := doRequest(http.MethodGet, "/agents", "")
	if err != nil {
		GinkgoWriter.Printf("Failed to list agents: %v — rehydration tests will be skipped\n", err)
		return
	}

	var body map[string]interface{}
	decodeJSON(resp, &body)

	agentsList, ok := body["agents"].([]interface{})
	if !ok {
		GinkgoWriter.Println("No agents found — rehydration tests will be skipped")
		return
	}

	providersByRegion = make(map[string][]ThreeTierProvider)

	for _, p := range agentsList {
		provider, _ := p.(map[string]interface{})
		name, _ := provider["name"].(string)
		health := stringField(provider, "health_status")

		serviceTypes, _ := provider["service_types"].([]interface{})
		serviceType := ""
		for _, st := range serviceTypes {
			if s, _ := st.(string); s == "three-tier-app-demo" || s == "three_tier_app_demo" {
				serviceType = s
				break
			}
		}
		if serviceType == "" {
			continue
		}
		if health != "ready" {
			continue
		}

		tp := ThreeTierProvider{
			Name:         name,
			HealthStatus: health,
			ServiceType:  serviceType,
		}

		if podmanAvailable {
			containerName := resolveContainerName(name)
			if containerName != "" {
				tp.ContainerName = containerName
			}
		}

		tp.Namespace = resolveProviderNamespace(name)

		threeTierProviders = append(threeTierProviders, tp)

		region := resolveProviderRegion(name)
		if region != "" {
			providersByRegion[region] = append(providersByRegion[region], tp)
		}
	}

	threeTierSPReady = len(threeTierProviders) >= 1
	multiProviderAvailable = len(threeTierProviders) >= 2
	threeProviderAvailable = len(threeTierProviders) >= 3

	GinkgoWriter.Printf("Three-tier SPs discovered: %d (multi=%v, three=%v)\n",
		len(threeTierProviders), multiProviderAvailable, threeProviderAvailable)
	for _, tp := range threeTierProviders {
		GinkgoWriter.Printf("  - %s (health=%s, ns=%s, container=%s)\n",
			tp.Name, tp.HealthStatus, tp.Namespace, tp.ContainerName)
	}
}

func resolveContainerName(providerName string) string {
	var searchName string
	switch {
	case strings.HasSuffix(providerName, "sp-b"):
		searchName = "three-tier-app-demo-sp-b"
	case strings.HasSuffix(providerName, "sp-c"):
		searchName = "three-tier-app-demo-sp-c"
	default:
		searchName = "three-tier-app-demo-service-provider"
	}

	out, err := exec.Command(podmanBin, "ps", "-a", "--filter", "name="+searchName, "--format", "{{.Names}}").CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" && !strings.Contains(line, "init-db") {
				return line
			}
		}
	}

	return "dcm-e2e_" + searchName + "_1"
}

func resolveProviderNamespace(providerName string) string {
	switch {
	case strings.Contains(providerName, "sp-b"):
		return "rehydrate-ns-b"
	case strings.Contains(providerName, "sp-c"):
		return "rehydrate-ns-c"
	default:
		return "rehydrate-ns-a"
	}
}

func resolveProviderRegion(providerName string) string {
	switch {
	case strings.Contains(providerName, "sp-c"):
		return "west"
	default:
		return "east"
	}
}

func firstResourceID(body map[string]interface{}) string {
	if v, ok := body["__e2e_resource_id"].(string); ok && v != "" {
		return v
	}
	if ids := resolve.LegacyResourceIDs(body); len(ids) > 0 {
		return ids[0]
	}
	if runID := stringField(body, "run_id"); runID != "" {
		return runResourceIDCache[runID]
	}
	return ""
}

func requireThreeTierSP() {
	if !threeTierSPReady {
		Skip("No three-tier SP available (deploy with --three-tier-app-demo-service-provider)")
	}
}

func requireMultiProvider() {
	if !multiProviderAvailable {
		Skip("Multi-provider not available (need 2+ three-tier SPs)")
	}
}

func requireThreeProviders() {
	if !threeProviderAvailable {
		Skip("Three providers not available (need 3 three-tier SPs for sovereignty tests)")
	}
}

func stringField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

// --- Instance lifecycle ---------------------------------------------------- //

func createTestInstance(displayName string, userValues []map[string]string) CatalogItemInstance {
	ensureProvidersReady()

	uvArray := make([]map[string]interface{}, len(userValues))
	for i, uv := range userValues {
		uvArray[i] = map[string]interface{}{
			"resource": uv["resource"],
			"path":     uv["path"],
			"value":    uv["value"],
		}
	}

	Expect(rehydrationCatalogItemID).NotTo(BeEmpty(),
		"rehydration catalog item not initialized — BeforeSuite must call initRehydrationCatalogItem()")

	payload := map[string]interface{}{
		"display_name": displayName,
		"api_version":  "v1alpha1",
		"spec": map[string]interface{}{
			"catalog_item_id": rehydrationCatalogItemID,
			"user_values":     uvArray,
		},
	}

	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())

	before := listServiceTypeInstanceIDs()
	resp, err := doRequest(http.MethodPost, "/catalog-item-instances", string(data))
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body := readBody(resp)
		Fail(fmt.Sprintf("create instance failed with status %d: %s", resp.StatusCode, string(body)))
	}

	inst := parseCatalogItemInstance(resp)
	if inst.ResourceID == "" {
		inst.ResourceID = resolveResourceIDAfterCreate(inst.Raw, before)
	}
	Expect(inst.ResourceID).NotTo(BeEmpty(),
		"could not resolve placement resource ID after create")
	return inst
}

func getInstance(uid string) CatalogItemInstance {
	resp, err := doRequest(http.MethodGet, "/catalog-item-instances/"+uid, "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	return parseCatalogItemInstance(resp)
}

func getInstanceRaw(uid string) (*http.Response, error) {
	return doRequest(http.MethodGet, "/catalog-item-instances/"+uid, "")
}

func rehydrateInstance(uid string) (*http.Response, map[string]interface{}) {
	ensureProvidersReady()
	before := listServiceTypeInstanceIDs()
	resp, err := doRequest(http.MethodPost, "/catalog-item-instances/"+uid+":rehydrate", "")
	Expect(err).NotTo(HaveOccurred())

	var body map[string]interface{}
	decodeJSON(resp, &body)
	if resp.StatusCode == http.StatusOK {
		rid := resolveResourceIDAfterCreate(body, before)
		body["__e2e_resource_id"] = rid
	}
	return resp, body
}

func rehydrateInstanceRaw(uid string) (*http.Response, []byte) {
	ensureProvidersReady()
	resp, err := doRequest(http.MethodPost, "/catalog-item-instances/"+uid+":rehydrate", "")
	Expect(err).NotTo(HaveOccurred())
	return resp, readBody(resp)
}

func deleteInstance(uid string) {
	resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+uid, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: cleanup DELETE failed for instance %s: %v\n", uid, err)
		return
	}
	resp.Body.Close()
}

func parseCatalogItemInstance(resp *http.Response) CatalogItemInstance {
	var raw map[string]interface{}
	decodeJSON(resp, &raw)

	inst := CatalogItemInstance{
		UID:         stringField(raw, "uid"),
		RunID:       stringField(raw, "run_id"),
		DisplayName: stringField(raw, "display_name"),
		APIVersion:  stringField(raw, "api_version"),
		CreateTime:  stringField(raw, "create_time"),
		UpdateTime:  stringField(raw, "update_time"),
		Raw:         raw,
	}
	if spec, ok := raw["spec"].(map[string]interface{}); ok {
		inst.Spec = spec
		if ids := resolve.LegacyResourceIDs(raw); len(ids) > 0 {
			inst.ResourceID = ids[0]
		}
	}
	if inst.ResourceID == "" && inst.RunID != "" {
		inst.ResourceID = runResourceIDCache[inst.RunID]
	}
	return inst
}

func waitForInstanceRunning(resourceID string, timeout time.Duration) {
	// Three-tier-app-demo SPs provision synchronously — status stays PENDING
	// because no NATS status callback exists for this SP type. Poll until
	// SPRM has the instance registered (GET 200), which confirms provisioning.
	Eventually(func() error {
		resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("GET /service-type-instances/%s returned %d", resourceID, resp.StatusCode)
		}
		var body map[string]interface{}
		decodeJSON(resp, &body)
		s, _ := body["status"].(string)
		if s == instanceStatusRunning || s == instanceStatusPending {
			return nil
		}
		return fmt.Errorf("instance %s has unexpected status %q", resourceID, s)
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(Succeed(),
		"service type instance %s should be provisioned", resourceID)
}

func ensureProvidersReady() {
	for _, tp := range threeTierProviders {
		if tp.ContainerName == "" {
			continue
		}
		out, _ := exec.Command(podmanBin, "inspect", "--format", "{{.State.Running}}", tp.ContainerName).CombinedOutput()
		if strings.TrimSpace(string(out)) != "true" {
			continue
		}
		waitForProviderHealth(tp.Name, "ready", healthTimeout)
	}
}

func defaultUserValues() []map[string]string {
	return []map[string]string{
		{"resource": rehydrationResourceName, "path": "database.engine", "value": "postgres"},
		{"resource": rehydrationResourceName, "path": "database.version", "value": "18"},
	}
}

// --- Policy helpers -------------------------------------------------------- //

func createPlacementPolicy(name, regoCode string) string {
	payload := map[string]interface{}{
		"display_name": name,
		"policy_type":  "GLOBAL",
		"rego_code":    regoCode,
	}
	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())

	resp, err := doRequest(http.MethodPost, "/policies", string(data))
	Expect(err).NotTo(HaveOccurred())

	if resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		deleteAllPolicies()
		resp, err = doRequest(http.MethodPost, "/policies", string(data))
		Expect(err).NotTo(HaveOccurred())
	}

	Expect(resp.StatusCode).To(SatisfyAny(
		Equal(http.StatusOK),
		Equal(http.StatusCreated),
	), "create policy failed with status %d", resp.StatusCode)

	var body map[string]interface{}
	decodeJSON(resp, &body)
	id := stringField(body, "id")
	Expect(id).NotTo(BeEmpty(), "policy creation returned empty ID")
	return id
}

func deletePlacementPolicy(id string) {
	resp, err := doRequest(http.MethodDelete, "/policies/"+id, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: policy delete failed for %s: %v\n", id, err)
		return
	}
	resp.Body.Close()
}

func deleteAllPolicies() {
	resp, err := doRequest(http.MethodGet, "/policies", "")
	if err != nil {
		return
	}
	var body map[string]interface{}
	decodeJSON(resp, &body)

	policies, ok := body["policies"].([]interface{})
	if !ok {
		return
	}
	for _, p := range policies {
		policy, _ := p.(map[string]interface{})
		if id := stringField(policy, "id"); id != "" {
			deletePlacementPolicy(id)
		}
	}
}

func regoSelectProvider(providerName string) string {
	return fmt.Sprintf(`package placement
import rego.v1
main := {"selected_agent": "%s"} if { true }`, providerName)
}

// --- Provider disruption --------------------------------------------------- //

func stopProvider(provider ThreeTierProvider) {
	Expect(provider.ContainerName).NotTo(BeEmpty(),
		"cannot stop provider %s — container name not resolved", provider.Name)
	out, err := exec.Command(podmanBin, "stop", provider.ContainerName).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(),
		"podman stop %s failed: %s", provider.ContainerName, string(out))
	GinkgoWriter.Printf("Stopped provider %s (container: %s)\n", provider.Name, provider.ContainerName)
}

func startProvider(provider ThreeTierProvider) {
	Expect(provider.ContainerName).NotTo(BeEmpty(),
		"cannot start provider %s — container name not resolved", provider.Name)

	out, err := exec.Command(podmanBin, "start", provider.ContainerName).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(),
		"podman start %s failed: %s", provider.ContainerName, string(out))
	GinkgoWriter.Printf("Started provider %s (container: %s)\n", provider.Name, provider.ContainerName)
}

func restartSPRM() {
	container := findComposeContainer("service-provider-resource-manager")
	out, err := exec.Command(podmanBin, "restart", container).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(),
		"podman restart SPRM failed: %s", string(out))
	GinkgoWriter.Println("Restarted service-provider-resource-manager")
}

func waitForProviderHealth(providerName, expectedStatus string, timeout time.Duration) {
	Eventually(func() string {
		resp, err := doRequest(http.MethodGet, "/agents", "")
		if err != nil {
			return ""
		}
		var body map[string]interface{}
		decodeJSON(resp, &body)
		agents, _ := body["agents"].([]interface{})
		for _, a := range agents {
			agent, _ := a.(map[string]interface{})
			if stringField(agent, "name") == providerName {
				return stringField(agent, "health_status")
			}
		}
		return ""
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(Equal(expectedStatus),
		"agent %s did not reach health status %q", providerName, expectedStatus)
}

// --- Namespace-aware K8s assertions ---------------------------------------- //

func runKubectlInNamespace(namespace string, args ...string) (string, error) {
	fullArgs := append([]string{"-n", namespace}, args...)
	cmd := exec.Command(kubectlBin, fullArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func getDeploymentsInNamespace(namespace, resourceID string) []string {
	requireKubectl()
	out, err := runKubectlInNamespace(namespace,
		"get", "deployments",
		"-l", "dcm-resource-id="+resourceID,
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil
	}
	result := strings.TrimSpace(out)
	if result == "" {
		return nil
	}
	return strings.Fields(result)
}

func waitForDeploymentsGone(namespace, resourceID string, timeout time.Duration) {
	Eventually(func() []string {
		return getDeploymentsInNamespace(namespace, resourceID)
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(BeEmpty(),
		"deployments with resource_id %s still exist in namespace %s", resourceID, namespace)
}

func getActiveDeploymentNamespace(resourceID string) string {
	for _, provider := range threeTierProviders {
		if provider.Namespace == "" || provider.Namespace == "default" {
			continue
		}
		deps := getDeploymentsInNamespace(provider.Namespace, resourceID)
		if len(deps) > 0 {
			return provider.Namespace
		}
	}
	deps := getDeploymentsInNamespace("default", resourceID)
	if len(deps) > 0 {
		return "default"
	}
	return ""
}

func countDeploymentsAcrossNamespaces(resourceID string) int {
	total := 0
	seen := make(map[string]bool)
	for _, provider := range threeTierProviders {
		ns := provider.Namespace
		if ns == "" {
			ns = "default"
		}
		if seen[ns] {
			continue
		}
		seen[ns] = true
		total += len(getDeploymentsInNamespace(ns, resourceID))
	}
	return total
}
