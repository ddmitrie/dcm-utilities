//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	kubevirtSPURL           string
	kubevirtSPOnce          sync.Once
	kubevirtStandaloneReady bool
)

// initKubevirtSP probes the standalone KubeVirt SP HTTP API.
// Default host port 8081 collides with environment-agent; only treat a URL as
// the standalone SP when GET /vms/health succeeds (agent exposes /health only).
func initKubevirtSP() {
	kubevirtSPOnce.Do(func() {
		initEnvironmentAgent()

		explicit := os.Getenv("DCM_KUBEVIRT_SP_URL")
		kubevirtSPURL = explicit
		if kubevirtSPURL == "" {
			kubevirtSPURL = "http://localhost:8081/api/v1alpha1"
		}
		kubevirtSPURL = strings.TrimRight(kubevirtSPURL, "/")

		// When the default URL is used and the agent owns :8081, skip the
		// misleading standalone probe unless the caller set DCM_KUBEVIRT_SP_URL.
		if explicit == "" && environmentAgentHealthy() && sameHTTPAuthority(kubevirtSPURL, agentBaseURL) {
			GinkgoWriter.Printf("Skipping default KubeVirt SP URL %s — environment agent is using the same host port\n", kubevirtSPURL)
			return
		}

		resp, err := unauthenticatedClient.Get(kubevirtSPURL + "/vms/health")
		if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
			GinkgoWriter.Printf("Standalone KubeVirt SP not reachable at %s/vms/health\n", kubevirtSPURL)
			if resp != nil {
				resp.Body.Close()
			}
			return
		}
		resp.Body.Close()
		kubevirtStandaloneReady = true
		GinkgoWriter.Printf("Standalone KubeVirt SP ready at %s\n", kubevirtSPURL)
	})
}

func sameHTTPAuthority(a, b string) bool {
	a = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(a), "https://"), "http://")
	b = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(b), "https://"), "http://")
	aHost, _, _ := strings.Cut(a, "/")
	bHost, _, _ := strings.Cut(b, "/")
	return aHost != "" && aHost == bHost
}

func kubevirtCapabilityAvailable() bool {
	initKubevirtSP()
	return kubevirtStandaloneReady || agentEmbeds("vm")
}

// requireKubevirtSP skips unless VM workloads can be provisioned via the
// control plane (standalone SP or agent-embedded vm).
func requireKubevirtSP() {
	if !kubevirtCapabilityAvailable() {
		Skip("KubeVirt capability not available (standalone --kubevirt-service-provider, or --with-environment-agent embedding vm)")
	}
}

// requireStandaloneKubevirtSP skips unless the direct KubeVirt SP HTTP API is up.
func requireStandaloneKubevirtSP() {
	initKubevirtSP()
	if !kubevirtStandaloneReady {
		Skip("Standalone KubeVirt SP not available (deploy with --kubevirt-service-provider; publish a port that does not clash with the agent)")
	}
}

