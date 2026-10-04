//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Core Platform", Label("core", "platform"), func() {
	Context("provisioning happy path", Ordered, func() {
		var containerProviderName string
		var catalogItemID, policyID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()
		})

		AfterAll(func() {
			if instanceID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
				if err != nil {
					GinkgoWriter.Printf("Warning: cleanup failed for catalog-item-instance %s: %v\n", instanceID, err)
				}
				if resp != nil {
					resp.Body.Close()
				}
				// Wait for async teardown so catalog-item delete doesn't hit a dependency conflict.
				Eventually(func() int {
					r, e := doRequest(http.MethodGet, "/catalog-item-instances/"+instanceID, "")
					if e != nil {
						return 0
					}
					defer r.Body.Close()
					return r.StatusCode
				}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(Equal(http.StatusNotFound))
			}
			if policyID != "" {
				resp, err := doRequest(http.MethodDelete, "/policies/"+policyID, "")
				if err != nil {
					GinkgoWriter.Printf("Warning: cleanup failed for policy %s: %v\n", policyID, err)
				}
				if resp != nil {
					resp.Body.Close()
				}
			}
			if catalogItemID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-items/"+catalogItemID, "")
				if err != nil {
					GinkgoWriter.Printf("Warning: cleanup failed for catalog-item %s: %v\n", catalogItemID, err)
				}
				if resp != nil {
					resp.Body.Close()
				}
			}
		})

		It("discovers the container agent", func() {
			override := os.Getenv("DCM_CONTAINER_AGENT_NAME")
			if override == "" {
				override = os.Getenv("DCM_CONTAINER_PROVIDER_NAME")
			}
			containerProviderName = discoverAgentByServiceType("container", override)
			GinkgoWriter.Printf("Selected container agent: %s\n", containerProviderName)
		})

		It("verifies the container service type exists", func() {
			resp, err := doRequest(http.MethodGet, "/service-types", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("results"))

			results, ok := body["results"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, r := range results {
				st, ok := r.(map[string]interface{})
				Expect(ok).To(BeTrue(), "service type entry should be a map")
				if stype, _ := st["service_type"].(string); stype == "container" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "no service type with service_type=container found — stack precondition not met")
		})

		It("creates a catalog item", func() {
			name := uniqueName("e2e-core")
			payload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"resources": [{
						"name": "main",
						"service_type": "container",
						"fields": [
							{"path": "metadata.name", "display_name": "Container Name", "editable": true, "default": %q},
							{"path": "image.reference", "display_name": "Image", "editable": true, "default": "docker.io/library/nginx:alpine"},
							{"path": "resources.cpu.min", "editable": false, "default": "1"},
							{"path": "resources.cpu.max", "editable": false, "default": "1"},
							{"path": "resources.memory.min", "editable": false, "default": "128MB"},
							{"path": "resources.memory.max", "editable": false, "default": "256MB"}
						]
					}]
				}
			}`, name, name)

			resp, err := doRequest(http.MethodPost, "/catalog-items", payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("uid"))

			uid, ok := body["uid"].(string)
			Expect(ok).To(BeTrue(), "uid should be a string")
			catalogItemID = uid
		})

		It("creates a routing policy", func() {
			Expect(containerProviderName).NotTo(BeEmpty(), "provider must be discovered first")

			name := uniqueName("e2e-core-policy")
			pkgName := fmt.Sprintf("e2e_core_%d", time.Now().UnixNano()%1000000)
			payload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, name, pkgName, containerProviderName)

			resp, err := doRequest(http.MethodPost, "/policies", payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("id"))

			id, ok := body["id"].(string)
			Expect(ok).To(BeTrue(), "id should be a string")
			policyID = id
		})

		It("creates a catalog item instance", func() {
			Expect(catalogItemID).NotTo(BeEmpty(), "catalog item must be created first")

			name := uniqueName("e2e-core-inst")
			payload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"catalog_item_id": %q,
					"user_values": [
						{"path": "metadata.name", "value": %q, "resource": "main"},
						{"path": "image.reference", "value": "docker.io/library/nginx:alpine", "resource": "main"}
					]
				}
			}`, name, catalogItemID, name)

			beforeSTIs := listServiceTypeInstanceIDs()
			resp, err := doRequest(http.MethodPost, "/catalog-item-instances", payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("uid"))

			uid, ok := body["uid"].(string)
			Expect(ok).To(BeTrue(), "uid should be a string")
			instanceID = uid

			spec, ok := body["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "spec should be a map")
			Expect(spec).To(HaveKey("catalog_item_id"))
			Expect(body).To(HaveKey("run_id"), "create must return run_id after control-plane#39")
			resourceID = resolveResourceIDAfterCreate(body, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
		})

		It("reaches RUNNING status", func() {
			Expect(resourceID).NotTo(BeEmpty(), "catalog item instance must be created first")

			Eventually(func() string {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusRunning),
				"service type instance should reach RUNNING status")
		})

		It("has correct agent assignment", func() {
			Expect(resourceID).NotTo(BeEmpty(), "instance must be provisioned first")

			resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body["status"]).To(Equal(instanceStatusRunning), "instance should still be RUNNING during verification")
			Expect(body["agent_name"]).To(Equal(containerProviderName))
		})
	})
})
