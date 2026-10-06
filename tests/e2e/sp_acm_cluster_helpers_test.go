//go:build e2e

package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
)

const defaultAcmClusterSPURL = "http://localhost:8083/api/v1alpha1"

var (
	acmClusterSPBaseURL string
	acmClusterSPReady   bool
	acmClusterSPOnce    sync.Once
)

func initAcmClusterSP() {
	acmClusterSPOnce.Do(func() {
		acmClusterSPBaseURL = os.Getenv("DCM_ACM_CLUSTER_SP_URL")
		if acmClusterSPBaseURL == "" {
			acmClusterSPBaseURL = defaultAcmClusterSPURL
		}
		acmClusterSPBaseURL = strings.TrimRight(acmClusterSPBaseURL, "/")

		initEnvironmentAgent()

		resp, err := unauthenticatedClient.Get(acmClusterSPBaseURL + "/clusters/health")
		if err != nil {
			GinkgoWriter.Printf("Standalone ACM Cluster SP not reachable at %s: %v\n", acmClusterSPBaseURL, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			GinkgoWriter.Printf("Standalone ACM Cluster SP health returned %d at %s\n", resp.StatusCode, acmClusterSPBaseURL)
			return
		}
		acmClusterSPReady = true
		GinkgoWriter.Printf("Standalone ACM Cluster SP ready at %s\n", acmClusterSPBaseURL)
	})
}

func acmClusterCapabilityAvailable() bool {
	initAcmClusterSP()
	if acmClusterSPReady {
		return true
	}
	return waitForAgentEmbed("cluster", 30*time.Second)
}

// requireAcmClusterSP skips unless cluster workloads can be provisioned via
// the control plane (standalone SP or agent-embedded cluster).
func requireAcmClusterSP() {
	if !acmClusterCapabilityAvailable() {
		Skip("ACM cluster capability not available (standalone --acm-cluster-service-provider on :8083, or --with-environment-agent embedding cluster)")
	}
}

func doAcmClusterSPRequest(method, path string, body string) (*http.Response, error) {
	initAcmClusterSP()
	if acmClusterSPReady {
		url := acmClusterSPBaseURL + path

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
	if agentEmbeds("cluster") {
		return doEmbeddedAcmClusterSPRequest(method, path, body)
	}
	return nil, fmt.Errorf("ACM cluster SP not available")
}

func deleteTestCluster(id string) {
	resp, err := doAcmClusterSPRequest(http.MethodDelete, "/clusters/"+id, "")
	if err != nil {
		GinkgoWriter.Printf("Warning: cleanup DELETE failed for cluster %s: %v\n", id, err)
		return
	}
	resp.Body.Close()
}
