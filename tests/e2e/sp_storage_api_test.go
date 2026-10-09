//go:build e2e

package e2e_test

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Storage SP API", Label("sp", "storage"), func() {
	BeforeEach(func() {
		if !waitForAgentEmbed("storage", 30*time.Second) {
			Skip("Storage provider is not Ready through Environment Agent")
		}
	})

	It("registers with DCM as a storage agent", func() {
		resp, err := doRequest(http.MethodGet, "/agents", "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var body map[string]interface{}
		decodeJSON(resp, &body)
		agents, ok := body["agents"].([]interface{})
		Expect(ok).To(BeTrue(), "expected agents array")

		override := os.Getenv("DCM_STORAGE_AGENT_NAME")
		var found map[string]interface{}
		for _, raw := range agents {
			agent, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := agent["name"].(string)
			if override != "" && name != override {
				continue
			}
			serviceTypes, _ := agent["service_types"].([]interface{})
			for _, rawServiceType := range serviceTypes {
				serviceType, _ := rawServiceType.(string)
				if strings.EqualFold(serviceType, "storage") {
					found = agent
					break
				}
			}
			if found != nil {
				break
			}
		}

		Expect(found).NotTo(BeNil(), "no agent advertising service type %q found", "storage")
		if override != "" {
			Expect(found).To(HaveKeyWithValue("name", override))
		}
	})

	It("exposes the storage service type schema", func() {
		resp, err := doRequest(http.MethodGet, "/service-types", "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var body map[string]interface{}
		decodeJSON(resp, &body)
		results, ok := body["results"].([]interface{})
		Expect(ok).To(BeTrue(), "expected service type results")

		var storage map[string]interface{}
		for _, raw := range results {
			serviceType, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if serviceType["service_type"] == "storage" {
				storage = serviceType
				break
			}
		}

		Expect(storage).NotTo(BeNil(), "storage service type is not present in the catalog")
		Expect(storage).To(HaveKeyWithValue("uid", "storage"))
		Expect(storage).To(HaveKeyWithValue("path", "service-types/storage"))
		Expect(storage["spec"]).To(BeEquivalentTo(map[string]interface{}{
			"capacity":    "",
			"volume_name": "",
		}))
	})

	It("creates a PVC through a catalog item instance", func() {
		requireKubectl()

		agentName := discoverAgentByServiceType("storage", os.Getenv("DCM_STORAGE_AGENT_NAME"))
		name := uniqueName("e2e-storage-pvc")
		resourceID, instanceID, status, body := createEmbeddedInstance("storage", name, []map[string]interface{}{
			{"path": "metadata.name", "value": name, "resource": "volume"},
			{"path": "capacity", "value": "1Gi", "resource": "volume"},
		})
		Expect(status).To(Equal(http.StatusCreated), "catalog item instance response: %#v", body)
		Expect(instanceID).NotTo(BeEmpty())
		Expect(resourceID).NotTo(BeEmpty())

		var instance map[string]interface{}
		Eventually(func() string {
			code, current, err := getSTI(resourceID)
			if err != nil || code != http.StatusOK {
				return ""
			}
			instance = current
			value, _ := current["status"].(string)
			if value == instanceStatusRunning || value == instanceStatusProvisioning {
				return instanceStatusProvisioning
			}
			return value
		}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Equal(instanceStatusProvisioning))
		Expect(instance["agent_name"]).To(Equal(agentName))

		var pvc map[string]interface{}
		Eventually(func() bool {
			items, err := getPVCsInNamespace(spNamespace)
			if err != nil {
				return false
			}
			for _, item := range items {
				metadata, _ := item["metadata"].(map[string]interface{})
				if metadata["name"] == resourceID {
					pvc = item
					return true
				}
			}
			return false
		}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"PVC named after service-type-instance %s should be created", resourceID)

		spec, ok := pvc["spec"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "PVC should include spec")
		Expect(spec["storageClassName"]).To(Equal(storageClassForE2E()))
		Expect(spec["accessModes"]).To(ConsistOf("ReadWriteOnce"))
		resources, ok := spec["resources"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "PVC should include resource requests")
		requests, ok := resources["requests"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "PVC should include resource requests")
		Expect(requests["storage"]).To(Equal("1Gi"))

		pvcStatus, _ := pvc["status"].(map[string]interface{})
		phase, _ := pvcStatus["phase"].(string)
		Expect([]string{"Bound", "Pending"}).To(ContainElement(phase))
	})

	It("lists and gets storage instances through SPRM", func() {
		agentName := discoverAgentByServiceType("storage", os.Getenv("DCM_STORAGE_AGENT_NAME"))
		name := uniqueName("e2e-storage-list")
		resourceID, instanceID, status, body := createEmbeddedInstance("storage", name, []map[string]interface{}{
			{"path": "metadata.name", "value": name, "resource": "volume"},
			{"path": "capacity", "value": "1Gi", "resource": "volume"},
		})
		Expect(status).To(Equal(http.StatusCreated), "catalog item instance response: %#v", body)
		Expect(instanceID).NotTo(BeEmpty())
		Expect(resourceID).NotTo(BeEmpty())

		var listed map[string]interface{}
		Eventually(func() bool {
			code, current, err := listSTIs("storage", url.Values{})
			if err != nil || code != http.StatusOK {
				return false
			}
			instances, _ := current["instances"].([]interface{})
			for _, raw := range instances {
				candidate, ok := raw.(map[string]interface{})
				if !ok || candidate["id"] != resourceID {
					continue
				}
				listed = candidate
				return true
			}
			return false
		}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"storage instance %s should appear in the filtered list", resourceID)

		Expect(listed["agent_name"]).To(Equal(agentName))
		Expect([]string{instanceStatusPending, instanceStatusRunning, instanceStatusProvisioning}).To(ContainElement(listed["status"]))

		code, fetched, err := getSTI(resourceID)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
		Expect(fetched["id"]).To(Equal(resourceID))
		Expect(fetched["agent_name"]).To(Equal(agentName))
		Expect([]string{instanceStatusPending, instanceStatusRunning, instanceStatusProvisioning}).To(ContainElement(fetched["status"]))

		spec, ok := fetched["spec"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "storage instance should include spec")
		Expect(spec["service_type"]).To(Equal("storage"))
		Expect(spec["capacity"]).To(Equal("1Gi"))
		metadata, ok := spec["metadata"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "storage instance spec should include metadata")
		Expect(metadata["name"]).To(Equal(name))
		hints, ok := spec["provider_hints"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "storage instance spec should include provider hints")
		kubernetes, ok := hints["kubernetes"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "storage instance spec should include Kubernetes provider hints")
		Expect(kubernetes["storage_class"]).To(Equal(storageClassForE2E()))
		Expect(kubernetes["access_mode"]).To(Equal("ReadWriteOnce"))
	})

	It("deletes a storage instance and its PVC through SPRM", func() {
		requireKubectl()

		name := uniqueName("e2e-storage-delete")
		resourceID, instanceID, status, body := createEmbeddedInstance("storage", name, []map[string]interface{}{
			{"path": "metadata.name", "value": name, "resource": "volume"},
			{"path": "capacity", "value": "1Gi", "resource": "volume"},
		})
		Expect(status).To(Equal(http.StatusCreated), "catalog item instance response: %#v", body)
		Expect(instanceID).NotTo(BeEmpty())
		Expect(resourceID).NotTo(BeEmpty())

		Eventually(func() bool {
			items, err := getPVCsInNamespace(spNamespace)
			if err != nil {
				return false
			}
			for _, item := range items {
				metadata, _ := item["metadata"].(map[string]interface{})
				if metadata["name"] == resourceID {
					return true
				}
			}
			return false
		}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"PVC named after service-type-instance %s should be created", resourceID)

		resp, err := doRequest(http.MethodDelete, "/service-type-instances/"+resourceID, "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect([]int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}).To(ContainElement(resp.StatusCode))

		Eventually(func() int {
			code, _, err := getSTI(resourceID)
			if err != nil {
				return 0
			}
			return code
		}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(Equal(http.StatusNotFound))

		Eventually(func() bool {
			items, err := getPVCsInNamespace(spNamespace)
			if err != nil {
				return false
			}
			for _, item := range items {
				metadata, _ := item["metadata"].(map[string]interface{})
				if metadata["name"] == resourceID {
					return false
				}
			}
			return true
		}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"PVC %s should be removed after deleting the storage instance", resourceID)

		catalogResp, err := doRequest(http.MethodDelete, "/catalog-item-instances/"+instanceID, "")
		Expect(err).NotTo(HaveOccurred())
		defer catalogResp.Body.Close()
		Expect([]int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}).To(ContainElement(catalogResp.StatusCode),
			"catalog-item instance cleanup must succeed after SPRM deletion")
	})
})

func storageClassForE2E() string {
	if value := os.Getenv("DCM_STORAGE_CLASS"); value != "" {
		return value
	}
	return "hostpath-csi"
}
