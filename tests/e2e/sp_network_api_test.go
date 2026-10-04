//go:build e2e

package e2e_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	defaultAgentURL              = "http://localhost:8081/api/v1alpha1"
	networkSPEnabledEnv          = "DCM_NETWORK_SP_ENABLED"
	networkProviderServiceType   = "network"
	networkProviderType          = "embedded"
	networkProviderStatusReady   = "Ready"
	networkResourceStatusReady   = "ready"
	networkResourceStatusPending = "pending"
	networkResourceStatusFailed  = "failed"
	networkStatusUnavailable     = "unavailable"
)

var (
	networkAgentBaseURL string
	networkSPEnabled    bool
	networkSPReady      bool
	networkSPOnce       sync.Once
)

type networkAgentProvider struct {
	ServiceType string `json:"service_type"`
	Status      string `json:"status"`
	Type        string `json:"type"`
}

type networkAgentProviderList struct {
	Results []networkAgentProvider `json:"results"`
}

type networkCRUDResource struct {
	PolicyID      string
	CatalogItemID string
	InstanceID    string
	ResourceID    string
	ServiceName   string
	Namespace     string
	NodePort      int
}

var _ = Describe("Network SP API", Label("sp", "network"), func() {
	var networkAgentName string

	Context("Phase A wiring", Label("lab-default"), Ordered, func() {
		BeforeAll(func() {
			requireNetworkSP()
		})

		It("E2E-01 verifies control-plane and embedded agent health", func() {
			assertControlPlaneHealth()
			assertEmbeddedAgentHealth()
			waitForEmbeddedNetworkProvider()
		})

		It("E2E-02 discovers a registered network agent", func() {
			override := os.Getenv("DCM_NETWORK_AGENT_NAME")
			networkAgentName = discoverAgentByServiceType("network", override)
			Expect(networkAgentName).NotTo(BeEmpty())
			GinkgoWriter.Printf("Selected network agent: %s\n", networkAgentName)
		})

		It("E2E-03 verifies the network service type schema", func() {
			serviceType := fetchNetworkServiceType()
			Expect(serviceType["service_type"]).To(Equal("network"))
			Expect(serviceType["api_version"]).To(Equal("v1alpha1"))

			spec, ok := serviceType["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "network service type must include a schema object")
			// Control-plane currently advertises the network shape with ports and
			// endpoints. routing_level is used on catalog fields for inference
			// but is not always present on the service-type schema object.
			Expect(spec).To(HaveKey("ports"))
			Expect(spec).To(HaveKey("endpoints"))
			if _, hasRouting := spec["routing_level"]; hasRouting {
				Expect(spec["routing_level"]).NotTo(BeNil())
			}
		})
	})

	Context("Phase B CRUD", Label("crud", "cluster"), func() {
		Context("ClusterIP inference", Label("clusterip"), Ordered, func() {
			var resource networkCRUDResource
			var deleted bool
			BeforeAll(func() {
				requireNetworkSP()
				requireKubectl()
			})
			AfterAll(func() {
				if !deleted {
					cleanupNetworkResource(resource)
				}
			})
			It("E2E-04 creates and reads a ClusterIP network", func() {
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-clusterip", selectorKey: "app", selectorVal: "e2e-network",
				}
				resource = newNetworkResource(opts)
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkReady(resource.ResourceID)
				assertNetworkService(resource, "ClusterIP", "", true, "")
				assertNetworkInstance(resource, agentName, networkResourceStatusReady)
			})
			It("E2E-05 deletes the ClusterIP network and its Kubernetes Service", func() {
				deleteNetworkResource(resource)
				deleted = true
			})
		})

		Context("NodePort inference", Label("nodeport"), Ordered, func() {
			var resource networkCRUDResource
			var deleted bool
			BeforeAll(func() {
				requireNetworkSP()
				requireKubectl()
			})
			AfterAll(func() {
				if !deleted {
					cleanupNetworkResource(resource)
				}
			})
			It("E2E-09 creates and reads a NodePort network", func() {
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-nodeport", nodePort: findUnusedNodePort(), selectorKey: "app", selectorVal: "e2e-network",
				}
				resource = newNetworkResource(opts)
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkReady(resource.ResourceID)
				assertNetworkService(resource, "NodePort", "", false, "http")
				assertNetworkInstance(resource, agentName, networkResourceStatusReady)
			})
			It("deletes the NodePort network and its Kubernetes Service", func() {
				deleteNetworkResource(resource)
				deleted = true
			})
		})

		Context("Independent network operations", func() {
			BeforeEach(func() {
				requireNetworkSP()
			})

			It("E2E-06 creates a LoadBalancer network without node ports", Label("no-lb-controller"), func() {
				requireKubectl()
				requireLoadBalancerMode("none")
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-loadbalancer", routingLevel: "network", selectorKey: "app", selectorVal: "e2e-network",
				}
				resource := newNetworkResource(opts)
				DeferCleanup(func() { cleanupNetworkResource(resource) })
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkPending(resource.ResourceID, resource.ServiceName, 2*time.Minute)
				assertNetworkService(resource, "LoadBalancer", "", false, "")
				assertNetworkServiceHasNoExternalIP(resource)
				assertNetworkInstance(resource, agentName, networkResourceStatusPending)
			})

			It("E2E-12 creates a controller-backed LoadBalancer network", Label("requires-lb-controller"), func() {
				requireKubectl()
				requireLoadBalancerController()
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-metallb", routingLevel: "network", selectorKey: "app", selectorVal: "e2e-network",
				}
				resource := newNetworkResource(opts)
				DeferCleanup(func() { cleanupNetworkResource(resource) })
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkReady(resource.ResourceID)
				assertNetworkService(resource, "LoadBalancer", "", false, "")
				assertNetworkServiceHasExternalIP(resource)
				assertNetworkInstance(resource, agentName, networkResourceStatusReady)
			})

			It("E2E-10 creates a LoadBalancer network with a node port", Label("loadbalancer"), func() {
				requireKubectl()
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-loadbalancer-np", routingLevel: "network", nodePort: findUnusedNodePort(), selectorKey: "app", selectorVal: "e2e-network",
				}
				resource := newNetworkResource(opts)
				DeferCleanup(func() { cleanupNetworkResource(resource) })
				provisionNetworkResource(&resource, agentName, opts)
				mode := loadBalancerMode()
				if mode == networkStatusUnavailable {
					Fail("load-balancer capability could not be determined; set DCM_NETWORK_LB_MODE explicitly")
				}
				if mode != "none" {
					waitForNetworkReady(resource.ResourceID)
				} else {
					waitForNetworkPending(resource.ResourceID, resource.ServiceName, 2*time.Minute)
				}
				assertNetworkService(resource, "LoadBalancer", "", false, "http")
				if mode != "none" {
					assertNetworkServiceHasExternalIP(resource)
					assertNetworkInstance(resource, agentName, networkResourceStatusReady)
				} else {
					assertNetworkInstance(resource, agentName, networkResourceStatusPending)
				}
			})

			It("E2E-13 creates a headless ClusterIP network", Label("headless"), func() {
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-headless", clusterIP: "None", selectorKey: "app", selectorVal: "e2e-network",
				}
				resource := newNetworkResource(opts)
				DeferCleanup(func() { cleanupNetworkResource(resource) })
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkReady(resource.ResourceID)
				assertNetworkService(resource, "ClusterIP", "None", false, "")
				assertNetworkInstance(resource, agentName, networkResourceStatusReady)
			})

			It("E2E-11 rejects application routing and does not create a Service", Label("contract", "unsupported"), func() {
				agentName := discoverAgentByServiceType("network", os.Getenv("DCM_NETWORK_AGENT_NAME"))
				opts := networkSpecOptions{
					namePrefix: "e2e-network-unsupported", routingLevel: "application", selectorKey: "app", selectorVal: "e2e-network",
				}
				resource := newNetworkResource(opts)
				DeferCleanup(func() { cleanupNetworkResource(resource) })
				provisionNetworkResource(&resource, agentName, opts)
				waitForNetworkFailed(resource.ResourceID)
				assertNetworkServiceAbsent(resource)
			})

			It("E2E-08 rejects an incomplete catalog-item instance request", Label("contract"), func() {
				resp, err := doRequest(http.MethodPost, "/catalog-item-instances", "{}")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				expectRFC9457Problem(resp, problemDetailExpectation{
					Status: http.StatusBadRequest,
				})
			})
		})
	})
})

