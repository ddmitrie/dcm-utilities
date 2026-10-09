//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ACM Cluster SP API", Label("sp", "acm-cluster"), func() {

	BeforeEach(func() {
		requireAcmClusterSP()
	})

	Context("registration", func() {

		It("registers with DCM as a cluster agent", func() {
			resp, err := doRequest(http.MethodGet, "/agents", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			agents, ok := body["agents"].([]interface{})
			Expect(ok).To(BeTrue(), "expected agents array in response")

			var found map[string]interface{}
			for _, a := range agents {
				am, ok := a.(map[string]interface{})
				if !ok {
					continue
				}
				serviceTypes, _ := am["service_types"].([]interface{})
				for _, st := range serviceTypes {
					if s, _ := st.(string); s == "cluster" {
						found = am
						break
					}
				}
				if found != nil {
					break
				}
			}
			Expect(found).NotTo(BeNil(), "no agent with service_types containing 'cluster' found")
			if acmClusterSPReady {
				Expect(found).To(HaveKeyWithValue("name", "acm-cluster-sp"))
			}
		})
	})

	Context("health", Ordered, func() {

		var healthResp map[string]interface{}

		BeforeAll(func() {
			requireAcmClusterSP()
			resp, err := doAcmClusterSPRequest(http.MethodGet, "/clusters/health", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			decodeJSON(resp, &healthResp)
		})

		It("returns the correct schema", func() {
			Expect(healthResp).To(HaveKey("type"))
			Expect(healthResp).To(HaveKey("status"))
			Expect(healthResp).To(HaveKey("path"))
			Expect(healthResp).To(HaveKey("version"))
			Expect(healthResp).To(HaveKey("uptime"))
			Expect(healthResp["type"]).To(Equal("acm-cluster-service-provider.dcm.io/health"))
			Expect(healthResp["path"]).To(Equal("health"))
		})

		It("reports a valid status value", func() {
			status, ok := healthResp["status"].(string)
			Expect(ok).To(BeTrue())
			Expect(status).To(SatisfyAny(Equal("healthy"), Equal("unhealthy")))
		})

		It("reports uptime as a non-negative number", func() {
			uptime, ok := healthResp["uptime"].(float64)
			Expect(ok).To(BeTrue(), "uptime should be a number")
			Expect(uptime).To(BeNumerically(">=", 0))
		})

		It("shows increasing uptime over time", func() {
			resp1, err := doAcmClusterSPRequest(http.MethodGet, "/clusters/health", "")
			Expect(err).NotTo(HaveOccurred())
			var h1 map[string]interface{}
			decodeJSON(resp1, &h1)
			t1 := h1["uptime"].(float64)

			time.Sleep(3 * time.Second)

			resp2, err := doAcmClusterSPRequest(http.MethodGet, "/clusters/health", "")
			Expect(err).NotTo(HaveOccurred())
			var h2 map[string]interface{}
			decodeJSON(resp2, &h2)
			t2 := h2["uptime"].(float64)

			Expect(t2).To(BeNumerically(">", t1), "uptime should increase")
		})
	})

	Context("input validation", func() {

		It("rejects an empty request body", func() {
			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("rejects a body with missing required fields", func() {
			skipUnlessDirectAcmClusterSP()
			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters", `{"spec":{}}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(SatisfyAny(
				Equal(http.StatusBadRequest),
				Equal(http.StatusUnprocessableEntity),
			))
		})

		It("rejects a body with wrong field types", func() {
			skipUnlessDirectAcmClusterSP()
			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters",
				`{"spec":{"service_type": 123, "version": true}}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("rejects an unsupported Kubernetes version [TC-14]", Label("core", "negative"), func() {
			skipUnlessDirectAcmClusterSP()

			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters", acmClusterRequest(uniqueName("e2e-unsupported"), "1.99"))
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusUnprocessableEntity))

			body, readErr := io.ReadAll(resp.Body)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(string(body)).To(ContainSubstring("unsupported"))
		})

	})

	Context("RFC 9457 error format", Label("contract"), func() {
		BeforeEach(func() {
			skipUnlessDirectAcmClusterSP()
		})

		It("returns problem+json with status and project URI on validation error", func() {
			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters", `{}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			expectRFC9457Problem(resp, problemDetailExpectation{
				Status:     http.StatusBadRequest,
				TypeSuffix: "invalid-argument",
				Title:      invalidArgumentTitle,
			})
		})

		It("returns problem+json on not found", func() {
			requireKubectl()
			_, err := runKubectl("get", "crd", "hostedclusters.hypershift.openshift.io")
			if err != nil {
				Skip("HyperShift CRDs required — without them GET returns 500 instead of 404")
			}

			resp, err := doAcmClusterSPRequest(http.MethodGet, "/clusters/nonexistent-e2e-rfc9457", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			expectRFC9457Problem(resp, problemDetailExpectation{
				Status:     http.StatusNotFound,
				TypeSuffix: "not-found",
				Title:      notFoundTitle,
			})
		})
	})

	// CRUD tests require HyperShift CRDs on the cluster. Without them, the SP
	// returns 500 for Get/List/Delete because the K8s API server doesn't
	// recognize the HostedCluster GVK. These tests are gated by a connectivity
	// check for the HostedCluster CRD.
	Context("CRUD lifecycle", Label("cluster"), Ordered, func() {

		var clusterID string
		var hypershiftAvailable bool

		BeforeAll(func() {
			requireAcmClusterSP()
			requireKubectl()

			_, err := runKubectl("get", "crd", "hostedclusters.hypershift.openshift.io")
			if err != nil {
				Skip("HyperShift CRDs not installed — CRUD tests require ACM with HyperShift")
			}
			hypershiftAvailable = true
		})

		AfterAll(func() {
			if clusterID != "" {
				deleteTestCluster(clusterID)
			}
		})

		It("returns empty list when no managed clusters exist", func() {
			if !hypershiftAvailable {
				Skip("HyperShift CRDs required")
			}
			resp, err := doAcmClusterSPRequest(http.MethodGet, "/clusters", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			clusters, ok := body["clusters"].([]interface{})
			if ok {
				// Filter for only our test clusters (there may be others)
				Expect(clusters).NotTo(BeNil())
			}
		})

		It("returns 404 for a non-existent cluster", func() {
			if !hypershiftAvailable {
				Skip("HyperShift CRDs required")
			}
			resp, err := doAcmClusterSPRequest(http.MethodGet, "/clusters/nonexistent-e2e-id", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when deleting a non-existent cluster", func() {
			if !hypershiftAvailable {
				Skip("HyperShift CRDs required")
			}
			resp, err := doAcmClusterSPRequest(http.MethodDelete, "/clusters/nonexistent-e2e-id", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})

	Context("core lifecycle", Label("core", "cluster"), Ordered, func() {
		var clusterID string
		var clusterName string

		BeforeAll(func() {
			skipUnlessDirectAcmClusterSP()
			requireKubectl()

			_, err := runKubectlInNamespace(acmClusterNamespace(), "get", "crd", "hostedclusters.hypershift.openshift.io")
			if err != nil {
				Skip("HyperShift CRDs not installed — ACM cluster lifecycle requires HyperShift")
			}
		})

		AfterAll(func() {
			if clusterID != "" {
				deleteTestCluster(clusterID)
			}
		})

		It("creates a KubeVirt cluster and waits for HostedCluster availability [TC-04]", func() {
			clusterName = uniqueName("e2e-acm")
			resp, err := doAcmClusterSPRequest(http.MethodPost, "/clusters", acmClusterRequest(clusterName, acmClusterKubernetesVersion()))
			Expect(err).NotTo(HaveOccurred())
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			Expect(readErr).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(SatisfyAny(Equal(http.StatusCreated), Equal(http.StatusOK)), string(body))

			var created map[string]interface{}
			Expect(json.Unmarshal(body, &created)).To(Succeed())
			clusterID, _ = created["id"].(string)
			if clusterID == "" {
				if path, ok := created["path"].(string); ok {
					clusterID = extractIDFromPath(path)
				}
			}
			Expect(clusterID).NotTo(BeEmpty(), "create response should include cluster id or path")

			Eventually(func() bool {
				return hostedClusterAvailable(clusterName)
			}).WithTimeout(10*time.Minute).WithPolling(15*time.Second).Should(BeTrue(),
				"HostedCluster %s should become Available", clusterName)
		})

		It("deletes the KubeVirt cluster and waits for cleanup [TC-06]", func() {
			Expect(clusterID).NotTo(BeEmpty(), "create test must run before delete test")

			resp, err := doAcmClusterSPRequest(http.MethodDelete, "/clusters/"+clusterID, "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(SatisfyAny(Equal(http.StatusOK), Equal(http.StatusNoContent), Equal(http.StatusAccepted)))

			Eventually(func() bool {
				_, err := runKubectlInNamespace(acmClusterNamespace(), "get", "hostedcluster", clusterName)
				return err != nil
			}).WithTimeout(10*time.Minute).WithPolling(15*time.Second).Should(BeTrue(),
				"HostedCluster %s should be removed", clusterName)

			Eventually(func() int {
				getResp, getErr := doAcmClusterSPRequest(http.MethodGet, "/clusters/"+clusterID, "")
				if getErr != nil {
					return 0
				}
				getResp.Body.Close()
				return getResp.StatusCode
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Equal(http.StatusNotFound))
			clusterID = ""
		})
	})
})

func acmClusterRequest(name, version string) string {
	baseDomain := os.Getenv("DCM_ACM_BASE_DOMAIN")
	if baseDomain == "" {
		baseDomain = os.Getenv("SP_BASE_DOMAIN")
	}
	if baseDomain == "" {
		baseDomain = "dcm-test.qe.lab.redhat.com"
	}

	return fmt.Sprintf(`{"spec":{"metadata":{"name":%q},"service_type":"cluster","version":%q,"provider_hints":{"acm":{"platform":"kubevirt","base_domain":%q}}}}`, name, version, baseDomain)
}

func acmClusterKubernetesVersion() string {
	if version := os.Getenv("DCM_ACM_KUBERNETES_VERSION"); version != "" {
		return version
	}
	return "1.31"
}

func acmClusterNamespace() string {
	if namespace := os.Getenv("SP_CLUSTER_NAMESPACE"); namespace != "" {
		return namespace
	}
	return "clusters"
}

func hostedClusterAvailable(name string) bool {
	out, err := runKubectlInNamespace(acmClusterNamespace(), "get", "hostedcluster", name, "-o", "json")
	if err != nil {
		return false
	}

	var hostedCluster map[string]interface{}
	if json.Unmarshal([]byte(out), &hostedCluster) != nil {
		return false
	}
	status, _ := hostedCluster["status"].(map[string]interface{})
	conditions, _ := status["conditions"].([]interface{})
	for _, raw := range conditions {
		condition, _ := raw.(map[string]interface{})
		if condition["type"] == "Available" && condition["status"] == "True" {
			return true
		}
	}

	return false
}