// requireNATS skips the test if NATS is not reachable at DCM_NATS_URL.
func requireNATS() {
	url := os.Getenv("DCM_NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}

	nc, err := nats.Connect(url, nats.Timeout(2*time.Second), nats.Name("dcm-e2e-kubevirt-nats-check"))
	if err != nil {
		Skip(fmt.Sprintf("NATS server not available at %s: %v", url, err))
	}
	nc.Close()
}

// doKubevirtRequest performs HTTP request against the KubeVirt SP
func doKubevirtRequest(method, path, payload string) (*http.Response, error) {
	return doRequestToURL(kubevirtSPURL+path, method, payload)
}

// createVMPath returns POST /vms?id=<uuid>. The current kubevirt SP panics when
// the optional id query param is omitted (*request.Params.Id nil deref).
func createVMPath() (path string, id string) {
	id = uuid.NewString()
	return "/vms?id=" + id, id
}

// doRequestToURL performs HTTP request to a specific URL
func doRequestToURL(url, method, payload string) (*http.Response, error) {
	var bodyReader io.Reader
	if payload != "" {
		bodyReader = bytes.NewBufferString(payload)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	return unauthenticatedClient.Do(req)
}

// VMSpec represents a minimal VM specification for testing
type VMSpec struct {
	ServiceType string     `json:"service_type"`
	Metadata    VMMetadata `json:"metadata"`
	GuestOS     VMGuestOS  `json:"guest_os"`
	Vcpu        VMVcpu     `json:"vcpu"`
	Memory      VMMemory   `json:"memory"`
	Storage     VMStorage  `json:"storage"`
}

type VMMetadata struct {
	Name string `json:"name"`
}

type VMGuestOS struct {
	Type string `json:"type"`
}

type VMVcpu struct {
	Count int `json:"count"`
}

type VMMemory struct {
	Size string `json:"size"`
}

type VMStorage struct {
	Disks []VMDisk `json:"disks"`
}

type VMDisk struct {
	Name     string `json:"name"`
	Capacity string `json:"capacity"`
}

// newTestVMSpec creates a minimal VM spec for testing
func newTestVMSpec(name string) VMSpec {
	return VMSpec{
		ServiceType: "vm",
		Metadata: VMMetadata{
			Name: name,
		},
		GuestOS: VMGuestOS{
			Type: "linux",
		},
		Vcpu: VMVcpu{
			Count: 1,
		},
		Memory: VMMemory{
			// OpenAPI requires ^[0-9]+(MB|GB|TB)$ — not Kubernetes units like Gi
			Size: "1GB",
		},
		Storage: VMStorage{
			Disks: []VMDisk{
				{
					Name:     "boot",
					Capacity: "10GB",
				},
			},
		},
	}
}

// getKubevirtProviderName discovers the KubeVirt agent name from DCM
func getKubevirtProviderName() (string, error) {
	resp, err := doRequest(http.MethodGet, "/agents", "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get agents: status %d", resp.StatusCode)
	}

	var body map[string]interface{}
	decodeJSON(resp, &body)

	agents, ok := body["agents"].([]interface{})
	if !ok || len(agents) == 0 {
		return "", fmt.Errorf("no agents registered")
	}

	for _, a := range agents {
		agent, _ := a.(map[string]interface{})
		serviceTypes, _ := agent["service_types"].([]interface{})
		for _, st := range serviceTypes {
			if s, _ := st.(string); s == "vm" {
				name, _ := agent["name"].(string)
				if name != "" {
					return name, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no agent with service_types containing 'vm' found")
}

// extractIDFromPath extracts the instance ID from a path like "/api/v1alpha1/vms/abc-123"
func extractIDFromPath(path string) string {
	// Find the last segment after the final slash
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// getVMFromCluster fetches VirtualMachine resource via kubectl/oc
func getVMFromCluster(vmName, namespace string) (map[string]interface{}, error) {
	// Try oc first, fall back to kubectl
	cmd := exec.Command("oc", "get", "vm", vmName, "-n", namespace, "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		// Try kubectl as fallback
		cmd = exec.Command("kubectl", "get", "vm", vmName, "-n", namespace, "-o", "json")
		out, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get vm %s: %w", vmName, err)
		}
	}

	var vm map[string]interface{}
	if err := json.Unmarshal(out, &vm); err != nil {
		return nil, fmt.Errorf("failed to parse vm json: %w", err)
	}
	return vm, nil
}

// DCM label keys used by kubevirt-service-provider.
const (
	dcmLabelManagedBy  = "dcm.project/managed-by"
	dcmLabelInstanceID = "dcm.project/dcm-instance-id"
	dcmManagedByValue  = "dcm"
)

// kubevirtNamespace returns the namespace where the SP creates VMs.
// Prefer KUBERNETES_NAMESPACE (compose/SP export), then deploy flag env
// KUBEVIRT_VM_NAMESPACE, then legacy KUBEVIRT_NAMESPACE.
func kubevirtNamespace() string {
	if ns := os.Getenv("KUBERNETES_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("KUBEVIRT_VM_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("KUBEVIRT_NAMESPACE"); ns != "" {
		return ns
	}
	return "vms"
}

// expectProblemDetails asserts an OpenAPI Error (RFC 7807) response:
// Content-Type application/problem+json with required type + title.
func expectProblemDetails(resp *http.Response) map[string]interface{} {
	GinkgoHelper()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	Expect(ct).To(ContainSubstring("application/problem+json"),
		"OpenAPI Error responses must use application/problem+json, got %q", resp.Header.Get("Content-Type"))
	var problem map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&problem)).To(Succeed())
	Expect(problem).To(HaveKey("type"))
	Expect(problem).To(HaveKey("title"))
	return problem
}

// expectProblemDetailContains asserts problem details and that type/title/detail
// together contain at least one of the given substrings (case-insensitive).
func expectProblemDetailContains(resp *http.Response, substrings ...string) map[string]interface{} {
	GinkgoHelper()
	problem := expectProblemDetails(resp)
	blob := strings.ToLower(fmt.Sprintf("%v %v %v", problem["type"], problem["title"], problem["detail"]))
	matched := false
	for _, s := range substrings {
		if strings.Contains(blob, strings.ToLower(s)) {
			matched = true
			break
		}
	}
	Expect(matched).To(BeTrue(),
		"problem details %#v should mention one of %v", problem, substrings)
	return problem
}

// expectOpenAPIValidationErrorContains asserts an OpenAPI-layer validation failure
// mentions at least one of the substrings.
//
// TODO(FLPATH-4751): OpenAPI middleware currently returns text/plain via http.Error
// instead of application/problem+json required by the OpenAPI Error schema. Accept
// either until https://redhat.atlassian.net/browse/FLPATH-4751 is fixed, then require
// problem+json only (expectProblemDetailContains).
func expectOpenAPIValidationErrorContains(resp *http.Response, substrings ...string) {
	GinkgoHelper()
	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(body).NotTo(BeEmpty())

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	blob := strings.ToLower(string(body))
	if strings.Contains(ct, "json") || (len(body) > 0 && body[0] == '{') {
		var problem map[string]interface{}
		if err := json.Unmarshal(body, &problem); err == nil {
			Expect(problem).To(HaveKey("type"))
			Expect(problem).To(HaveKey("title"))
			blob = strings.ToLower(fmt.Sprintf("%v %v %v", problem["type"], problem["title"], problem["detail"]))
		}
	}

	matched := false
	for _, s := range substrings {
		if strings.Contains(blob, strings.ToLower(s)) {
			matched = true
			break
		}
	}
	Expect(matched).To(BeTrue(),
		"validation error body %q (content-type %q) should mention one of %v — workaround for FLPATH-4751",
		string(body), resp.Header.Get("Content-Type"), substrings)
}

// normalizeProviderOperations converts SPRM operations (string slice or comma string) to uppercased []string.
func normalizeProviderOperations(ops interface{}) []string {
	var raw []string
	switch v := ops.(type) {
	case []interface{}:
		for _, item := range v {
			raw = append(raw, fmt.Sprint(item))
		}
	case []string:
		raw = append(raw, v...)
	case string:
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				raw = append(raw, part)
			}
		}
	default:
		raw = append(raw, fmt.Sprint(ops))
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		out = append(out, strings.ToUpper(strings.TrimSpace(s)))
	}
	return out
}

// runKubeCmd tries oc then kubectl with the given args.
func runKubeCmd(args ...string) (string, error) {
	cmd := exec.Command("oc", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), nil
	}
	cmd = exec.Command("kubectl", args...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, string(out))
	}
	return string(out), nil
}