type networkSpecOptions struct {
	namePrefix   string
	clusterIP    string
	nodePort     int
	routingLevel string
	selectorKey  string
	selectorVal  string
}

func newNetworkResource(opts networkSpecOptions) networkCRUDResource {
	GinkgoHelper()
	return networkCRUDResource{
		ServiceName: uniqueName(opts.namePrefix),
		Namespace:   networkTestNamespace(),
		NodePort:    opts.nodePort,
	}
}

func provisionNetworkResource(resource *networkCRUDResource, agentName string, opts networkSpecOptions) {
	GinkgoHelper()
	resource.PolicyID = createNetworkPolicy(agentName)
	resource.CatalogItemID = createNetworkCatalogItem(resource.ServiceName, opts)
	resource.InstanceID, resource.ResourceID = createNetworkInstance(*resource, opts)
}

func createNetworkPolicy(agentName string) string {
	GinkgoHelper()
	packageName := fmt.Sprintf("e2e_network_%d", time.Now().UnixNano()%1000000)
	payload := map[string]interface{}{
		"display_name": uniqueName("e2e-network-policy"),
		"policy_type":  "GLOBAL",
		"priority":     100,
		"description":  "E2E network Phase B routing policy",
		"rego_code":    fmt.Sprintf("package %s\n\nmain := {\"selected_agent\": \"%s\"}", packageName, agentName),
	}
	resp := postNetworkJSON("/policies", payload)
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	var policy map[string]interface{}
	decodeJSON(resp, &policy)
	id, _ := policy["id"].(string)
	Expect(id).NotTo(BeEmpty())
	return id
}

