//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Service Type Instances API", func() {
	Context("service_type filter", Ordered, func() {
		var containerAgentName string
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			containerAgentName = discoverAgentByServiceType("container", "")
			GinkgoWriter.Printf("Using container agent: %s\n", containerAgentName)

			By("creating a routing policy to direct traffic to the container agent")
			polName := uniqueName("e2e-filter-pol")
			pkgName := fmt.Sprintf("e2e_filter_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E instance filter test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, containerAgentName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item for the container service type")
			catName := uniqueName("e2e-filter")
			catPayload := fmt.Sprintf(`{
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
			}`, catName, catName)

			resp, err = doRequest(http.MethodPost, "/catalog-items", catPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var catBody map[string]interface{}
			decodeJSON(resp, &catBody)
			catalogItemID, _ = catBody["uid"].(string)
			Expect(catalogItemID).NotTo(BeEmpty())
			GinkgoWriter.Printf("Created catalog item: %s\n", catalogItemID)

			By("creating a catalog item instance")
			instName := uniqueName("e2e-filter-inst")
			instPayload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"catalog_item_id": %q,
					"user_values": [
						{"path": "metadata.name", "value": %q, "resource": "main"},
						{"path": "image.reference", "value": "docker.io/library/nginx:alpine", "resource": "main"}
					]
				}
			}`, instName, catalogItemID, instName)

			beforeSTIs := listServiceTypeInstanceIDs()
			resp, err = doRequest(http.MethodPost, "/catalog-item-instances", instPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var instBody map[string]interface{}
			decodeJSON(resp, &instBody)
			instanceID, _ = instBody["uid"].(string)
			Expect(instanceID).NotTo(BeEmpty())
			Expect(instBody).To(HaveKey("spec"), "spec should be present")
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			// control-plane#39: resource IDs come from CreateRun / STI discovery, not spec.resource_ids
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
			GinkgoWriter.Printf("Created catalog-item-instance: %s (resource_id=%s run_id=%v)\n",
				instanceID, resourceID, instBody["run_id"])

			By("waiting for the service-type-instance to be queryable")
			Eventually(func() int {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Equal(http.StatusOK))

			By("logging list-response field names for diagnostics")
			diagResp, diagErr := doRequest(http.MethodGet, "/service-type-instances?max_page_size=1", "")
			if diagErr == nil && diagResp.StatusCode == http.StatusOK {
				var diagBody map[string]interface{}
				decodeJSON(diagResp, &diagBody)
				if insts, ok := diagBody["instances"].([]interface{}); ok && len(insts) > 0 {
					first, _ := insts[0].(map[string]interface{})
					keys := make([]string, 0, len(first))
					for k := range first {
						keys = append(keys, k)
					}
					GinkgoWriter.Printf("Instance list-response fields: %v\n", keys)
				}
			}
		})

		AfterAll(func() {
			if instanceID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
				Eventually(func() int {
					r, e := doRequest(http.MethodGet, "/catalog-item-instances/"+instanceID, "")
					if e != nil {
						return 0
					}
					defer r.Body.Close()
					return r.StatusCode
				}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(Equal(http.StatusNotFound))
			}
			if catalogItemID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-items/"+catalogItemID, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
			}
			if policyID != "" {
				resp, err := doRequest(http.MethodDelete, "/policies/"+policyID, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
			}
		})

		It("returns the instance when filtering by service_type=container", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=container", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, inst := range instances {
				i := inst.(map[string]interface{})
				if i["id"] == resourceID {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(),
				"instance %s should appear when filtering by service_type=container", resourceID)
		})

		It("excludes the instance when filtering by a different service_type", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=vm", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			for _, inst := range instances {
				i := inst.(map[string]interface{})
				Expect(i["id"]).NotTo(Equal(resourceID),
					"container instance should not appear in service_type=vm results")
			}
		})

		It("returns empty list for non-existent service type", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=nonexistent-type", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(instances).To(BeEmpty(),
				"filtering by a non-existent service_type should return zero results")
		})

		It("includes the instance in unfiltered list", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, inst := range instances {
				i := inst.(map[string]interface{})
				if i["id"] == resourceID {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(),
				"unfiltered list should include instance %s", resourceID)
		})

		It("combines service_type with agent_name filter", func() {
			resp, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&agent_name="+containerAgentName, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, inst := range instances {
				i := inst.(map[string]interface{})
				if i["id"] == resourceID {
					found = true
				}
				agentName, _ := i["agent_name"].(string)
				Expect(agentName).To(Equal(containerAgentName),
					"combined filter should only return instances from the specified agent")
			}
			Expect(found).To(BeTrue(),
				"combined filter should include our instance")
		})

		It("respects pagination with service_type filter", func() {
			resp, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&max_page_size=1", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var page1 map[string]interface{}
			decodeJSON(resp, &page1)
			Expect(page1).To(HaveKey("instances"))

			instances, ok := page1["instances"].([]interface{})
			Expect(ok).To(BeTrue(), "instances should be an array")
			Expect(len(instances)).To(BeNumerically("<=", 1),
				"page size should be respected with filter")

			if token, ok := page1["next_page_token"].(string); ok && token != "" {
				resp2, err := doRequest(http.MethodGet,
					"/service-type-instances?service_type=container&max_page_size=1&page_token="+token, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp2.StatusCode).To(Equal(http.StatusOK))

				var page2 map[string]interface{}
				decodeJSON(resp2, &page2)
				Expect(page2).To(HaveKey("instances"))
			}
		})

		It("stores service_type derived from the provider on the instance", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)

			spec, ok := body["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "instance should have a spec object")
			Expect(spec["service_type"]).To(Equal("container"),
				"spec.service_type should match the provider's registered service type")
		})

		It("treats service_type filter as case-sensitive", func() {
			for _, variant := range []string{"Container", "CONTAINER", "ContaineR"} {
				resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type="+variant, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				var body map[string]interface{}
				decodeJSON(resp, &body)
				Expect(body).To(HaveKey("instances"))

				instances, ok := body["instances"].([]interface{})
				Expect(ok).To(BeTrue())

				for _, inst := range instances {
					i := inst.(map[string]interface{})
					Expect(i["id"]).NotTo(Equal(resourceID),
						"instance should not appear for case variant %q", variant)
				}
			}
		})

		It("handles empty service_type parameter gracefully", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(SatisfyAny(
				Equal(http.StatusOK),
				Equal(http.StatusBadRequest),
			), "empty service_type should either return all instances or a 400 error")

			if resp.StatusCode == http.StatusOK {
				var body map[string]interface{}
				decodeJSON(resp, &body)
				Expect(body).To(HaveKey("instances"))

				instances, ok := body["instances"].([]interface{})
				Expect(ok).To(BeTrue())

				var found bool
				for _, inst := range instances {
					i := inst.(map[string]interface{})
					if i["id"] == resourceID {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(),
					"empty service_type should behave like no filter (return all)")
			}
		})
	})

	Context("service_type filter edge cases", func() {
		BeforeEach(func() {
			requireContainerSP()
		})

		It("rejects or ignores special characters in service_type", func() {
			for _, malicious := range []string{"../admin", "'; DROP TABLE--", "<script>", "container&agent_name=x"} {
				encoded := url.QueryEscape(malicious)
				resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type="+encoded, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(SatisfyAny(
					Equal(http.StatusOK),
					Equal(http.StatusBadRequest),
				), "special chars %q should not cause a 500", malicious)

				if resp.StatusCode == http.StatusOK {
					var body map[string]interface{}
					decodeJSON(resp, &body)
					Expect(body).To(HaveKey("instances"))
					instances, _ := body["instances"].([]interface{})
					Expect(instances).To(BeEmpty(),
						"special chars %q should not match any real instances", malicious)
				} else {
					resp.Body.Close()
				}
			}
		})

		It("handles duplicate service_type query parameters", func() {
			resp, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&service_type=vm", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(SatisfyAny(
				Equal(http.StatusOK),
				Equal(http.StatusBadRequest),
			), "duplicate service_type params should not cause a 500")

			if resp.StatusCode == http.StatusOK {
				var body map[string]interface{}
				decodeJSON(resp, &body)
				Expect(body).To(HaveKey("instances"))
			} else {
				resp.Body.Close()
			}
		})

		It("returns empty for contradictory service_type and agent_name combination", func() {
			containerAgent := discoverAgentByServiceType("container", "")

			resp, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=vm&agent_name="+containerAgent, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, _ := body["instances"].([]interface{})
			Expect(instances).To(BeEmpty(),
				"contradictory service_type=vm with a container agent should yield empty results")
		})

		It("does not trim whitespace from service_type value", func() {
			for _, padded := range []string{" container", "container ", " container "} {
				encoded := url.QueryEscape(padded)
				resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type="+encoded, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(SatisfyAny(
					Equal(http.StatusOK),
					Equal(http.StatusBadRequest),
				), "whitespace-padded value %q should not cause a 500", padded)

				if resp.StatusCode == http.StatusOK {
					var body map[string]interface{}
					decodeJSON(resp, &body)
					Expect(body).To(HaveKey("instances"))
					instances, _ := body["instances"].([]interface{})
					Expect(instances).To(BeEmpty(),
						"whitespace-padded %q should not match 'container' (no trimming)", padded)
				} else {
					resp.Body.Close()
				}
			}
		})

		It("treats all-whitespace service_type as no filter", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=%20%20%20", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var filtered map[string]interface{}
			decodeJSON(resp, &filtered)
			filteredInstances, _ := filtered["instances"].([]interface{})

			resp2, err := doRequest(http.MethodGet, "/service-type-instances", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp2.StatusCode).To(Equal(http.StatusOK))

			var unfiltered map[string]interface{}
			decodeJSON(resp2, &unfiltered)
			unfilteredInstances, _ := unfiltered["instances"].([]interface{})

			Expect(len(filteredInstances)).To(Equal(len(unfilteredInstances)),
				"all-whitespace service_type should be equivalent to no filter")
		})

		It("handles very long service_type value", func() {
			longValue := ""
			for i := 0; i < 300; i++ {
				longValue += "a"
			}
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type="+longValue, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(SatisfyAny(
				Equal(http.StatusOK),
				Equal(http.StatusBadRequest),
				Equal(http.StatusRequestURITooLong),
			), "very long service_type should not cause a 500")

			if resp.StatusCode == http.StatusOK {
				var body map[string]interface{}
				decodeJSON(resp, &body)
				Expect(body).To(HaveKey("instances"))
				instances, _ := body["instances"].([]interface{})
				Expect(instances).To(BeEmpty(),
					"a 300-char service_type should match nothing")
			} else {
				resp.Body.Close()
			}
		})
	})

	Context("show_deleted combined with service_type filter", func() {
		BeforeEach(func() {
			requireContainerSP()
		})

		It("accepts show_deleted=true alongside service_type filter", func() {
			resp, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&show_deleted=true", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("instances"))

			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(len(instances)).To(BeNumerically(">=", 0),
				"response should be a valid list")
		})

		It("returns consistent results for active instances regardless of show_deleted", func() {
			resp1, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&show_deleted=false", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp1.StatusCode).To(Equal(http.StatusOK))

			var body1 map[string]interface{}
			decodeJSON(resp1, &body1)
			withoutDeleted, _ := body1["instances"].([]interface{})

			resp2, err := doRequest(http.MethodGet,
				"/service-type-instances?service_type=container&show_deleted=true", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp2.StatusCode).To(Equal(http.StatusOK))

			var body2 map[string]interface{}
			decodeJSON(resp2, &body2)
			withDeleted, _ := body2["instances"].([]interface{})

			Expect(len(withDeleted)).To(BeNumerically(">=", len(withoutDeleted)),
				"show_deleted=true should return at least as many results as show_deleted=false")
		})
	})
})
