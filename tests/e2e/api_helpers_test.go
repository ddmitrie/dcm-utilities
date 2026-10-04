//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dcm-project/utilities/tests/e2e/internal/resolve"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// runResourceIDCache maps run_id → first resource ID discovered during create.
// Ginkgo specs are sequential so no locking needed.
var runResourceIDCache = map[string]string{}

const defaultGatewayURL = "http://localhost:8080/api/v1alpha1"

// Control-plane ServiceTypeInstance.Status is stored lowercase after the
// StatusConsumer normalizes CloudEvent payloads (control-plane
// internal/sp/store/model/status.go). Standalone SP HTTP/NATS payloads may
// still use uppercase enums (RUNNING, PENDING).
const (
	instanceStatusPending      = "pending"
	instanceStatusQueued       = "queued"
	instanceStatusProvisioning = "provisioning"
	instanceStatusRunning      = "running"
	instanceStatusDeleting     = "deleting"
	instanceStatusFailed       = "failed"
)

var _ = BeforeSuite(func() {
	gatewayBaseURL = os.Getenv("DCM_GATEWAY_URL")
	if gatewayBaseURL == "" {
		gatewayBaseURL = defaultGatewayURL
	}
	gatewayBaseURL = strings.TrimRight(gatewayBaseURL, "/")

	Expect(configureHTTPClients()).To(Succeed())

	GinkgoWriter.Printf("Using gateway URL: %s\n", gatewayBaseURL)

	// Wait for the stack to be reachable before running any tests.
	Eventually(func() error {
		resp, err := httpClient.Get(gatewayBaseURL + "/health")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health check returned %d", resp.StatusCode)
		}
		return nil
	}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

	// Resolve CLI binary (tests skip gracefully if not found).
	initCLI()

	// Probe environment-agent first so embedded SP capability is known.
	initEnvironmentAgent()

	// Probe service providers (tests skip gracefully if not deployed).
	initContainerSP()
	initAcmClusterSP()
	initKubevirtSP()
	initNetworkSP()

	// Resolve cluster CLI for tests that need kubectl/oc.
	initKubectl()

	// Check podman for infrastructure disruption tests.
	initPodman()

	// Create catalog item and discover SPs for rehydration tests.
	initRehydrationCatalogItem()
	initThreeTierSP()
})

// doRequest builds a full URL from a relative path, sends the request, and
// returns the response. The caller is responsible for closing the body.
func doRequest(method, path string, body string) (*http.Response, error) {
	return doRequestWithClient(httpClient, method, path, body)
}

// readBody reads and closes the response body, returning the raw bytes.
func readBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// decodeJSON reads the response body and unmarshals it into the target.
func decodeJSON(resp *http.Response, target interface{}) {
	data := readBody(resp)
	Expect(json.Unmarshal(data, target)).To(Succeed())
}

// listServiceTypeInstanceIDs returns the set of current service-type-instance IDs.
func listServiceTypeInstanceIDs() map[string]struct{} {
	ids := map[string]struct{}{}
	token := ""
	for {
		path := "/service-type-instances?max_page_size=100"
		if token != "" {
			path += "&page_token=" + token
		}
		resp, err := doRequest(http.MethodGet, path, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var body map[string]interface{}
		decodeJSON(resp, &body)
		instances, _ := body["instances"].([]interface{})
		for _, raw := range instances {
			inst, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			id, _ := inst["id"].(string)
			if id != "" {
				ids[id] = struct{}{}
			}
		}
		next, _ := body["next_page_token"].(string)
		if next == "" {
			break
		}
		token = next
	}
	return ids
}

// waitForNewServiceTypeInstanceIDs polls until at least n STI IDs appear that
// were not present in before.
func waitForNewServiceTypeInstanceIDs(before map[string]struct{}, n int, timeout time.Duration) []string {
	var found []string
	Eventually(func() int {
		found = found[:0]
		for id := range listServiceTypeInstanceIDs() {
			if _, exists := before[id]; !exists {
				found = append(found, id)
			}
		}
		return len(found)
	}).WithTimeout(timeout).WithPolling(2*time.Second).Should(BeNumerically(">=", n),
		"expected at least %d new service-type-instance(s) after placement CreateRun", n)
	return found
}

// discoverAgentByServiceType queries GET /agents (paging through the full
// AgentList) and returns the name of the first agent whose service_types
// list contains serviceType. An optional overrideName restricts the match
// to that exact agent name.
// Fails the current test if no matching agent is found.
func discoverAgentByServiceType(serviceType, overrideName string) string {
	token := ""
	for {
		path := "/agents?max_page_size=100"
		if token != "" {
			path += "&page_token=" + token
		}
		resp, err := doRequest(http.MethodGet, path, "")
		Expect(err).NotTo(HaveOccurred())

		body, err := func() (map[string]interface{}, error) {
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			data, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return nil, readErr
			}
			var b map[string]interface{}
			return b, json.Unmarshal(data, &b)
		}()
		Expect(err).NotTo(HaveOccurred())

		agents, ok := body["agents"].([]interface{})
		Expect(ok).To(BeTrue(), "expected agents array in response")

		for _, raw := range agents {
			agent, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := agent["name"].(string)
			if overrideName != "" && name != overrideName {
				continue
			}
			serviceTypes, _ := agent["service_types"].([]interface{})
			for _, st := range serviceTypes {
				if s, _ := st.(string); s == serviceType {
					return name
				}
			}
		}

		next, _ := body["next_page_token"].(string)
		if next == "" {
			break
		}
		token = next
	}

	Fail(fmt.Sprintf("no agent with service_types containing %q found (override=%q)", serviceType, overrideName))
	return ""
}

// resolveResourceIDAfterCreate returns the primary placement resource ID for a
// catalog-item-instance create/rehydrate response.
//
// Breaking change: control-plane#39 removed spec.resource_ids and added run_id.
// When resource_ids are absent, newly appeared service-type-instance IDs
// (relative to before) are treated as the placement resource IDs.
//
// The disambiguation logic itself (legacy extraction, single-candidate
// selection) is pure and lives in internal/resolve, where it has real unit
// test coverage (see FLPATH-4809) — this function is just the e2e glue that
// gathers candidates over the network and fails the spec on error.
func resolveResourceIDAfterCreate(body map[string]interface{}, before map[string]struct{}) string {
	if ids := resolve.LegacyResourceIDs(body); len(ids) > 0 {
		if runID, _ := body["run_id"].(string); runID != "" {
			runResourceIDCache[runID] = ids[0]
		}
		return ids[0]
	}

	runID, _ := body["run_id"].(string)
	Expect(runID).NotTo(BeEmpty(),
		"catalog-item-instance response missing run_id and spec.resource_ids")

	ids := waitForNewServiceTypeInstanceIDs(before, 1, 60*time.Second)
	id, err := resolve.Unique(ids)
	Expect(err).NotTo(HaveOccurred(),
		"could not resolve placement resource ID after create (run_id=%s)", runID)
	runResourceIDCache[runID] = id
	return id
}