func createNetworkCatalogItem(serviceName string, opts networkSpecOptions) string {
	GinkgoHelper()
	fields := []map[string]interface{}{
		{"path": "metadata.name", "display_name": "Name", "editable": true, "default": serviceName},
		{"path": "ports", "display_name": "Ports", "editable": true, "default": networkPorts()},
		{"path": "provider_hints.kubernetes.selector", "display_name": "Selector", "editable": true, "default": networkSelector(opts)},
	}
	if opts.clusterIP != "" {
		fields = append(fields, map[string]interface{}{
			"path": "provider_hints.kubernetes.cluster_ip", "display_name": "Cluster IP", "editable": true, "default": opts.clusterIP,
		})
	}
	if opts.nodePort != 0 {
		fields = append(fields, map[string]interface{}{
			"path": "provider_hints.kubernetes.node_ports", "display_name": "Node ports", "editable": true, "default": map[string]int{"http": opts.nodePort},
		})
	}
	if opts.routingLevel != "" {
		fields = append(fields, map[string]interface{}{
			"path": "routing_level", "display_name": "Routing level", "editable": true, "default": opts.routingLevel,
		})
	}
	payload := map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": uniqueName("e2e-network-catalog"),
		"spec": map[string]interface{}{
			"resources": []interface{}{
				map[string]interface{}{"name": "main", "service_type": "network", "fields": fields},
			},
		},
	}
	resp := postNetworkJSON("/catalog-items", payload)
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	var catalog map[string]interface{}
	decodeJSON(resp, &catalog)
	id, _ := catalog["uid"].(string)
	Expect(id).NotTo(BeEmpty())
	return id
}

func createNetworkInstance(resource networkCRUDResource, opts networkSpecOptions) (string, string) {
	GinkgoHelper()
	payload := map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": resource.ServiceName,
		"spec": map[string]interface{}{
			"catalog_item_id": resource.CatalogItemID,
			"user_values":     networkUserValues(resource.ServiceName, opts),
		},
	}
	beforeSTIs := listServiceTypeInstanceIDs()
	resp := postNetworkJSON("/catalog-item-instances", payload)
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	var instance map[string]interface{}
	decodeJSON(resp, &instance)
	instanceID, _ := instance["uid"].(string)
	Expect(instanceID).NotTo(BeEmpty())
	resourceID := resolveResourceIDAfterCreate(instance, beforeSTIs)
	Expect(resourceID).NotTo(BeEmpty())
	return instanceID, resourceID
}

func networkPorts() []map[string]interface{} {
	return []map[string]interface{}{{"name": "http", "port": 80, "target_port": 8080, "protocol": "TCP"}}
}

