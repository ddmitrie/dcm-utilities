//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	defaultContainerSPURL = "http://localhost:8082/api/v1alpha1"
	defaultNATSURL        = "nats://localhost:4222"
	natsStatusSubject     = "dcm.container" // flat subject, NOT hierarchical
)

var (
	containerSPBaseURL string
	containerSPReady   bool
	containerSPOnce    sync.Once
	natsURL            string
)

func initContainerSP() {
	containerSPOnce.Do(func() {
		containerSPBaseURL = os.Getenv("DCM_CONTAINER_SP_URL")
		if containerSPBaseURL == "" {
			containerSPBaseURL = defaultContainerSPURL
		}
		containerSPBaseURL = strings.TrimRight(containerSPBaseURL, "/")

		natsURL = os.Getenv("DCM_NATS_URL")
		if natsURL == "" {
			natsURL = defaultNATSURL
		}

		initEnvironmentAgent()

		resp, err := unauthenticatedClient.Get(containerSPBaseURL + "/containers/health")
		if err != nil {
			GinkgoWriter.Printf("Standalone container SP not reachable at %s: %v\n", containerSPBaseURL, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			GinkgoWriter.Printf("Standalone container SP health returned %d at %s\n", resp.StatusCode, containerSPBaseURL)
			return
		}
		containerSPReady = true
		GinkgoWriter.Printf("Standalone container SP ready at %s\n", containerSPBaseURL)
	})
}

// containerCapabilityAvailable is true when a standalone container SP is up
// or the environment-agent embeds a Ready container provider.
func containerCapabilityAvailable() bool {
	initContainerSP()
	return containerSPReady || agentEmbeds("container")
}

// requireContainerSP skips unless container workloads can be provisioned via
// the control plane (standalone SP or agent-embedded container).
func requireContainerSP() {
	if !containerCapabilityAvailable() {
		Skip("Container capability not available (standalone --k8s-container-service-provider on :8082, or --with-environment-agent embedding container)")
	}
}

// requireStandaloneContainerSP is kept for existing specs. It now requires
// container capability (standalone port or agent-embedded), not the SP port.
func requireStandaloneContainerSP() {
	requireContainerSP()
}

// doContainerSPRequest sends a request to the container SP HTTP API when a
// standalone SP is up, otherwise via the control plane / environment-agent.
func doContainerSPRequest(method, path string, body string) (*http.Response, error) {
	initContainerSP()
	if containerSPReady {
		url := containerSPBaseURL + path

		var reqBody io.Reader
		if body != "" {
			reqBody = strings.NewReader(body)
		}

		req, err := http.NewRequest(method, url, reqBody)
		if err != nil {
			return nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}

		return unauthenticatedClient.Do(req)
	}
	if agentEmbeds("container") {
		return doEmbeddedContainerSPRequest(method, path, body)
	}
	return nil, fmt.Errorf("container SP not available")
}

// expectRFC9457Problem asserts an RFC 9457 problem+json response (FLPATH-4720/4721)
// and returns the decoded ProblemDetail struct.
func expectRFC9457Problem(resp *http.Response, want problemDetailExpectation) ProblemDetail {
	GinkgoHelper()
	Expect(resp.StatusCode).To(Equal(want.Status))

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	Expect(err).NotTo(HaveOccurred(), "Content-Type must be a valid media type, got %q", resp.Header.Get("Content-Type"))
	Expect(mediaType).To(Equal("application/problem+json"),
		"expected application/problem+json, got %q", resp.Header.Get("Content-Type"))

	var problem ProblemDetail
	decodeJSON(resp, &problem)

	Expect(strings.TrimSpace(problem.Type)).NotTo(BeEmpty())
	Expect(strings.TrimSpace(problem.Title)).NotTo(BeEmpty())
	if want.TypeSuffix != "" {
		Expect(problem.Type).To(Equal(problemTypeBaseURI + want.TypeSuffix))
	}
	if want.Title != "" {
		Expect(problem.Title).To(Equal(want.Title))
	}
	Expect(problem.Status).To(Equal(want.Status))
	if want.Detail != "" {
		Expect(problem.Detail).To(Equal(want.Detail))
	} else {
		Expect(problem.Detail).NotTo(BeEmpty())
	}

	return problem
}

// decodeContainerProblemDetail decodes a container SP multi-error problem body.
func decodeContainerProblemDetail(resp *http.Response) ContainerProblemDetail {
	GinkgoHelper()
	var problem ContainerProblemDetail
	decodeJSON(resp, &problem)
	return problem
}

// readContainerProblemDetail asserts HTTP status and problem+json, then decodes the body.
func readContainerProblemDetail(resp *http.Response, wantStatus int) ContainerProblemDetail {
	GinkgoHelper()
	Expect(resp.StatusCode).To(Equal(wantStatus))

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	Expect(err).NotTo(HaveOccurred(), "Content-Type must be a valid media type, got %q", resp.Header.Get("Content-Type"))
	Expect(mediaType).To(Equal("application/problem+json"),
		"expected application/problem+json, got %q", resp.Header.Get("Content-Type"))

	return decodeContainerProblemDetail(resp)
}

// createTestContainer creates a container via the SP API and returns the parsed response body.
// It fails the test if creation doesn't succeed.
func createTestContainer(spec string) map[string]interface{} {
	resp, err := doContainerSPRequest(http.MethodPost, "/containers", spec)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(SatisfyAny(Equal(http.StatusOK), Equal(http.StatusCreated)),
		"create container failed with status %d", resp.StatusCode)

	var body map[string]interface{}
	decodeJSON(resp, &body)
	Expect(body).To(HaveKey("id"))
	return body
}

// deleteTestContainer removes a container by ID, ignoring 404 (already gone).
func deleteTestContainer(id string) {
	resp, err := doContainerSPRequest(http.MethodDelete, "/containers/"+id, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: cleanup DELETE failed for container %s: %v\n", id, err)
		return
	}
	resp.Body.Close()
}

// containerSpecOpts configures optional fields for containerSpec.
type containerSpecOpts struct {
	ports   bool
	command []string
}

// containerSpec returns a JSON body for creating a test container per the OpenAPI schema.
func containerSpec(name, imageRef string, ports bool) string {
	return containerSpecWith(name, imageRef, containerSpecOpts{ports: ports})
}

// containerSpecWith returns a JSON body with full control over optional fields.
func containerSpecWith(name, imageRef string, opts containerSpecOpts) string {
	spec := map[string]interface{}{
		"service_type": "container",
		"metadata":     map[string]interface{}{"name": name},
		"image":        map[string]interface{}{"reference": imageRef},
		"resources": map[string]interface{}{
			// CPU quantities are Kubernetes resource.Quantity strings (e.g. "1", "500m").
			"cpu":    map[string]interface{}{"min": "1", "max": "1"},
			"memory": map[string]interface{}{"min": "128MB", "max": "256MB"},
		},
	}
	if opts.ports {
		spec["network"] = map[string]interface{}{
			"ports": []map[string]interface{}{
				{"container_port": 80, "visibility": "internal"},
			},
		}
	}
	if len(opts.command) > 0 {
		spec["process"] = map[string]interface{}{
			"command": opts.command,
		}
	}
	body := map[string]interface{}{"spec": spec}
	data, _ := json.Marshal(body)
	return string(data)
}

// uniqueName generates a short unique name for test containers.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1000000)
}

