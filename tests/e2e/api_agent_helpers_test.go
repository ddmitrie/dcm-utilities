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
	// embeddedReady maps service_type → Ready (live /providers, or provisional
	// env hint only while /providers is unavailable).
	embeddedReady = map[string]bool{}
	// embeddedProvisional marks Ready values seeded from DCM_EMBEDDED_SPS when
	// /providers could not be read. Cleared on the next successful refresh so
	// a later live not-Ready/absent response cannot keep returning true.
	embeddedProvisional = map[string]bool{}
	// embeddedSeen maps service_type → present in a successful /providers list
	// (any status). Used so env hints never override a live not-Ready entry.
	embeddedSeen = map[string]bool{}
	// embeddedHinted maps service_type → listed in DCM_EMBEDDED_SPS (discovery
	// hint only; not evidence of readiness).
	embeddedHinted = map[string]bool{}
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
		loadEmbeddedHints()

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

		if !refreshEmbeddedProviders() {
			// /providers unreachable or unreadable — fall back to env hints as
			// provisional Ready so suites can still attempt (and poll/skip).
			seedEmbeddedReadyFromHints("providers unavailable")
			return
		}
		// Successful /providers decode: DCM_EMBEDDED_SPS stays a discovery hint
		// only. Do not mark hinted types Ready when the live list omitted them
		// as Ready (including present-but-not-Ready).
	})
}

// loadEmbeddedHints records DCM_EMBEDDED_SPS tokens as discovery hints.
func loadEmbeddedHints() {
	raw := strings.TrimSpace(os.Getenv(embeddedSPsEnv))
	if raw == "" {
		return
	}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if !embeddedHinted[tok] {
			embeddedHinted[tok] = true
			GinkgoWriter.Printf("Embedded SP hint from %s: %s\n", embeddedSPsEnv, tok)
		}
	}
}

// refreshEmbeddedProviders re-fetches agent GET /providers and merges Ready
// service types into embeddedReady. Live-confirmed Ready stays sticky across
// probes; provisional hint Ready is cleared when the live list does not
// confirm Ready. Types present in the list (any status) are recorded in
// embeddedSeen.
func refreshEmbeddedProviders() bool {
	if agentBaseURL == "" {
		agentBaseURL = strings.TrimRight(os.Getenv(defaultAgentURLEnv), "/")
		if agentBaseURL == "" {
			agentBaseURL = defaultAgentURLVal
		}
	}
	if !agentHealthy {
		return false
	}

	provResp, err := unauthenticatedClient.Get(agentBaseURL + "/providers")
	if err != nil {
		GinkgoWriter.Printf("Environment agent /providers failed: %v\n", err)
		return false
	}
	defer provResp.Body.Close()
	if provResp.StatusCode != http.StatusOK {
		GinkgoWriter.Printf("Environment agent /providers returned %d\n", provResp.StatusCode)
		return false
	}

	var list agentProviderList
	if err := json.NewDecoder(provResp.Body).Decode(&list); err != nil {
		GinkgoWriter.Printf("Environment agent /providers decode failed: %v\n", err)
		return false
	}
	for _, p := range list.Results {
		st := strings.ToLower(strings.TrimSpace(p.ServiceType))
		if st == "" {
			continue
		}
		embeddedSeen[st] = true
		if !strings.EqualFold(p.Status, "Ready") {
			continue
		}
		if !embeddedReady[st] || embeddedProvisional[st] {
			GinkgoWriter.Printf("Embedded SP ready via agent: %s (type=%s)\n", st, p.Type)
		}
		embeddedReady[st] = true
		delete(embeddedProvisional, st)
	}
	// Drop provisional Ready that the live list did not confirm (absent or
	// non-Ready). Entries confirmed Ready above already left embeddedProvisional.
	for st := range embeddedProvisional {
		if embeddedReady[st] {
			GinkgoWriter.Printf("Clearing provisional Ready for %s (absent or not Ready in /providers)\n", st)
		}
		embeddedReady[st] = false
		delete(embeddedProvisional, st)
	}
	return true
}

// seedEmbeddedReadyFromHints marks hinted types Ready only when /providers
// could not be used. Marks them provisional so a later successful /providers
// response can clear Ready when the type is absent or not Ready.
func seedEmbeddedReadyFromHints(reason string) {
	for tok := range embeddedHinted {
		if embeddedReady[tok] && !embeddedProvisional[tok] {
			continue
		}
		embeddedReady[tok] = true
		embeddedProvisional[tok] = true
		GinkgoWriter.Printf("Embedded SP provisional Ready from %s (%s): %s\n", embeddedSPsEnv, reason, tok)
	}
}

// waitForAgentEmbed polls the live agent /providers list until serviceType is
// Ready (or timeout). Env hints alone never skip the live poll when the agent
// is healthy and /providers is readable.
func waitForAgentEmbed(serviceType string, timeout time.Duration) bool {
	st := strings.ToLower(strings.TrimSpace(serviceType))
	initEnvironmentAgent()
	if !agentHealthy {
		return false
	}

	// Always re-probe when possible so a prior hint/env seed cannot win over
	// a live not-Ready response.
	if refreshEmbeddedProviders() {
		if embeddedReady[st] {
			return true
		}
		// Not Ready (or absent). Only keep polling when discovery expects it.
		if !embeddedHinted[st] && !embeddedSeen[st] {
			return false
		}
	} else if embeddedReady[st] {
		// Providers unavailable; provisional env seed from init.
		return true
	} else if !embeddedHinted[st] {
		return false
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return embeddedReady[st]
		}
		time.Sleep(2 * time.Second)
		if !refreshEmbeddedProviders() {
			continue
		}
		if embeddedReady[st] {
			return true
		}
	}
}

func environmentAgentHealthy() bool {
	initEnvironmentAgent()
	return agentHealthy
}

// agentEmbeds reports live Ready, or provisional Ready only while /providers
// remains unavailable after init. Prefer waitForAgentEmbed before enabling
// suites so a recovered /providers list can clear stale provisional Ready.
func agentEmbeds(serviceType string) bool {
	initEnvironmentAgent()
	return embeddedReady[strings.ToLower(strings.TrimSpace(serviceType))]
}

func requireEnvironmentAgent() {
	if !environmentAgentHealthy() {
		Skip("Environment agent not available (deploy with --with-environment-agent; DCM_AGENT_URL)")
	}
}
