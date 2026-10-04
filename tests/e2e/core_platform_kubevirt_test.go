//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Core Platform - KubeVirt Provider", Label("core", "platform", "kubevirt"), func() {
	Context("VM provisioning happy path", Ordered, func() {
		var kubevirtProviderName string
		var catalogItemID, policyID, instanceID, resourceID string
		var instanceDisplayName string

		BeforeAll(func() {
			requireKubevirtSP()

			if err := checkClusterAccess(); err != nil {
				Skip("kubectl/oc cluster access required for core platform test")
			}
			// Boot disk is containerDisk (no PVC); StorageClass is not required for create.
		})

		AfterAll(func() {
			if instanceID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
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
			if catalogItemID != "" {
				resp, err := doRequest(http.MethodDelete, "/catalog-items/"+catalogItemID, "")
				if err == nil && resp != nil {
					resp.Body.Close()
				}
			}
		})

		It("discovers the KubeVirt agent with registration fields [TC-01]", func() {
			resp, err := doRequest(http.MethodGet, "/agents", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("agents"))

			agents, ok := body["agents"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(agents).NotTo(BeEmpty(), "no agents registered")

			var p map[string]interface{}
			for _, raw := range agents {
				cand, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := cand["name"].(string)
				serviceTypes, _ := cand["service_types"].([]interface{})
				hasVM := false
				for _, st := range serviceTypes {
					if s, _ := st.(string); s == "vm" {
						hasVM = true
						break
					}
				}
				if hasVM && strings.Contains(strings.ToLower(name), "kubevirt") {
					p = cand
					break
				}
			}
			if p == nil {
				for _, raw := range agents {
					cand, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					serviceTypes, _ := cand["service_types"].([]interface{})
					for _, st := range serviceTypes {
						if s, _ := st.(string); s == "vm" {
							p = cand
							break
						}
					}
					if p != nil {
						break
					}
				}
			}
			Expect(p).NotTo(BeNil(), "no agent with service_types containing 'vm' found")

			kubevirtProviderName, _ = p["name"].(string)
			Expect(kubevirtProviderName).NotTo(BeEmpty())

			serviceTypes, _ := p["service_types"].([]interface{})
			hasVM := false
			for _, st := range serviceTypes {
				if s, _ := st.(string); s == "vm" {
					hasVM = true
					break
				}
			}
			Expect(hasVM).To(BeTrue(), "agent should have 'vm' in service_types")

			GinkgoWriter.Printf("Using KubeVirt agent: %s\n", kubevirtProviderName)
		})

		It("verifies the vm service type exists", func() {
			resp, err := doRequest(http.MethodGet, "/service-types", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("results"))

			results, ok := body["results"].([]interface{})
			Expect(ok).To(BeTrue())

			var found bool
			for _, r := range results {
				st, ok := r.(map[string]interface{})
				Expect(ok).To(BeTrue())
				if stype, _ := st["service_type"].(string); stype == "vm" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "no service type with service_type=vm found")
		})

		It("creates a catalog item for VM", func() {
			name := uniqueName("e2e-kubevirt")
			// Use whole-array path storage.disks (not disks[0].*) — control-plane
			// nested_map only splits on '.', so indexed segments become literal keys.
			payload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"resources": [
						{
							"name": "vm",
							"service_type": "vm",
							"fields": [
								{"path": "metadata.name", "display_name": "VM Name", "editable": true, "default": %q},
								{"path": "guest_os.type", "display_name": "Guest OS", "editable": true, "default": "linux"},
								{"path": "vcpu.count", "editable": false, "default": 1},
								{"path": "memory.size", "editable": false, "default": "1GB"},
								{"path": "storage.disks", "editable": false, "default": [{"name":"boot","capacity":"10GB"}]}
							]
						}
					]
				}
			}`, name, name)

			resp, err := doRequest(http.MethodPost, "/catalog-items", payload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			uid, ok := body["uid"].(string)
			Expect(ok).To(BeTrue())
			catalogItemID = uid

			getResp, err := doRequest(http.MethodGet, "/catalog-items/"+catalogItemID, "")
			Expect(err).NotTo(HaveOccurred())
			defer getResp.Body.Close()
			Expect(getResp.StatusCode).To(Equal(http.StatusOK))
			var got map[string]interface{}
			decodeJSON(getResp, &got)
			Expect(got["display_name"]).To(Equal(name))
			spec, ok := got["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue())
			resources, ok := spec["resources"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(resources).To(HaveLen(1))
			res, ok := resources[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(res["service_type"]).To(Equal("vm"))
			fields, ok := res["fields"].([]interface{})
			Expect(ok).To(BeTrue())
			fieldPaths := map[string]bool{}
			for _, f := range fields {
				fm, ok := f.(map[string]interface{})
				Expect(ok).To(BeTrue())
				path, _ := fm["path"].(string)
				fieldPaths[path] = true
			}
			Expect(fieldPaths).To(HaveKey("memory.size"))
			Expect(fieldPaths).To(HaveKey("storage.disks"))
		})

		It("creates a routing policy for KubeVirt provider", func() {
			Expect(kubevirtProviderName).NotTo(BeEmpty())

			name := uniqueName("e2e-kubevirt-policy")
			pkgName := fmt.Sprintf("e2e_kubevirt_%d", time.Now().UnixNano()%1000000)
			priority := int(time.Now().UnixNano()%999) + 1
			payload := fmt.Sprintf(`{
				"display_name": %q,
				"policy_type": "GLOBAL",
				"priority": %d,
				"description": "E2E test: route to KubeVirt agent",
				"rego_code": "package %s\n\nmain := {\"selected_agent\": \"%s\"}"
			}`, name, priority, pkgName, kubevirtProviderName)

			resp, err := doRequest(http.MethodPost, "/policies", payload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			id, ok := body["id"].(string)
			Expect(ok).To(BeTrue())
			policyID = id

			getResp, err := doRequest(http.MethodGet, "/policies/"+policyID, "")
			Expect(err).NotTo(HaveOccurred())
			defer getResp.Body.Close()
			Expect(getResp.StatusCode).To(Equal(http.StatusOK))
			var got map[string]interface{}
			decodeJSON(getResp, &got)
			Expect(got["policy_type"]).To(Equal("GLOBAL"))
			Expect(got["priority"]).To(BeNumerically("==", priority))
			rego, _ := got["rego_code"].(string)
			Expect(rego).To(ContainSubstring(kubevirtProviderName))
			Expect(rego).To(ContainSubstring("selected_agent"))
		})

		It("creates a catalog item instance for VM [TC-23]", func() {
			Expect(catalogItemID).NotTo(BeEmpty())

			instanceDisplayName = uniqueName("e2e-kubevirt-inst")
			payload := fmt.Sprintf(`{
				"api_version": "v1alpha1",
				"display_name": %q,
				"spec": {
					"catalog_item_id": %q,
					"user_values": [
						{"resource": "vm", "path": "metadata.name", "value": %q},
						{"resource": "vm", "path": "guest_os.type", "value": "linux"}
					]
				}
			}`, instanceDisplayName, catalogItemID, instanceDisplayName)

			beforeSTIs := listServiceTypeInstanceIDs()
			resp, err := doRequest(http.MethodPost, "/catalog-item-instances", payload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			instanceID, _ = body["uid"].(string)
			Expect(instanceID).NotTo(BeEmpty())

			resourceID = resolveResourceIDAfterCreate(body, beforeSTIs)
			Expect(resourceID).NotTo(BeEmpty(), "placement resource ID should be discoverable after create")
		})

		It("VM reaches Running status on STI and cluster [TC-23]", func() {
			Expect(resourceID).NotTo(BeEmpty())

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
				status, _ := body["status"].(string)
				GinkgoWriter.Printf("VM status: %s\n", status)
				return status
			}).WithTimeout(600 * time.Second).WithPolling(10 * time.Second).Should(Equal(instanceStatusRunning),
				"VM should reach running status on the service type instance")

			// Cluster verification: STI id is the DCM instance id used as SP VM id
			ns := kubevirtNamespace()
			Eventually(func() error {
				name, err := findVMNameByInstanceID(resourceID, ns)
				if err != nil {
					return err
				}
				return verifyDCMLabels(name, ns, resourceID)
			}).WithTimeout(120 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("has correct provider assignment", func() {
			Expect(resourceID).NotTo(BeEmpty())

			resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body["status"]).To(Equal(instanceStatusRunning))
			Expect(body["agent_name"]).To(Equal(kubevirtProviderName))
		})

		It("deletes catalog instance and removes VM [TC-24]", func() {
			Expect(instanceID).NotTo(BeEmpty())
			Expect(resourceID).NotTo(BeEmpty())

			ns := kubevirtNamespace()
			clusterName, err := findVMNameByInstanceID(resourceID, ns)
			Expect(err).NotTo(HaveOccurred())

			resp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
			Expect(err).NotTo(HaveOccurred())
			if resp != nil {
				resp.Body.Close()
			}
			instanceID = "" // AfterAll should not double-delete

			Eventually(func() int {
				r, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
				if err != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}).WithTimeout(180 * time.Second).WithPolling(5 * time.Second).
				Should(Or(Equal(http.StatusNotFound), Equal(http.StatusGone)))

			Eventually(func() error {
				return verifyVMDeleted(clusterName, ns)
			}).WithTimeout(180 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})
	})
})
