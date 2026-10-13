//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// GitOps coverage selected from FLPATH-4114 TC-09 and FLPATH-4910 TC-01
// through TC-03. Provisioning a manifest is intentionally kept separate: it
// requires a mutable public fixture and a catalog-item ID that exists in the
// target deployment.
var _ = Describe("GitOps Declarative API", Label("api", "declarative", "gitops"), Ordered, func() {
	var (
		repositoryID   string
		repositoryURL  string
		repositoryRef  string
		repositoryPath string
	)

	BeforeAll(func() {
		requireGitOpsReconciler()

		resp, err := doRequest(http.MethodGet, "/git-repositories", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		var body map[string]interface{}
		decodeJSON(resp, &body)
	})

	AfterAll(func() {
		if repositoryID == "" {
			return
		}
		resp, err := doRequest(http.MethodDelete, "/git-repositories/"+url.PathEscape(repositoryID), "")
		if err != nil {
			GinkgoWriter.Printf("Warning: GitRepository cleanup failed: %v\n", err)
			return
		}
		_ = resp.Body.Close()
		Eventually(func() int {
			check, err := doRequest(http.MethodGet, "/git-repositories/"+url.PathEscape(repositoryID), "")
			if err != nil {
				return 0
			}
			defer check.Body.Close()
			return check.StatusCode
		}).WithTimeout(cleanupTimeout).WithPolling(2*time.Second).Should(Equal(http.StatusNotFound),
			"GitRepository %s must be removed", repositoryID)
	})

	It("registers a public repository and preserves its configuration", func() {
		repositoryID = uniqueName("e2e-gitops-repository")
		repositoryURL = envOrDefault("GITOPS_REPOSITORY_URL", "https://github.com/dcm-project/dcm-gitops-test-fixtures.git")
		repositoryRef = envOrDefault("GITOPS_REPOSITORY_BRANCH", "main")
		repositoryPath = envOrDefault("GITOPS_REPOSITORY_PATH", ".")

		payload := gitRepositoryPayload(repositoryID, repositoryURL, repositoryRef, repositoryPath)
		resp, err := doRequest(http.MethodPost,
			"/git-repositories?id="+url.QueryEscape(repositoryID), payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		_ = resp.Body.Close()

		stored := getGitRepository(repositoryID)
		Expect(stringField(stored, "uid")).To(Equal(repositoryID))
		spec, ok := stored["spec"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "GitRepository response must contain spec")
		Expect(stringField(spec, "url")).To(Equal(repositoryURL))
		Expect(stringField(spec, "path")).To(Equal(repositoryPath))
		ref, ok := spec["ref"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "GitRepository spec must contain ref")
		Expect(stringField(ref, "branch")).To(Equal(repositoryRef))
	})

	It("synchronizes the registered repository and records a commit", func() {
		var synced map[string]interface{}
		Eventually(func() string {
			synced = getGitRepository(repositoryID)
			state := gitRepositoryState(synced)
			if strings.EqualFold(state, "synced") && gitRepositoryCommit(synced) != "" {
				return "synced"
			}
			return state
		}).WithTimeout(10*time.Minute).WithPolling(15*time.Second).Should(Equal("synced"),
			"GitOps must clone and synchronize the public repository")

		Expect(gitRepositoryCommit(synced)).NotTo(BeEmpty(),
			"synchronized repository must expose the commit it consumed")
	})
})

func requireGitOpsReconciler() {
	if strings.EqualFold(os.Getenv("GITOPS_TEST_ENABLED"), "true") {
		return
	}

	command := exec.Command("podman", "ps", "--filter", "name=dcm-e2e_dcm-gitops_1", "--format", "{{.Names}}")
	output, err := command.Output()
	if err == nil && strings.TrimSpace(string(output)) != "" {
		return
	}

	Skip("dcm-gitops is not running; deploy with the GitOps reconciler enabled")
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func gitRepositoryPayload(displayName, repositoryURL, repositoryRef, repositoryPath string) string {
	payload := map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": displayName,
		"spec": map[string]interface{}{
			"url":              repositoryURL,
			"ref":              map[string]string{"branch": repositoryRef},
			"path":             repositoryPath,
			"interval_seconds": 15,
		},
	}
	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func getGitRepository(id string) map[string]interface{} {
	resp, err := doRequest(http.MethodGet, "/git-repositories/"+url.PathEscape(id), "")
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	var body map[string]interface{}
	decodeJSON(resp, &body)
	return body
}

func gitRepositoryState(body map[string]interface{}) string {
	for _, path := range [][]string{
		{"status"},
		{"state"},
		{"status", "state"},
		{"status", "phase"},
	} {
		if value := nestedString(body, path...); value != "" {
			return strings.ToLower(value)
		}
	}
	return "pending"
}

func gitRepositoryCommit(body map[string]interface{}) string {
	for _, path := range [][]string{
		{"commit"},
		{"revision"},
		{"status", "commit"},
		{"status", "revision"},
		{"status", "synced_commit"},
	} {
		if value := nestedString(body, path...); value != "" {
			return value
		}
	}
	return ""
}

func nestedString(body map[string]interface{}, path ...string) string {
	var current interface{} = body
	for _, key := range path {
		object, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current = object[key]
	}
	value, _ := current.(string)
	return value
}
