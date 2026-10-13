//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Declarative API coverage selected from FLPATH-4114 and FLPATH-4854.
//
// The suite deliberately uses two container resources because this exercises
// both the declarative DAG and the resource selector that the CLI previously
// dropped. It works with either a standalone container SP or an embedded
// container provider.
var _ = Describe("Declarative API", Label("api", "declarative"), Ordered, func() {
	var (
		catalogItemID string
		policyID      string
		createdPolicy bool
		instanceIDs   []string
	)

	BeforeAll(func() {
		requireContainerSP()

		agentName := discoverAgentByServiceType("container", "")
		policyID = findGlobalPolicySelectingAgent(agentName)
		if policyID == "" {
			policyID, createdPolicy = createEmbeddedRoutingPolicy("container", agentName)
		}
		Expect(policyID).NotTo(BeEmpty(), "a container placement policy is required")

		catalogItemID = createDeclarativeCatalogItem()
		currentDeclarativeCatalogItemID = catalogItemID
		verifyDeclarativeCatalogItem(catalogItemID)
	})

	AfterAll(func() {
		for _, instanceID := range instanceIDs {
			deleteDeclarativeInstance(instanceID)
		}
		if catalogItemID != "" {
			deleteDeclarativeObject("/catalog-items/" + catalogItemID)
		}
		if createdPolicy && policyID != "" {
			deleteDeclarativeObject("/policies/" + policyID)
		}
	})

	It("creates a multi-resource instance and preserves resource selectors", func() {
		backendName := uniqueName("e2e-decl-backend")
		frontendName := uniqueName("e2e-decl-frontend")
		instanceID, body := createDeclarativeInstance(backendName, frontendName)
		instanceIDs = append(instanceIDs, instanceID)

		Expect(body["run_id"]).NotTo(BeEmpty(), "create must return a run_id")
		userValues, ok := body["spec"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "instance response must contain spec")
		values, ok := userValues["user_values"].([]interface{})
		Expect(ok).To(BeTrue(), "instance response must preserve user_values")
		Expect(values).To(HaveLen(2))
		selectors := map[string]string{}
		for _, raw := range values {
			value, ok := raw.(map[string]interface{})
			Expect(ok).To(BeTrue(), "each user value must be an object")
			Expect(value["path"]).To(Equal("metadata.name"))
			selectors[stringField(value, "resource")] = stringField(value, "value")
		}
		Expect(selectors).To(Equal(map[string]string{
			"backend":  backendName,
			"frontend": frontendName,
		}))

		backend := waitForDeclarativeSTI(backendName, instanceStatusRunning)
		frontend := waitForDeclarativeSTI(frontendName, instanceStatusRunning)
		Expect(stiStatus(backend)).To(Equal(instanceStatusRunning))
		Expect(stiStatus(frontend)).To(Equal(instanceStatusRunning))

		getResp, err := doRequest(http.MethodGet, "/catalog-item-instances/"+instanceID, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(getResp.StatusCode).To(Equal(http.StatusOK))
		var got map[string]interface{}
		decodeJSON(getResp, &got)
		Expect(got["uid"]).To(Equal(instanceID))
	})

	It("rejects an unknown resource selector without creating a resource", func() {
		before := listServiceTypeInstanceIDs()
		name := uniqueName("e2e-decl-invalid")
		payload := declarativeInstancePayload(name, []map[string]interface{}{
			{"resource": "does-not-exist", "path": "metadata.name", "value": name},
		})

		resp, err := doRequest(http.MethodPost, "/catalog-item-instances", payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		problem := string(readBody(resp))
		Expect(problem).To(ContainSubstring("resource"))
		Expect(problem).To(ContainSubstring("does-not-exist"))

		Consistently(func() int {
			return len(newServiceTypeInstanceIDs(before))
		}).WithTimeout(5*time.Second).WithPolling(1*time.Second).Should(Equal(0),
			"invalid selector must not create a child service-type instance")
	})

	It("provisions dependencies in order and cleans up all child resources", func() {
		backendName := uniqueName("e2e-decl-order-backend")
		frontendName := uniqueName("e2e-decl-order-frontend")
		instanceID, _ := createDeclarativeInstance(backendName, frontendName)
		instanceIDs = append(instanceIDs, instanceID)

		frontendSeenBeforeBackend := false
		var backend map[string]interface{}
		Eventually(func() string {
			items := declarativeSTIsByName()
			backend = items[backendName]
			frontend := items[frontendName]
			if backend != nil && stiStatus(backend) != instanceStatusRunning && frontend != nil {
				frontendSeenBeforeBackend = true
			}
			if backend == nil {
				return "missing"
			}
			return stiStatus(backend)
		}).WithTimeout(provisionTimeout).WithPolling(2*time.Second).Should(Equal(instanceStatusRunning),
			"backend must reach RUNNING before the dependent frontend")
		Expect(frontendSeenBeforeBackend).To(BeFalse(),
			"frontend must not be provisioned before backend reaches RUNNING")

		frontend := waitForDeclarativeSTI(frontendName, instanceStatusRunning)
		Expect(stiStatus(frontend)).To(Equal(instanceStatusRunning))

		deleteDeclarativeInstance(instanceID)
		instanceIDs = instanceIDs[:len(instanceIDs)-1]
		Eventually(func() int {
			resp, err := doRequest(http.MethodGet, "/catalog-item-instances/"+instanceID, "")
			if err != nil {
				return 0
			}
			defer resp.Body.Close()
			return resp.StatusCode
		}).WithTimeout(cleanupTimeout).WithPolling(2 * time.Second).Should(Equal(http.StatusNotFound))

		for _, name := range []string{backendName, frontendName} {
			Eventually(func() int {
				item := declarativeSTIsByName()[name]
				if item == nil {
					return http.StatusNotFound
				}
				return http.StatusOK
			}).WithTimeout(cleanupTimeout).WithPolling(2*time.Second).Should(Equal(http.StatusNotFound),
				"child resource %s must be removed with the catalog instance", name)
		}
	})
})

func createDeclarativeCatalogItem() string {
	name := uniqueName("e2e-declarative")
	payload := map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": name,
		"spec": map[string]interface{}{
			"resources": []map[string]interface{}{
				declarativeResource("backend", "decl-backend"),
				{
					"name":               "frontend",
					"service_type":       "container",
					"requires_resources": []string{"backend"},
					"fields":             declarativeFields("decl-frontend", "docker.io/library/httpd:alpine"),
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	resp, err := doRequest(http.MethodPost, "/catalog-items", string(data))
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	id := stringField(body, "uid")
	Expect(id).NotTo(BeEmpty(), "catalog item creation must return uid")
	return id
}

func declarativeResource(name, defaultName string) map[string]interface{} {
	return map[string]interface{}{
		"name":         name,
		"service_type": "container",
		"fields":       declarativeFields(defaultName, "docker.io/library/nginx:alpine"),
	}
}

func declarativeFields(defaultName, image string) []map[string]interface{} {
	return []map[string]interface{}{
		{"path": "metadata.name", "display_name": "Name", "editable": true, "default": defaultName},
		{"path": "image.reference", "display_name": "Image", "editable": true, "default": image},
		{"path": "resources.cpu.min", "editable": false, "default": "1"},
		{"path": "resources.cpu.max", "editable": false, "default": "1"},
		{"path": "resources.memory.min", "editable": false, "default": "128MB"},
		{"path": "resources.memory.max", "editable": false, "default": "256MB"},
	}
}

func verifyDeclarativeCatalogItem(id string) {
	resp, err := doRequest(http.MethodGet, "/catalog-items/"+id, "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	spec, ok := body["spec"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	resources, ok := spec["resources"].([]interface{})
	Expect(ok).To(BeTrue())
	Expect(resources).To(HaveLen(2))

	names := map[string]bool{}
	frontendDependencies := []interface{}{}
	for _, raw := range resources {
		resource, ok := raw.(map[string]interface{})
		Expect(ok).To(BeTrue())
		name := stringField(resource, "name")
		names[name] = true
		Expect(resource["service_type"]).To(Equal("container"))
		if name == "frontend" {
			frontendDependencies, _ = resource["requires_resources"].([]interface{})
		}
	}
	Expect(names).To(Equal(map[string]bool{"backend": true, "frontend": true}))
	Expect(frontendDependencies).To(ConsistOf("backend"),
		"frontend must retain its dependency on backend")
}

func createDeclarativeInstance(backendName, frontendName string) (string, map[string]interface{}) {
	payload := declarativeInstancePayload(uniqueName("e2e-decl-instance"), []map[string]interface{}{
		{"resource": "backend", "path": "metadata.name", "value": backendName},
		{"resource": "frontend", "path": "metadata.name", "value": frontendName},
	})
	payloadMap := map[string]interface{}{}
	Expect(json.Unmarshal([]byte(payload), &payloadMap)).To(Succeed())
	spec := payloadMap["spec"].(map[string]interface{})
	spec["catalog_item_id"] = currentDeclarativeCatalogItemID
	data, err := json.Marshal(payloadMap)
	Expect(err).NotTo(HaveOccurred())
	resp, err := doRequest(http.MethodPost, "/catalog-item-instances", string(data))
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	id := stringField(body, "uid")
	Expect(id).NotTo(BeEmpty(), "catalog item instance creation must return uid")
	return id, body
}

var currentDeclarativeCatalogItemID string

func declarativeInstancePayload(displayName string, values []map[string]interface{}) string {
	payload := map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": displayName,
		"spec": map[string]interface{}{
			"catalog_item_id": currentDeclarativeCatalogItemID,
			"user_values":     values,
		},
	}
	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func deleteDeclarativeInstance(id string) {
	if id == "" {
		return
	}
	resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+id, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: cleanup DELETE instance %s failed: %v\n", id, err)
		return
	}
	_ = resp.Body.Close()
	Eventually(func() int {
		check, err := doRequest(http.MethodGet, "/catalog-item-instances/"+id, "")
		if err != nil {
			return 0
		}
		defer check.Body.Close()
		return check.StatusCode
	}).WithTimeout(cleanupTimeout).WithPolling(2*time.Second).Should(Equal(http.StatusNotFound),
		"catalog item instance %s must be removed", id)
}

func deleteDeclarativeObject(path string) {
	resp, err := doRequest(http.MethodDelete, path, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: cleanup DELETE %s failed: %v\n", path, err)
		return
	}
	_ = resp.Body.Close()
}

func declarativeSTIsByName() map[string]map[string]interface{} {
	resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=container&max_page_size=100", "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	items, _ := body["instances"].([]interface{})
	result := map[string]map[string]interface{}{}
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if name := declarativeSTIName(item); name != "" {
			result[name] = item
		}
	}
	return result
}

func newServiceTypeInstanceIDs(before map[string]struct{}) []string {
	var result []string
	for id := range listServiceTypeInstanceIDs() {
		if _, exists := before[id]; !exists {
			result = append(result, id)
		}
	}
	return result
}

func waitForDeclarativeSTI(name, expectedStatus string) map[string]interface{} {
	var found map[string]interface{}
	Eventually(func() string {
		found = declarativeSTIsByName()[name]
		if found == nil {
			return "missing"
		}
		return stiStatus(found)
	}).WithTimeout(provisionTimeout).WithPolling(2*time.Second).Should(Equal(expectedStatus),
		"service-type-instance %s must reach %s", name, expectedStatus)
	return found
}

func declarativeSTIName(item map[string]interface{}) string {
	if spec, ok := item["spec"].(map[string]interface{}); ok {
		if metadata, ok := spec["metadata"].(map[string]interface{}); ok {
			if name := stringField(metadata, "name"); name != "" {
				return name
			}
		}
	}
	return stringField(item, "name")
}

func stiStatus(item map[string]interface{}) string {
	return strings.ToLower(stringField(item, "status"))
}
