//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tests for FLPATH-3426 / FLPATH-3427: the SPRM's NATS status consumer.
//
// The container SP publishes CloudEvents to NATS when Pod phase changes.
// The SPRM's StatusConsumer reads these events and calls UpdateStatus on
// its database, which is then reflected in the GET /service-type-instances API.
//
// These tests verify the full round-trip:
//   SP → NATS → SPRM consumer → DB → API response
//
// Existing tests cover adjacent concerns:
//   - sp_container_status_test.go: SP publishing behavior and NATS event format
//   - core_platform_test.go: single happy-path "reaches RUNNING" check
//
// This file focuses specifically on the status reader's behavior as observable
// through the gateway API.

var _ = Describe("Status Reader", Label("nats"), func() {
	Context("status propagation through gateway API", Ordered, func() {
		var containerProviderName string
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			containerProviderName = discoverAgentByServiceType("container", "")

			By("creating a routing policy to direct traffic to the container agent")
			polName := uniqueName("e2e-status-pol")
			pkgName := fmt.Sprintf("e2e_status_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E status reader test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, containerProviderName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item")
			catName := uniqueName("e2e-status")
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

			By("creating a catalog item instance to trigger provisioning")
			instName := uniqueName("e2e-status-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
			GinkgoWriter.Printf("Status reader test: created instance %s (resource_id=%s)\n", instanceID, resourceID)
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

		It("has a status field immediately after creation", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("status"), "instance should have a status field from creation")

			status, ok := body["status"].(string)
			Expect(ok).To(BeTrue(), "status should be a string")
			Expect(status).NotTo(BeEmpty(), "status should not be empty")
			GinkgoWriter.Printf("Initial status: %q\n", status)
		})

		It("reaches RUNNING status via NATS consumer update", func() {
			Eventually(func() string {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if err != nil {
					return ""
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusRunning),
				"SPRM should reflect RUNNING after the NATS consumer processes the SP's status event")
		})

		It("returns status in the list endpoint", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=container", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, inst := range instances {
				i, ok := inst.(map[string]interface{})
				Expect(ok).To(BeTrue())
				if i["id"] == resourceID {
					found = true
					Expect(i).To(HaveKey("status"))
					Expect(i["status"]).To(Equal(instanceStatusRunning),
						"list endpoint should show the same status as individual GET")
					break
				}
			}
			Expect(found).To(BeTrue(), "instance should appear in filtered list")
		})

		It("persists status across repeated reads", func() {
			for i := 0; i < 3; i++ {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				var body map[string]interface{}
				decodeJSON(resp, &body)
				Expect(body["status"]).To(Equal(instanceStatusRunning),
					"status should be consistent across read #%d", i+1)
			}
		})

		It("includes update_time reflecting the status change", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("update_time"))
			Expect(body["update_time"]).NotTo(BeEmpty(),
				"update_time should be set after status consumer updates the record")
		})
	})

	Context("error status propagation", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent for policy creation")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-badimg-pol")
			pkgName := fmt.Sprintf("e2e_badimg_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E bad image test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item with a bad image that will fail to pull")
			catName := uniqueName("e2e-badimg")
			badImage := fmt.Sprintf("quay.io/nonexistent/image-%d:fake", time.Now().UnixNano())
			catPayload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"resources": [{
						"name": "main",
						"service_type": "container",
						"fields": [
							{"path": "metadata.name", "display_name": "Container Name", "editable": true, "default": %q},
							{"path": "image.reference", "display_name": "Image", "editable": true, "default": %q},
							{"path": "resources.cpu.min", "editable": false, "default": "1"},
							{"path": "resources.cpu.max", "editable": false, "default": "1"},
							{"path": "resources.memory.min", "editable": false, "default": "128MB"},
							{"path": "resources.memory.max", "editable": false, "default": "256MB"}
						]
					}]
				}
			}`, catName, catName, badImage)

			resp, err = doRequest(http.MethodPost, "/catalog-items", catPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var catBody map[string]interface{}
			decodeJSON(resp, &catBody)
			catalogItemID, _ = catBody["uid"].(string)
			Expect(catalogItemID).NotTo(BeEmpty())

			By("creating an instance that will encounter ImagePullBackOff")
			instName := uniqueName("e2e-badimg-inst")
			instPayload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"catalog_item_id": %q,
					"user_values": [
						{"path": "metadata.name", "value": %q, "resource": "main"},
						{"path": "image.reference", "value": %q, "resource": "main"}
					]
				}
			}`, instName, catalogItemID, instName, badImage)

			beforeSTIs := listServiceTypeInstanceIDs()
			resp, err = doRequest(http.MethodPost, "/catalog-item-instances", instPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var instBody map[string]interface{}
			decodeJSON(resp, &instBody)
			instanceID, _ = instBody["uid"].(string)
			Expect(instanceID).NotTo(BeEmpty())
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
			GinkgoWriter.Printf("Bad image test: created instance %s (resource_id=%s)\n", instanceID, resourceID)
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

		It("reflects PENDING status for a failing container", func() {
			Eventually(func() string {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if err != nil {
					return ""
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusPending),
				"SPRM API should show pending when the SP reports ImagePullBackOff")
		})

		It("does not transition to RUNNING for a bad image", func() {
			Consistently(func() string {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if err != nil {
					return "error"
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return "error"
				}
				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(15*time.Second).WithPolling(3*time.Second).ShouldNot(Equal(instanceStatusRunning),
				"instance with invalid image should never reach RUNNING")
		})
	})

	Context("independent status updates for concurrent instances", Ordered, func() {
		var policyID, catalogItemID string
		var instanceIDs []string
		var resourceIDs []string

		const instanceCount = 3

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-multi-pol")
			pkgName := fmt.Sprintf("e2e_multi_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E multi-instance test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a shared catalog item")
			catName := uniqueName("e2e-multi")
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

			By(fmt.Sprintf("creating %d instances to verify independent status tracking", instanceCount))
			for i := 0; i < instanceCount; i++ {
				instName := uniqueName(fmt.Sprintf("e2e-multi-%d", i))
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
				resp, err := doRequest(http.MethodPost, "/catalog-item-instances", instPayload)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusCreated))

				var instBody map[string]interface{}
				decodeJSON(resp, &instBody)
				uid, _ := instBody["uid"].(string)
				Expect(uid).NotTo(BeEmpty())
				_, specOK := instBody["spec"].(map[string]interface{})
				Expect(specOK).To(BeTrue(), "spec should be a map")
				rid := resolveResourceIDAfterCreate(instBody, beforeSTIs)
				Expect(rid).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
				instanceIDs = append(instanceIDs, uid)
				resourceIDs = append(resourceIDs, rid)
				GinkgoWriter.Printf("Created instance %d: %s (resource_id=%s)\n", i, uid, rid)
			}
		})

		AfterAll(func() {
			for _, uid := range instanceIDs {
				resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+uid, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
			}
			for _, uid := range instanceIDs {
				Eventually(func() int {
					r, e := doRequest(http.MethodGet, "/catalog-item-instances/"+uid, "")
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

		It("each instance reaches RUNNING independently", func() {
			for i, rid := range resourceIDs {
				Eventually(func() string {
					resp, err := doRequest(http.MethodGet, "/service-type-instances/"+rid, "")
					if err != nil {
						return ""
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						return ""
					}
					var body map[string]interface{}
					decodeJSON(resp, &body)
					s, _ := body["status"].(string)
					return s
				}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusRunning),
					"instance %d (resource_id=%s) should reach RUNNING via independent status update", i, rid)
			}
		})

		It("all instances show RUNNING simultaneously in list", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=container", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			ridSet := make(map[string]bool, len(resourceIDs))
			for _, rid := range resourceIDs {
				ridSet[rid] = true
			}

			var foundCount int
			for _, inst := range instances {
				i, ok := inst.(map[string]interface{})
				Expect(ok).To(BeTrue())
				id, _ := i["id"].(string)
				if ridSet[id] {
					foundCount++
					Expect(i["status"]).To(Equal(instanceStatusRunning),
						"instance %s should be RUNNING in list response", id)
				}
			}
			Expect(foundCount).To(Equal(instanceCount),
				"all %d test instances should appear in the list", instanceCount)
		})
	})

	Context("consumer resilience: fake instance ID", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-fake-pol")
			pkgName := fmt.Sprintf("e2e_fake_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E fake ID test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())
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

		It("publishes a status event for a non-existent instance without crashing the consumer", func() {
			By("connecting to NATS and publishing a fake status event")
			conn, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			fakeID := uuid.New().String()
			event := map[string]interface{}{
				"specversion":     "1.0",
				"id":              uuid.New().String(),
				"source":          "dcm/providers/e2e-fake-provider",
				"type":            "dcm.status.updated",
				"time":            time.Now().Format(time.RFC3339),
				"datacontenttype": "application/json",
				"data": map[string]interface{}{
					"id":        fakeID,
					"status":    "RUNNING",
					"message":   "fake status for non-existent instance",
					"timestamp": time.Now().Format(time.RFC3339),
				},
			}
			payload, err := json.Marshal(event)
			Expect(err).NotTo(HaveOccurred())

			err = conn.Publish("dcm.container", payload)
			Expect(err).NotTo(HaveOccurred())
			conn.Flush()

			By("verifying the control-plane health endpoint is still responsive")
			Eventually(func() int {
				r, e := doRequest(http.MethodGet, "/health", "")
				if e != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Equal(http.StatusOK),
				"control plane should remain healthy after processing event for non-existent instance")
		})

		It("still processes real status events after the fake one", func() {
			By("creating a catalog item and instance")
			catName := uniqueName("e2e-fake-cat")
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

			resp, err := doRequest(http.MethodPost, "/catalog-items", catPayload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var catBody map[string]interface{}
			decodeJSON(resp, &catBody)
			catalogItemID, _ = catBody["uid"].(string)

			instName := uniqueName("e2e-fake-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")

			By("verifying the real instance still reaches RUNNING")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusRunning),
				"consumer should still process real events after handling a fake instance ID")
		})
	})

	Context("consumer resilience: malformed NATS message", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-malform-pol")
			pkgName := fmt.Sprintf("e2e_malform_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E malformed msg test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())
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

		It("publishes malformed data without crashing the consumer", func() {
			By("connecting to NATS and publishing various malformed messages")
			conn, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			malformedPayloads := []struct {
				desc string
				data []byte
			}{
				{"empty bytes", []byte{}},
				{"invalid JSON", []byte("this is not json at all!!!")},
				{"valid JSON but not a CloudEvent", []byte(`{"foo": "bar"}`)},
				{"CloudEvent with empty data", []byte(`{"specversion":"1.0","id":"test","source":"test","type":"test","data":null}`)},
				{"CloudEvent with non-object data", []byte(`{"specversion":"1.0","id":"test","source":"test","type":"test","data":"just a string"}`)},
				{"CloudEvent with missing id in payload", []byte(`{"specversion":"1.0","id":"test","source":"test","type":"test","datacontenttype":"application/json","data":{"status":"RUNNING","message":"no id field"}}`)},
			}

			for _, m := range malformedPayloads {
				err = conn.Publish("dcm.container", m.data)
				Expect(err).NotTo(HaveOccurred(), "failed to publish %s", m.desc)
			}
			conn.Flush()

			By("verifying the control-plane health endpoint is still responsive after all malformed messages")
			Eventually(func() int {
				r, e := doRequest(http.MethodGet, "/health", "")
				if e != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Equal(http.StatusOK),
				"control plane should remain healthy after processing malformed NATS messages")
		})

		It("still processes real status events after malformed ones", func() {
			By("creating a catalog item and instance")
			catName := uniqueName("e2e-malform-cat")
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

			resp, err := doRequest(http.MethodPost, "/catalog-items", catPayload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var catBody map[string]interface{}
			decodeJSON(resp, &catBody)
			catalogItemID, _ = catBody["uid"].(string)

			instName := uniqueName("e2e-malform-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")

			By("verifying the real instance still reaches RUNNING")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(instanceStatusRunning),
				"consumer should recover from malformed messages and process real events")
		})
	})

	Context("status stability after reaching RUNNING", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-stable-pol")
			pkgName := fmt.Sprintf("e2e_stable_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E stability test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item and instance")
			catName := uniqueName("e2e-stable")
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

			instName := uniqueName("e2e-stable-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")

			By("waiting for RUNNING before stability check")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Equal(instanceStatusRunning))
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

		It("does not regress from RUNNING without external cause", func() {
			Consistently(func() string {
				resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if err != nil {
					return "error"
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return "error"
				}
				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(15*time.Second).WithPolling(2*time.Second).Should(Equal(instanceStatusRunning),
				"status should remain RUNNING without any disruption")
		})
	})

	Context("deleted instance returns 404", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string
		var instanceDeleted bool

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-del-pol")
			pkgName := fmt.Sprintf("e2e_del_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E deletion test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item and instance")
			catName := uniqueName("e2e-del")
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

			instName := uniqueName("e2e-del-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")

			By("waiting for RUNNING before deletion")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Equal(instanceStatusRunning))
		})

		AfterAll(func() {
			if !instanceDeleted && instanceID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
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

		It("returns 404 after the instance is deleted", func() {
			By("deleting the catalog-item-instance")
			resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(SatisfyAny(Equal(http.StatusOK), Equal(http.StatusNoContent), Equal(http.StatusAccepted)))
			resp.Body.Close()

			By("waiting for the service-type-instance to become 404")
			Eventually(func() int {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(Equal(http.StatusNotFound),
				"service-type-instance should return 404 after deletion — no ghost status records")
			instanceDeleted = true
		})

		It("does not appear in list results after deletion", func() {
			resp, err := doRequest(http.MethodGet, "/service-type-instances?service_type=container", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			instances, ok := body["instances"].([]interface{})
			Expect(ok).To(BeTrue())

			for _, inst := range instances {
				i, ok := inst.(map[string]interface{})
				Expect(ok).To(BeTrue())
				Expect(i["id"]).NotTo(Equal(resourceID),
					"deleted instance should not appear in list results")
			}
		})
	})

	Context("consumer edge cases: status value handling", Ordered, func() {
		var policyID, catalogItemID, instanceID, resourceID string

		BeforeAll(func() {
			requireContainerSP()

			By("discovering the container agent")
			providerName := discoverAgentByServiceType("container", "")

			By("creating a routing policy")
			polName := uniqueName("e2e-edge-pol")
			pkgName := fmt.Sprintf("e2e_edge_%d", time.Now().UnixNano()%1000000)
			polPayload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": 100,
				"description": "E2E edge case test: route to container agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, polName, pkgName, providerName)

			resp, err := doRequest(http.MethodPost, "/policies", polPayload)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var polBody map[string]interface{}
			decodeJSON(resp, &polBody)
			policyID, _ = polBody["id"].(string)
			Expect(policyID).NotTo(BeEmpty())

			By("creating a catalog item and instance")
			catName := uniqueName("e2e-edge")
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

			instName := uniqueName("e2e-edge-inst")
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
			_, specOK := instBody["spec"].(map[string]interface{})
			Expect(specOK).To(BeTrue(), "spec should be a map")
			resourceID = resolveResourceIDAfterCreate(instBody, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")

			By("waiting for RUNNING before edge case tests")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Equal(instanceStatusRunning))
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

		It("passes through arbitrary status values to the API", func() {
			By("publishing a CloudEvent with a custom status value")
			conn, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			customStatus := "CUSTOM_STATE_12345"
			event := map[string]interface{}{
				"specversion":     "1.0",
				"id":              uuid.New().String(),
				"source":          "dcm/providers/e2e-test",
				"type":            "dcm.status.updated",
				"time":            time.Now().Format(time.RFC3339),
				"datacontenttype": "application/json",
				"data": map[string]interface{}{
					"id":        resourceID,
					"status":    customStatus,
					"message":   "testing arbitrary status pass-through",
					"timestamp": time.Now().Format(time.RFC3339),
				},
			}
			payload, err := json.Marshal(event)
			Expect(err).NotTo(HaveOccurred())

			err = conn.Publish("dcm.container", payload)
			Expect(err).NotTo(HaveOccurred())
			conn.Flush()

			By("verifying the API reflects the custom status verbatim")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Equal(strings.ToLower(customStatus)),
				"consumer stores status lowercased; arbitrary values are not rejected")
		})

		// This tests ordered delivery within a single NATS publisher connection.
		// NATS guarantees ordering per publisher, so the consumer processes RUNNING
		// then PENDING sequentially. Cross-publisher concurrent ordering is not
		// tested here — that would require multiple connections publishing
		// simultaneously, which introduces non-deterministic delivery order.
		It("reflects last-write-wins when multiple status events arrive", func() {
			By("publishing RUNNING then immediately PENDING (ordered within one publisher)")
			conn, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			for _, status := range []string{"RUNNING", "PENDING"} {
				event := map[string]interface{}{
					"specversion":     "1.0",
					"id":              uuid.New().String(),
					"source":          "dcm/providers/e2e-test",
					"type":            "dcm.status.updated",
					"time":            time.Now().Format(time.RFC3339),
					"datacontenttype": "application/json",
					"data": map[string]interface{}{
						"id":        resourceID,
						"status":    status,
						"message":   fmt.Sprintf("last-write-wins test: %s", status),
						"timestamp": time.Now().Format(time.RFC3339),
					},
				}
				payload, err := json.Marshal(event)
				Expect(err).NotTo(HaveOccurred())
				err = conn.Publish("dcm.container", payload)
				Expect(err).NotTo(HaveOccurred())
			}
			conn.Flush()

			By("verifying the API shows PENDING (the last published event)")
			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return ""
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return ""
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Equal(instanceStatusPending),
				"consumer should apply last-write-wins — no forward-only constraint on status")
		})

		It("applies an empty status string from a status event", func() {
			By("first setting status to a known value")
			conn, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			knownStatus := "KNOWN_STATE"
			event := map[string]interface{}{
				"specversion":     "1.0",
				"id":              uuid.New().String(),
				"source":          "dcm/providers/e2e-test",
				"type":            "dcm.status.updated",
				"time":            time.Now().Format(time.RFC3339),
				"datacontenttype": "application/json",
				"data": map[string]interface{}{
					"id":        resourceID,
					"status":    knownStatus,
					"message":   "setting known state",
					"timestamp": time.Now().Format(time.RFC3339),
				},
			}
			payload, err := json.Marshal(event)
			Expect(err).NotTo(HaveOccurred())
			err = conn.Publish("dcm.container", payload)
			Expect(err).NotTo(HaveOccurred())
			conn.Flush()

			Eventually(func() string {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return "error"
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return "error"
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(Equal(strings.ToLower(knownStatus)))

			By("publishing an event with empty status string")
			emptyEvent := map[string]interface{}{
				"specversion":     "1.0",
				"id":              uuid.New().String(),
				"source":          "dcm/providers/e2e-test",
				"type":            "dcm.status.updated",
				"time":            time.Now().Format(time.RFC3339),
				"datacontenttype": "application/json",
				"data": map[string]interface{}{
					"id":        resourceID,
					"status":    "",
					"message":   "empty status test",
					"timestamp": time.Now().Format(time.RFC3339),
				},
			}
			payload, err = json.Marshal(emptyEvent)
			Expect(err).NotTo(HaveOccurred())
			err = conn.Publish("dcm.container", payload)
			Expect(err).NotTo(HaveOccurred())
			conn.Flush()

			// StatusConsumer always UpdateStatus after ToLower/TrimSpace, and the
			// store Select("status", "status_message") so an empty string overwrites.
			By("verifying the API reflects the empty status")
			Eventually(func() (string, error) {
				r, e := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if e != nil {
					return "", e
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					return "", fmt.Errorf("GET status %d", r.StatusCode)
				}
				var body map[string]interface{}
				decodeJSON(r, &body)
				s, _ := body["status"].(string)
				return s, nil
			}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Equal(""),
				"empty status in the CloudEvent is persisted, not treated as a no-op")
		})
	})

	// NOTE: "status update on soft-deleted instance" is not testable via the
	// gateway API because DELETE /service-type-instances/{id}?deferred=true is
	// an internal SPRM endpoint not exposed through the gateway. The deferred
	// deletion flow is triggered internally during rehydration. This scenario
	// would need to be tested at the unit/integration level within the SPRM repo.
})