// --- NATS helpers --------------------------------------------------------- //

// CloudEvent represents the status event published by the container SP.
type CloudEvent struct {
	SpecVersion     string                 `json:"specversion"`
	ID              string                 `json:"id"`
	Source          string                 `json:"source"`
	Type            string                 `json:"type"`
	Time            string                 `json:"time"`
	DataContentType string                 `json:"datacontenttype"`
	Data            map[string]interface{} `json:"data"`
}

// NATSCollector subscribes to a NATS subject and collects messages.
type NATSCollector struct {
	conn   *nats.Conn
	sub    *nats.Subscription
	mu     sync.Mutex
	events []CloudEvent
}

func newNATSCollector(subject string) *NATSCollector {
	conn, err := nats.Connect(natsURL,
		nats.Timeout(5*time.Second),
		nats.Name("dcm-e2e-test"),
	)
	Expect(err).NotTo(HaveOccurred(), "failed to connect to NATS at %s", natsURL)

	c := &NATSCollector{conn: conn}

	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		var evt CloudEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			GinkgoWriter.Printf("Warning: failed to parse NATS message: %v\n", err)
			return
		}
		c.mu.Lock()
		c.events = append(c.events, evt)
		c.mu.Unlock()
	})
	Expect(err).NotTo(HaveOccurred(), "failed to subscribe to %s", subject)
	c.sub = sub

	return c
}

