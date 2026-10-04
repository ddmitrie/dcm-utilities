//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

const (
	defaultAgentURLEnv = "DCM_AGENT_URL"
	defaultAgentURLVal = "http://localhost:8081/api/v1alpha1"
	embeddedSPsEnv     = "DCM_EMBEDDED_SPS"
)

var (
	agentInitOnce sync.Once
	agentBaseURL  string
	agentHealthy  bool
	// embeddedReady maps service_type → Ready via agent GET /providers.
	embeddedReady = map[string]bool{}
)

type agentProviderEntry struct {
	ServiceType string `json:"service_type"`
	Status      string `json:"status"`
	Type        string `json:"type"`
}

type agentProviderList struct {
	Results []agentProviderEntry `json:"results"`
}

// initEnvironmentAgent probes the environment-agent API and records which
// embedded service types are Ready. Safe to call multiple times.
func initEnvironmentAgent() {
	agentInitOnce.Do(func() {
		agentBaseURL = strings.TrimRight(os.Getenv(defaultAgentURLEnv), "/")
		if agentBaseURL == "" {
			agentBaseURL = defaultAgentURLVal
		}

		resp, err := unauthenticatedClient.Get(agentBaseURL + "/health")
		if err != nil {
			GinkgoWriter.Printf("Environment agent not reachable at %s: %v\n", agentBaseURL, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			GinkgoWriter.Printf("Environment agent health returned %d at %s\n", resp.StatusCode, agentBaseURL)
			return
		}
		agentHealthy = true
		embeddedAgentSeenAt = time.Now()
		GinkgoWriter.Printf("Environment agent healthy at %s\n", agentBaseURL)

		provResp, err := unauthenticatedClient.Get(agentBaseURL + "/providers")
		if err != nil {
			GinkgoWriter.Printf("Environment agent /providers failed: %v\n", err)
			seedEmbeddedFromEnv()
			return
		}
		defer provResp.Body.Close()
		if provResp.StatusCode != http.StatusOK {
			GinkgoWriter.Printf("Environment agent /providers returned %d\n", provResp.StatusCode)
			seedEmbeddedFromEnv()
			return
		}

		var list agentProviderList
		if err := json.NewDecoder(provResp.Body).Decode(&list); err != nil {
			GinkgoWriter.Printf("Environment agent /providers decode failed: %v\n", err)
			seedEmbeddedFromEnv()
			return
		}
		for _, p := range list.Results {
			st := strings.ToLower(strings.TrimSpace(p.ServiceType))
			status := strings.EqualFold(p.Status, "Ready") || strings.EqualFold(p.Status, "ready")
			if st == "" || !status {
				continue
			}
			embeddedReady[st] = true
			GinkgoWriter.Printf("Embedded SP ready via agent: %s (type=%s)\n", st, p.Type)
		}
		seedEmbeddedFromEnv()
	})
}

// seedEmbeddedFromEnv marks tokens from DCM_EMBEDDED_SPS as available when the
// agent is healthy but /providers omitted them (or returned an unexpected shape).
func seedEmbeddedFromEnv() {
	if !agentHealthy {
		return
	}
	raw := strings.TrimSpace(os.Getenv(embeddedSPsEnv))
	if raw == "" {
		return
	}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if !embeddedReady[tok] {
			embeddedReady[tok] = true
			GinkgoWriter.Printf("Embedded SP from %s: %s\n", embeddedSPsEnv, tok)
		}
	}
}

func environmentAgentHealthy() bool {
	initEnvironmentAgent()
	return agentHealthy
}

func agentEmbeds(serviceType string) bool {
	initEnvironmentAgent()
	return embeddedReady[strings.ToLower(strings.TrimSpace(serviceType))]
}

func requireEnvironmentAgent() {
	if !environmentAgentHealthy() {
		Skip("Environment agent not available (deploy with --with-environment-agent; DCM_AGENT_URL)")
	}
}