// findVMNameByInstanceID looks up the cluster VM name via DCM instance-id label.
func findVMNameByInstanceID(instanceID, namespace string) (string, error) {
	out, err := runKubeCmd("get", "vm", "-n", namespace,
		"-l", dcmLabelInstanceID+"="+instanceID,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no VM found with %s=%s in ns %s", dcmLabelInstanceID, instanceID, namespace)
	}
	return name, nil
}

// getVMByInstanceID fetches the VirtualMachine for a DCM instance id.
func getVMByInstanceID(instanceID, namespace string) (map[string]interface{}, string, error) {
	name, err := findVMNameByInstanceID(instanceID, namespace)
	if err != nil {
		return nil, "", err
	}
	vm, err := getVMFromCluster(name, namespace)
	return vm, name, err
}

func labelMap(obj map[string]interface{}, path ...string) (map[string]interface{}, error) {
	cur := obj
	for i, key := range path {
		next, ok := cur[key].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("missing path %v at %s", path[:i+1], key)
		}
		cur = next
	}
	return cur, nil
}

// verifyDCMLabels checks VirtualMachine (and template) have required DCM labels.
func verifyDCMLabels(vmName, namespace, expectedInstanceID string) error {
	vm, err := getVMFromCluster(vmName, namespace)
	if err != nil {
		return err
	}

	labels, err := labelMap(vm, "metadata", "labels")
	if err != nil {
		return err
	}

	managedBy, _ := labels[dcmLabelManagedBy].(string)
	if managedBy != dcmManagedByValue {
		return fmt.Errorf("missing or incorrect %s: got %q, want %q", dcmLabelManagedBy, managedBy, dcmManagedByValue)
	}

	instanceID, _ := labels[dcmLabelInstanceID].(string)
	if instanceID != expectedInstanceID {
		return fmt.Errorf("%s mismatch: got %q, want %q", dcmLabelInstanceID, instanceID, expectedInstanceID)
	}

	tmplLabels, err := labelMap(vm, "spec", "template", "metadata", "labels")
	if err != nil {
		return fmt.Errorf("template labels: %w", err)
	}
	if tmplManaged, _ := tmplLabels[dcmLabelManagedBy].(string); tmplManaged != dcmManagedByValue {
		return fmt.Errorf("template missing %s", dcmLabelManagedBy)
	}
	if tmplID, _ := tmplLabels[dcmLabelInstanceID].(string); tmplID != expectedInstanceID {
		return fmt.Errorf("template %s mismatch: got %q, want %q", dcmLabelInstanceID, tmplID, expectedInstanceID)
	}

	return nil
}