func (c *NATSCollector) Close() {
	if c.sub != nil {
		_ = c.sub.Unsubscribe()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}

// EventsForInstance returns events matching the given instance ID.
func (c *NATSCollector) EventsForInstance(instanceID string) []CloudEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CloudEvent
	for _, e := range c.events {
		if id, ok := e.Data["id"].(string); ok && id == instanceID {
			out = append(out, e)
		}
	}
	return out
}

// WaitForStatus polls until an event with the given instance ID and status appears.
func (c *NATSCollector) WaitForStatus(instanceID, status string, timeout time.Duration) CloudEvent {
	var matched CloudEvent
	Eventually(func() bool {
		for _, e := range c.EventsForInstance(instanceID) {
			if s, ok := e.Data["status"].(string); ok && s == status {
				matched = e
				return true
			}
		}
		return false
	}).WithTimeout(timeout).WithPolling(500*time.Millisecond).Should(BeTrue(),
		fmt.Sprintf("timed out waiting for status %q on instance %s", status, instanceID))
	return matched
}

// --- Cluster helpers (kubectl/oc) ----------------------------------------- //

var (
	kubectlBin       string
	kubectlAvailable bool
	spNamespace      string
)

func initKubectl() {
	spNamespace = os.Getenv("K8S_CONTAINER_SP_NAMESPACE")
	if spNamespace == "" {
		spNamespace = "default"
	}

	for _, bin := range []string{"oc", "kubectl"} {
		if path, err := exec.LookPath(bin); err == nil {
			kubectlBin = path
			break
		}
	}
	if kubectlBin == "" {
		GinkgoWriter.Println("kubectl/oc not found — cluster-level SP tests will be skipped")
		return
	}

	out, err := exec.Command(kubectlBin, "cluster-info").CombinedOutput()
	if err != nil {
		GinkgoWriter.Printf("Cluster not reachable via %s: %s — cluster-level SP tests will be skipped\n", kubectlBin, string(out))
		return
	}
	kubectlAvailable = true
	GinkgoWriter.Printf("Using %s for cluster operations (namespace: %s)\n", kubectlBin, spNamespace)
}

func requireKubectl() {
	if !kubectlAvailable {
		Skip("kubectl/oc not available or cluster not reachable")
	}
}

// runKubectl executes a kubectl/oc command and returns combined output.
func runKubectl(args ...string) (string, error) {
	fullArgs := append([]string{"-n", spNamespace}, args...)
	cmd := exec.Command(kubectlBin, fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		GinkgoWriter.Printf("kubectl %v failed: %s\n", args, string(out))
	}
	return string(out), err
}

// runKubectlAllNamespaces executes a cluster-wide kubectl/oc command.
func runKubectlAllNamespaces(args ...string) (string, error) {
	cmd := exec.Command(kubectlBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		GinkgoWriter.Printf("kubectl %v failed: %s\n", args, string(out))
	}
	return string(out), err
}

// findDeploymentName returns the Kubernetes Deployment name for a DCM
// container instance. The SP uses GenerateName so the actual Deployment
// name has a random suffix; this helper resolves it via label selector.
func findDeploymentName(containerID string) string {
	selector := "dcm.project/dcm-instance-id=" + containerID
	out, err := runKubectl("get", "deployment",
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred(),
		"failed to look up Deployment for container %s", containerID)
	name := strings.TrimSpace(out)
	ExpectWithOffset(1, name).NotTo(BeEmpty(),
		"no Deployment found with label %s", selector)
	return name
}

// --- Podman helpers ------------------------------------------------------- //

var (
	podmanBin       string
	podmanAvailable bool
)

func initPodman() {
	if path, err := exec.LookPath("podman"); err == nil {
		podmanBin = path
	} else {
		GinkgoWriter.Println("podman not found — infrastructure disruption tests will be skipped")
		return
	}

	if err := exec.Command(podmanBin, "info").Run(); err != nil {
		GinkgoWriter.Println("podman not functional — infrastructure disruption tests will be skipped")
		return
	}
	podmanAvailable = true
	GinkgoWriter.Printf("Using %s for infrastructure tests\n", podmanBin)
}

func requirePodman() {
	if !podmanAvailable {
		Skip("podman not available")
	}
}

// findComposeContainer returns the container ID for a compose service name.
func findComposeContainer(serviceName string) string {
	out, err := exec.Command(podmanBin, "ps", "--filter", "name="+serviceName, "--format", "{{.ID}}").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "failed to find container for service %s: %s", serviceName, string(out))
	id := strings.TrimSpace(string(out))
	Expect(id).NotTo(BeEmpty(), "no running container found matching %q", serviceName)
	return strings.Split(id, "\n")[0]
}