func networkSelector(opts networkSpecOptions) map[string]string {
	return map[string]string{opts.selectorKey: opts.selectorVal}
}

func networkUserValues(serviceName string, opts networkSpecOptions) []map[string]interface{} {
	values := []map[string]interface{}{
		{"path": "metadata.name", "value": serviceName, "resource": "main"},
		{"path": "ports", "value": networkPorts(), "resource": "main"},
		{"path": "provider_hints.kubernetes.selector", "value": networkSelector(opts), "resource": "main"},
	}
	if opts.clusterIP != "" {
		values = append(values, map[string]interface{}{
			"path": "provider_hints.kubernetes.cluster_ip", "value": opts.clusterIP, "resource": "main",
		})
	}
	if opts.nodePort != 0 {
		values = append(values, map[string]interface{}{
			"path": "provider_hints.kubernetes.node_ports", "value": map[string]int{"http": opts.nodePort}, "resource": "main",
		})
	}
	if opts.routingLevel != "" {
		values = append(values, map[string]interface{}{
			"path": "routing_level", "value": opts.routingLevel, "resource": "main",
		})
	}
	return values
}

const (
	nodePortMin = 30000
	nodePortMax = 32767
)

func findUnusedNodePort() int {
	GinkgoHelper()
	requireKubectl()
	out, err := runKubectlAllNamespaces("get", "services", "--all-namespaces", "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "listing allocated NodePorts")
	var services struct {
		Items []struct {
			Spec struct {
				Ports []struct {
					NodePort int `json:"nodePort"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	Expect(json.Unmarshal([]byte(out), &services)).To(Succeed())
	allocated := make(map[int]struct{})
	for _, service := range services.Items {
		for _, port := range service.Spec.Ports {
			allocated[port.NodePort] = struct{}{}
		}
	}
	portRange := nodePortMax - nodePortMin + 1
	start := nodePortMin + int(time.Now().UnixNano()%int64(portRange))
	for offset := 0; offset < portRange; offset++ {
		candidate := nodePortMin + (start-nodePortMin+offset)%portRange
		if _, inUse := allocated[candidate]; !inUse {
			return candidate
		}
	}
	Fail("no unused NodePort is available")
	return 0
}

func postNetworkJSON(path string, payload interface{}) *http.Response {
	GinkgoHelper()
	body, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	resp, err := doRequest(http.MethodPost, path, string(body))
	Expect(err).NotTo(HaveOccurred())
	return resp
}

func waitForNetworkReady(resourceID string) {
	GinkgoHelper()
	waitForNetworkStatus(resourceID, networkResourceStatusReady)
}

func waitForNetworkPending(resourceID, serviceName string, duration time.Duration) {
	GinkgoHelper()
	pendingState := func() string {
		status := networkResourceStatus(resourceID)
		if status != networkResourceStatusPending {
			return status
		}
		service, err := networkService(networkTestNamespace(), serviceName)
		if err != nil {
			return "service-unavailable"
		}
		if hasExternalAddress(service) {
			return "external-address"
		}
		return networkResourceStatusPending
	}
	Eventually(pendingState).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal(networkResourceStatusPending),
		"network Service should become pending before stability is checked")
	Consistently(pendingState).WithTimeout(duration).WithPolling(3*time.Second).Should(Equal(networkResourceStatusPending),
		"network Service should remain pending without an external address")
}

func waitForNetworkStatus(resourceID, expected string) {
	GinkgoHelper()
	Eventually(func() interface{} {
		status := networkResourceStatus(resourceID)
		if status == networkResourceStatusFailed && expected != networkResourceStatusFailed {
			return StopTrying(fmt.Sprintf("network resource %s failed", resourceID))
		}
		if status == networkResourceStatusReady && expected == networkResourceStatusFailed {
			return StopTrying(fmt.Sprintf("unsupported network routing unexpectedly reached ready: %s", resourceID))
		}
		return status
	}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Equal(expected))
}

func networkResourceStatus(resourceID string) string {
	GinkgoHelper()
	resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resourceID, "")
	if err != nil || resp == nil {
		return networkStatusUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return networkStatusUnavailable
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "invalid-response"
	}
	status, _ := body["status"].(string)
	return status
}

func waitForNetworkFailed(resourceID string) {
	GinkgoHelper()
	waitForNetworkStatus(resourceID, networkResourceStatusFailed)
}

func assertNetworkInstance(resource networkCRUDResource, agentName, expectedStatus string) {
	GinkgoHelper()
	resp, err := doRequest(http.MethodGet, "/service-type-instances/"+resource.ResourceID, "")
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	Expect(body["status"]).To(Equal(expectedStatus))
	Expect(body["agent_name"]).To(Equal(agentName))
}

func assertNetworkService(resource networkCRUDResource, wantType, wantClusterIP string, requireAssignedClusterIP bool, wantNodePortName string) {
	GinkgoHelper()
	requireKubectl()
	var out string
	Eventually(func() bool {
		var err error
		out, err = runKubectlInNamespace(resource.Namespace, "get", "service", resource.ServiceName, "-o", "json")
		return err == nil
	}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
		"Kubernetes Service %s was not readable; last kubectl output: %s", resource.ServiceName, out)
	var service map[string]interface{}
	Expect(json.Unmarshal([]byte(out), &service)).To(Succeed())
	metadata, ok := service["metadata"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	labels, ok := metadata["labels"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(labels["dcm.project/managed-by"]).To(Equal("dcm"))
	Expect(labels["dcm.project/dcm-service-type"]).To(Equal("network"))
	Expect(labels["dcm.project/dcm-instance-id"]).To(Equal(resource.ResourceID))
	spec, ok := service["spec"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(spec["type"]).To(Equal(wantType))
	Expect(spec["selector"]).To(Equal(map[string]interface{}{"app": "e2e-network"}))
	if wantClusterIP != "" {
		Expect(spec["clusterIP"]).To(Equal(wantClusterIP))
	}
	if requireAssignedClusterIP {
		clusterIP, ok := spec["clusterIP"].(string)
		Expect(ok).To(BeTrue())
		Expect(clusterIP).NotTo(BeEmpty())
		Expect(clusterIP).NotTo(Equal("None"))
	}
	ports, ok := spec["ports"].([]interface{})
	Expect(ok).To(BeTrue())
	Expect(ports).To(HaveLen(1))
	port, ok := ports[0].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(port["name"]).To(Equal("http"))
	Expect(port["protocol"]).To(Equal("TCP"))
	Expect(port["port"]).To(Equal(float64(80)))
	Expect(port["targetPort"]).To(Equal(float64(8080)))
	if wantNodePortName != "" {
		Expect(port["name"]).To(Equal(wantNodePortName))
		Expect(port["nodePort"]).To(Equal(float64(resource.NodePort)))
	}
}

func assertNetworkServiceAbsent(resource networkCRUDResource) {
	GinkgoHelper()
	requireKubectl()
	Eventually(func() string {
		out, _ := runKubectlInNamespace(resource.Namespace, "get", "service", resource.ServiceName,
			"-o", "name", "--ignore-not-found")
		return strings.TrimSpace(out)
	}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeEmpty(),
		"unsupported network request must not create Kubernetes Service %s", resource.ServiceName)
}

func assertNetworkServiceHasNoExternalIP(resource networkCRUDResource) {
	GinkgoHelper()
	service, err := networkService(resource.Namespace, resource.ServiceName)
	Expect(err).NotTo(HaveOccurred())
	Expect(hasExternalAddress(service)).To(BeFalse(),
		"LoadBalancer Service must not have an external address")
}

func assertNetworkServiceHasExternalIP(resource networkCRUDResource) {
	GinkgoHelper()
	service, err := networkService(resource.Namespace, resource.ServiceName)
	Expect(err).NotTo(HaveOccurred())
	Expect(hasExternalAddress(service)).To(BeTrue(),
		"LoadBalancer Service must have an external address")
}

func networkService(namespace, serviceName string) (map[string]interface{}, error) {
	out, err := runKubectlInNamespace(namespace, "get", "service", serviceName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("reading Service %s: %w; output: %s", serviceName, err, out)
	}
	var service map[string]interface{}
	if err := json.Unmarshal([]byte(out), &service); err != nil {
		return nil, fmt.Errorf("decoding Service %s: %w", serviceName, err)
	}
	return service, nil
}

func hasExternalAddress(service map[string]interface{}) bool {
	status, ok := service["status"].(map[string]interface{})
	if !ok {
		return false
	}
	loadBalancer, ok := status["loadBalancer"].(map[string]interface{})
	if !ok {
		return false
	}
	ingress, ok := loadBalancer["ingress"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range ingress {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ip, _ := entry["ip"].(string)
		hostname, _ := entry["hostname"].(string)
		if ip != "" || hostname != "" {
			return true
		}
	}
	return false
}

func requireLoadBalancerController() {
	GinkgoHelper()
	mode := loadBalancerMode()
	if mode == networkStatusUnavailable {
		Fail("load-balancer capability could not be determined; set DCM_NETWORK_LB_MODE explicitly")
	}
	if mode != "metallb" && mode != "cloud" {
		Skip("a load-balancer controller is not configured")
	}
}

func requireLoadBalancerMode(expected string) {
	GinkgoHelper()
	mode := loadBalancerMode()
	if mode == networkStatusUnavailable {
		Fail("load-balancer capability could not be determined; set DCM_NETWORK_LB_MODE explicitly")
	}
	if mode != expected {
		Skip(fmt.Sprintf("load-balancer mode %q is required", expected))
	}
}

func loadBalancerMode() string {
	if mode := os.Getenv("DCM_NETWORK_LB_MODE"); mode != "" {
		return mode
	}
	out, err := runKubectlInNamespace("metallb-system", "get", "deployment", "controller", "-o", "json")
	if err != nil {
		return networkStatusUnavailable
	}
	var deployment map[string]interface{}
	if json.Unmarshal([]byte(out), &deployment) != nil {
		return networkStatusUnavailable
	}
	status, ok := deployment["status"].(map[string]interface{})
	if !ok {
		return networkStatusUnavailable
	}
	available, _ := status["availableReplicas"].(float64)
	if available > 0 {
		return "metallb"
	}
	return "none"
}

func deleteNetworkResource(resource networkCRUDResource) {
	GinkgoHelper()
	cleanupNetworkResource(resource)
}

func cleanupNetworkResource(resource networkCRUDResource) {
	GinkgoHelper()
	Expect(teardownNetworkResource(resource)).To(Succeed())
}

func teardownNetworkResource(resource networkCRUDResource) error {
	var errs []error
	if resource.InstanceID != "" {
		errs = append(errs, deleteNetworkObject("/catalog-item-instances/"+resource.InstanceID,
			"catalog-item-instance"))
		errs = append(errs, waitForNotFound("/catalog-item-instances/"+resource.InstanceID,
			"catalog-item-instance "+resource.InstanceID))
		if resource.ResourceID != "" {
			errs = append(errs, waitForNotFound("/service-type-instances/"+resource.ResourceID,
				"service-type-instance "+resource.ResourceID))
		}
		if resource.ServiceName != "" {
			errs = append(errs, waitForServiceAbsent(resource))
		}
	}
	if resource.CatalogItemID != "" {
		errs = append(errs, deleteNetworkObject("/catalog-items/"+resource.CatalogItemID, "catalog item"))
	}
	if resource.PolicyID != "" {
		errs = append(errs, deleteNetworkObject("/policies/"+resource.PolicyID, "policy"))
	}
	return errors.Join(errs...)
}

func waitForNotFound(path, description string) error {
	return waitForCleanupCondition(description+" should be removed", func() error {
		resp, err := doRequest(http.MethodGet, path, "")
		if err != nil {
			return err
		}
		if resp == nil {
			return fmt.Errorf("%s lookup returned no response", description)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("%s returned HTTP %d", description, resp.StatusCode)
		}
		return nil
	})
}

func waitForServiceAbsent(resource networkCRUDResource) error {
	return waitForCleanupCondition("Kubernetes Service "+resource.ServiceName+" should be removed", func() error {
		out, err := runKubectlInNamespace(resource.Namespace, "get", "service", resource.ServiceName,
			"-o", "name", "--ignore-not-found")
		if err != nil {
			return fmt.Errorf("kubectl service lookup failed: %w; output: %s", err, out)
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("service still exists: %s", strings.TrimSpace(out))
		}
		return nil
	})
}

func waitForCleanupCondition(description string, check func() error) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := check(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s: %w", description, lastErr)
}

func deleteNetworkObject(path, description string) error {
	resp, err := doRequest(http.MethodDelete, path, "")
	if err != nil {
		return fmt.Errorf("deleting %s: %w", description, err)
	}
	if resp == nil {
		return fmt.Errorf("deleting %s returned no response", description)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound ||
		(resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices) {
		return nil
	}
	return fmt.Errorf("deleting %s returned HTTP %d", description, resp.StatusCode)
}

func networkTestNamespace() string {
	if namespace := os.Getenv("SP_K8S_NAMESPACE"); namespace != "" {
		return namespace
	}
	return "default"
}

func assertControlPlaneHealth() {
	resp, err := doRequest(http.MethodGet, "/health", "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	Expect(body).To(HaveKeyWithValue("status", "ok"))
}

func assertEmbeddedAgentHealth() {
	resp, err := networkAgentRequest(http.MethodGet, "/health")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	Expect(body).To(HaveKeyWithValue("status", "healthy"))
	Expect(body).To(HaveKeyWithValue("path", "health"))
}

// initNetworkSP verifies the embedded Network SP when explicitly enabled via
// DCM_NETWORK_SP_ENABLED=true, or when the environment-agent embeds network.
func initNetworkSP() {
	networkSPOnce.Do(func() {
		initEnvironmentAgent()

		networkAgentBaseURL = strings.TrimRight(os.Getenv("DCM_AGENT_URL"), "/")
		if networkAgentBaseURL == "" {
			networkAgentBaseURL = defaultAgentURL
		}

		explicit := os.Getenv(networkSPEnabledEnv) == "true"
		networkSPEnabled = explicit || agentEmbeds("network")
		if !networkSPEnabled {
			GinkgoWriter.Printf("Network SP disabled (%s=true or agent embedding network) — Network SP tests will be skipped\n", networkSPEnabledEnv)
			return
		}

		waitForNetworkProvider(30 * time.Second)
		networkSPReady = true
		GinkgoWriter.Printf("Network SP ready through agent at %s (explicit=%v embedded=%v)\n",
			networkAgentBaseURL, explicit, agentEmbeds("network"))
	})
}

func waitForNetworkProvider(timeout time.Duration) {
	Eventually(func() bool {
		return networkAgentHealthy() && embeddedNetworkProviderReady()
	}).WithTimeout(timeout).WithPolling(2*time.Second).Should(BeTrue(),
		"Network SP is enabled but did not become ready through the environment agent")
}

func requireNetworkSP() {
	if !networkSPEnabled {
		Skip(fmt.Sprintf("Network SP disabled (set %s=true, or deploy --with-environment-agent embedding network)", networkSPEnabledEnv))
	}
	if !networkSPReady {
		Fail("Network SP is enabled but unavailable")
	}
}

func networkAgentHealthy() bool {
	resp, err := unauthenticatedClient.Get(networkAgentBaseURL + "/health")
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitForEmbeddedNetworkProvider() {
	Eventually(embeddedNetworkProviderReady).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
		"embedded network provider should be registered and Ready")
}

func embeddedNetworkProviderReady() bool {
	resp, err := networkAgentRequest(http.MethodGet, "/providers")
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	var providers networkAgentProviderList
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		return false
	}
	for _, provider := range providers.Results {
		if provider.ServiceType == networkProviderServiceType && provider.Type == networkProviderType && provider.Status == networkProviderStatusReady {
			return true
		}
	}
	return false
}

func networkAgentRequest(method, path string) (*http.Response, error) {
	baseURL := os.Getenv("DCM_AGENT_URL")
	if baseURL == "" {
		baseURL = defaultAgentURL
	}
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func fetchNetworkServiceType() map[string]interface{} {
	resp, err := doRequest(http.MethodGet, "/service-types", "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	var body map[string]interface{}
	decodeJSON(resp, &body)
	results, ok := body["results"].([]interface{})
	Expect(ok).To(BeTrue(), "service-types response must contain results")
	for _, raw := range results {
		serviceType, ok := raw.(map[string]interface{})
		Expect(ok).To(BeTrue(), "service type entry should be an object")
		if serviceType["service_type"] == "network" {
			return serviceType
		}
	}

	Fail("network service type was not advertised by the control-plane")
	return nil
}