// createUnlabeledVM creates a minimal VirtualMachine without DCM labels (for TC-16).
func createUnlabeledVM(name, namespace string) error {
	yaml := fmt.Sprintf(`apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: %s
  namespace: %s
spec:
  runStrategy: Halted
  template:
    metadata:
      labels:
        kubevirt.io/domain: %s
    spec:
      domain:
        devices:
          disks:
          - name: containerdisk
            disk:
              bus: virtio
        resources:
          requests:
            memory: 64Mi
      volumes:
      - name: containerdisk
        containerDisk:
          image: quay.io/kubevirt/cirros-container-disk-demo:latest
`, name, namespace, name)
	cmd := exec.Command("oc", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(yaml)
		out, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("apply unlabeled vm: %w: %s", err, string(out))
		}
	}
	return nil
}

// patchVMRunning sets spec.running (legacy) or runStrategy for stop/start (TC-32).
func setVMRunStrategy(name, namespace, strategy string) error {
	patch := fmt.Sprintf(`{"spec":{"runStrategy":%q}}`, strategy)
	_, err := runKubeCmd("patch", "vm", name, "-n", namespace, "--type=merge", "-p", patch)
	return err
}

// getPVCsForVM lists PVCs in the namespace correlated to a VirtualMachine name
// (ownerReference name match or PVC name containing the VM name).
func getPVCsForVM(vmName, namespace string) ([]map[string]interface{}, error) {
	all, err := getPVCsInNamespace(namespace)
	if err != nil {
		return nil, err
	}
	var matched []map[string]interface{}
	for _, pvc := range all {
		md, _ := pvc["metadata"].(map[string]interface{})
		if md == nil {
			continue
		}
		pvcName, _ := md["name"].(string)
		if strings.Contains(pvcName, vmName) {
			matched = append(matched, pvc)
			continue
		}
		owners, _ := md["ownerReferences"].([]interface{})
		for _, o := range owners {
			om, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			ownerName, _ := om["name"].(string)
			if ownerName == vmName || strings.Contains(ownerName, vmName) {
				matched = append(matched, pvc)
				break
			}
		}
	}
	return matched, nil
}

// getPVCsInNamespace lists PVCs in the namespace (best-effort for storage class checks).
func getPVCsInNamespace(namespace string) ([]map[string]interface{}, error) {
	out, err := runKubeCmd("get", "pvc", "-n", namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list map[string]interface{}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	items, _ := list["items"].([]interface{})
	var result []map[string]interface{}
	for _, it := range items {
		if m, ok := it.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result, nil
}

// createTestVM posts a VM to the SP and returns instance id + request name.
func createTestVM(name string) (id string, err error) {
	spec := newTestVMSpec(name)
	payload, err := json.Marshal(map[string]interface{}{"spec": spec})
	if err != nil {
		return "", err
	}
	path, expectedID := createVMPath()
	resp, err := doKubevirtRequest(http.MethodPost, path, string(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create VM status %d: %s", resp.StatusCode, string(body))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	// OpenAPI CreateVM 201 → VM { path, spec }
	if _, ok := parsed["path"]; !ok {
		return "", fmt.Errorf("create VM 201 missing path: %s", string(body))
	}
	if _, ok := parsed["spec"]; !ok {
		return "", fmt.Errorf("create VM 201 missing spec: %s", string(body))
	}
	pathStr, _ := parsed["path"].(string)
	id = extractIDFromPath(pathStr)
	if id == "" {
		id = expectedID
	}
	return id, nil
}

// deleteTestVM best-effort deletes a VM via the SP API.
func deleteTestVM(id string) {
	if id == "" {
		return
	}
	resp, err := doKubevirtRequest(http.MethodDelete, "/vms/"+id, "")
	if err == nil && resp != nil {
		resp.Body.Close()
	}
}

// listVMIDs returns ids from GET /vms (from path or id fields).
func listVMIDs() ([]string, error) {
	resp, err := doKubevirtRequest(http.MethodGet, "/vms", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list status %d", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	vms, _ := body["vms"].([]interface{})
	var ids []string
	for _, v := range vms {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := m["id"].(string); id != "" {
			ids = append(ids, id)
			continue
		}
		if p, _ := m["path"].(string); p != "" {
			ids = append(ids, extractIDFromPath(p))
		}
	}
	return ids, nil
}

// deleteVMFromCluster removes VirtualMachine directly via kubectl/oc
func deleteVMFromCluster(vmName, namespace string) error {
	cmd := exec.Command("oc", "delete", "vm", vmName, "-n", namespace, "--ignore-not-found=true")
	if err := cmd.Run(); err != nil {
		// Try kubectl as fallback
		cmd = exec.Command("kubectl", "delete", "vm", vmName, "-n", namespace, "--ignore-not-found=true")
		return cmd.Run()
	}
	return nil
}

// verifyVMDeleted confirms VirtualMachine no longer exists.
// Succeeds only on NotFound; other kubectl/oc errors are returned so Eventually
// can keep polling or fail (unlike treating any get failure as "gone").
func verifyVMDeleted(vmName, namespace string) error {
	out, err := runKubeCmd("get", "vm", vmName, "-n", namespace)
	if err == nil {
		return fmt.Errorf("VM %s still exists in namespace %s", vmName, namespace)
	}
	blob := strings.ToLower(out + " " + err.Error())
	if strings.Contains(blob, "notfound") || strings.Contains(blob, "not found") {
		return nil
	}
	return fmt.Errorf("failed checking VM %s/%s deleted: %w (%s)", namespace, vmName, err, strings.TrimSpace(out))
}

// checkClusterAccess verifies kubectl/oc connectivity
func checkClusterAccess() error {
	cmd := exec.Command("oc", "cluster-info")
	if err := cmd.Run(); err != nil {
		// Try kubectl as fallback
		cmd = exec.Command("kubectl", "cluster-info")
		return cmd.Run()
	}
	return nil
}

// checkStorageClass verifies at least one StorageClass is available
func checkStorageClass() error {
	cmd := exec.Command("oc", "get", "storageclass", "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		// Try kubectl as fallback
		cmd = exec.Command("kubectl", "get", "storageclass", "-o", "json")
		out, err = cmd.Output()
		if err != nil {
			return fmt.Errorf("failed to get storage classes: %w", err)
		}
	}

	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("failed to parse storage class json: %w", err)
	}

	items, ok := result["items"].([]interface{})
	if !ok || len(items) == 0 {
		return fmt.Errorf("no storage classes available in cluster")
	}

	GinkgoWriter.Printf("Found %d storage class(es) available\n", len(items))
	return nil
}

// getDefaultStorageClass returns the name of the default storage class, or empty if none
func getDefaultStorageClass() string {
	cmd := exec.Command("oc", "get", "storageclass", "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command("kubectl", "get", "storageclass", "-o", "json")
		out, err = cmd.Output()
		if err != nil {
			return ""
		}
	}

	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return ""
	}

	items, ok := result["items"].([]interface{})
	if !ok {
		return ""
	}

	// Find default storage class
	for _, item := range items {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		metadata, ok := sc["metadata"].(map[string]interface{})
		if !ok {
			continue
		}

		annotations, ok := metadata["annotations"].(map[string]interface{})
		if !ok {
			continue
		}

		// Check for default annotation
		if isDefault, _ := annotations["storageclass.kubernetes.io/is-default-class"].(string); isDefault == "true" {
			name, _ := metadata["name"].(string)
			return name
		}
	}

	return ""
}
