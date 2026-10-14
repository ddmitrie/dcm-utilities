//go:build e2e

package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
		repositoryID       string
		repositoryURL      string
		repositoryRef      string
		repositoryPath     string
		updateRepoID       string
		updateBranch       string
		catalogItemID      = "dcm-gitops-two-tier-web"
		catalogItemCreated bool
		policyID           string
	)

	BeforeAll(func() {
		requireGitOpsReconciler()
		catalogItemCreated = ensureGitOpsCatalogItem(catalogItemID)
		policyID = createGitOpsRoutingPolicy()

		resp, err := doRequest(http.MethodGet, "/git-repositories", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		var body map[string]interface{}
		decodeJSON(resp, &body)
	})

	AfterAll(func() {
		if repositoryID == "" && updateRepoID == "" && policyID == "" && !catalogItemCreated {
			return
		}
		for _, id := range []string{repositoryID, updateRepoID} {
			if id == "" {
				continue
			}
			resp, err := doRequest(http.MethodDelete, "/git-repositories/"+url.PathEscape(id), "")
			if err != nil {
				GinkgoWriter.Printf("Warning: GitRepository cleanup failed: %v\n", err)
				continue
			}
			_ = resp.Body.Close()
			Eventually(func() int {
				check, err := doRequest(http.MethodGet, "/git-repositories/"+url.PathEscape(id), "")
				if err != nil {
					return 0
				}
				defer check.Body.Close()
				return check.StatusCode
			}).WithTimeout(cleanupTimeout).WithPolling(2*time.Second).Should(Equal(http.StatusNotFound),
				"GitRepository %s must be removed", id)
		}
		if updateBranch != "" {
			deleteGitHubBranch(repositoryURL, updateBranch)
		}
		if policyID != "" {
			resp, err := doRequest(http.MethodDelete, "/policies/"+url.PathEscape(policyID), "")
			if err != nil {
				GinkgoWriter.Printf("Warning: policy cleanup failed: %v\n", err)
			} else {
				_ = resp.Body.Close()
			}
		}
		if catalogItemCreated {
			resp, err := doRequest(http.MethodDelete, "/catalog-items/"+url.PathEscape(catalogItemID), "")
			if err != nil {
				GinkgoWriter.Printf("Warning: catalog item cleanup failed: %v\n", err)
				return
			}
			_ = resp.Body.Close()
			Eventually(func() int {
				check, err := doRequest(http.MethodGet, "/catalog-items/"+url.PathEscape(catalogItemID), "")
				if err != nil {
					return 0
				}
				defer check.Body.Close()
				return check.StatusCode
			}).WithTimeout(cleanupTimeout).WithPolling(2*time.Second).Should(Equal(http.StatusNotFound),
				"catalog item %s must be removed", catalogItemID)
		}
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

	It("observes a changed fixture commit on a disposable branch", func() {
		githubToken := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		if githubToken == "" {
			Skip("set GITHUB_TOKEN to run the opt-in GitOps update-reconciliation test")
		}

		owner, repo := githubRepository(repositoryURL)
		updateBranch = gitOpsUpdateBranch()
		baseSHA := githubBranchSHA(owner, repo, repositoryRef, githubToken)
		createGitHubBranch(owner, repo, updateBranch, baseSHA, githubToken)

		file := githubFile(owner, repo, "catalog-item-instance.yaml", repositoryRef, githubToken)
		updatedContent := strings.Replace(string(file.Decoded), "nginx:alpine", "nginx:stable-alpine", 1)
		Expect(updatedContent).NotTo(Equal(string(file.Decoded)), "fixture must contain the expected baseline image")

		updateRepoID = uniqueName("e2e-gitops-update-repository")
		payload := gitRepositoryPayload(updateRepoID, repositoryURL, updateBranch, repositoryPath)
		resp, err := doRequest(http.MethodPost,
			"/git-repositories?id="+url.QueryEscape(updateRepoID), payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		_ = resp.Body.Close()

		var initial map[string]interface{}
		Eventually(func() string {
			initial = getGitRepository(updateRepoID)
			return gitRepositoryState(initial)
		}).WithTimeout(10 * time.Minute).WithPolling(15 * time.Second).Should(Equal("synced"))
		initialCommit := gitRepositoryCommit(initial)
		Expect(initialCommit).NotTo(BeEmpty())

		updateGitHubFile(owner, repo, "catalog-item-instance.yaml", updateBranch, file.SHA, updatedContent, githubToken)

		var updated map[string]interface{}
		Eventually(func() bool {
			updated = getGitRepository(updateRepoID)
			return strings.EqualFold(gitRepositoryState(updated), "synced") &&
				gitRepositoryCommit(updated) != "" &&
				gitRepositoryCommit(updated) != initialCommit
		}).WithTimeout(10*time.Minute).WithPolling(15*time.Second).Should(BeTrue(),
			"GitOps must observe the new commit on the disposable branch")
	})
})

func ensureGitOpsCatalogItem(id string) bool {
	resp, err := doRequest(http.MethodGet, "/catalog-items/"+url.PathEscape(id), "")
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return false
	}
	_ = resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusNotFound), "catalog item %s must be absent or valid", id)

	payload := fmt.Sprintf(`{
		"api_version": "v1alpha1",
		"display_name": %q,
		"spec": {
			"resources": [
				{
					"name": "backend",
					"service_type": "container",
					"fields": %s
				},
				{
					"name": "frontend",
					"service_type": "container",
					"requires_resources": ["backend"],
					"fields": %s
				}
			]
		}
	}`, id, gitOpsCatalogFields("dcm-gitops-backend", "docker.io/library/nginx:alpine"),
		gitOpsCatalogFields("dcm-gitops-frontend", "docker.io/library/httpd:alpine"))

	resp, err = doRequest(http.MethodPost, "/catalog-items?id="+url.QueryEscape(id), payload)
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode == http.StatusConflict {
		_ = resp.Body.Close()
		return false
	}
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	_ = resp.Body.Close()
	return true
}

func createGitOpsRoutingPolicy() string {
	agentName := discoverAgentByServiceType("container", "")
	Expect(agentName).NotTo(BeEmpty(), "embedded container agent must be discoverable")

	packageName := fmt.Sprintf("e2e_gitops_%d", time.Now().UnixNano())
	regoCode := fmt.Sprintf(`package %s

import rego.v1

main := {"selected_agent": %q} if {
	input.spec.service_type == "container"
}`, packageName, agentName)
	payloadBytes, err := json.Marshal(map[string]interface{}{
		"display_name": uniqueName("e2e-gitops-policy"),
		"policy_type":  "GLOBAL",
		"priority":     100,
		"enabled":      true,
		"description":  "E2E GitOps route to embedded container agent",
		"rego_code":    regoCode,
	})
	Expect(err).NotTo(HaveOccurred())

	resp, err := doRequest(http.MethodPost, "/policies", string(payloadBytes))
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))

	var body map[string]interface{}
	decodeJSON(resp, &body)
	policyID := stringField(body, "id")
	Expect(policyID).NotTo(BeEmpty(), "GitOps routing policy must return an ID")
	return policyID
}

func gitOpsCatalogFields(defaultName, image string) string {
	return fmt.Sprintf(`[
		{"path":"metadata.name","display_name":"Name","editable":true,"default":%q},
		{"path":"metadata.labels","display_name":"Labels","editable":true,"default":{}},
		{"path":"image.reference","display_name":"Image","editable":true,"default":%q},
		{"path":"resources.cpu.min","editable":false,"default":"1"},
		{"path":"resources.cpu.max","editable":false,"default":"1"},
		{"path":"resources.memory.min","editable":false,"default":"128MB"},
		{"path":"resources.memory.max","editable":false,"default":"256MB"}
	]`, defaultName, image)
}

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

func gitOpsUpdateBranch() string {
	buildIdentifier := strings.TrimSpace(os.Getenv("BUILD_TAG"))
	if buildIdentifier == "" {
		buildIdentifier = strings.TrimSpace(os.Getenv("BUILD_NUMBER"))
	}
	if buildIdentifier == "" {
		return uniqueName("e2e-gitops-update")
	}

	var sanitized strings.Builder
	previousDash := false
	for _, character := range buildIdentifier {
		valid := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-'
		if valid {
			sanitized.WriteRune(character)
			previousDash = false
		} else if !previousDash {
			sanitized.WriteByte('-')
			previousDash = true
		}
	}

	identifier := strings.Trim(sanitized.String(), "-_")
	if identifier == "" {
		return uniqueName("e2e-gitops-update")
	}
	return uniqueName("e2e-gitops-update-" + identifier)
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

type githubFileResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
	Decoded  []byte `json:"-"`
}

func githubRepository(repositoryURL string) (string, string) {
	parsed, err := url.Parse(repositoryURL)
	Expect(err).NotTo(HaveOccurred())
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	Expect(parts).To(HaveLen(2), "GitHub repository URL must have /owner/repository path")
	return parts[0], strings.TrimSuffix(parts[1], ".git")
}

func githubRequest(method, endpoint, token string, payload interface{}) map[string]interface{} {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		Expect(err).NotTo(HaveOccurred())
		body = strings.NewReader(string(data))
	}
	request, err := http.NewRequest(method, "https://api.github.com"+endpoint, body)
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	Expect(err).NotTo(HaveOccurred())
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.StatusCode).To(BeNumerically("<", 300),
		"GitHub API %s %s returned %d: %s", method, endpoint, response.StatusCode, string(data))
	if len(data) == 0 {
		return map[string]interface{}{}
	}
	var result map[string]interface{}
	Expect(json.Unmarshal(data, &result)).To(Succeed())
	return result
}

func githubBranchSHA(owner, repo, branch, token string) string {
	response := githubRequest(http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, url.PathEscape(branch)), token, nil)
	object, ok := response["object"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	sha, _ := object["sha"].(string)
	Expect(sha).NotTo(BeEmpty())
	return sha
}

func createGitHubBranch(owner, repo, branch, sha, token string) {
	githubRequest(http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), token, map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	})
}

func githubFile(owner, repo, path, branch, token string) githubFileResponse {
	response := githubRequest(http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, url.QueryEscape(branch)), token, nil)
	data, err := json.Marshal(response)
	Expect(err).NotTo(HaveOccurred())
	var file githubFileResponse
	Expect(json.Unmarshal(data, &file)).To(Succeed())
	Expect(file.Encoding).To(Equal("base64"))
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	Expect(err).NotTo(HaveOccurred())
	file.Decoded = decoded
	return file
}

func updateGitHubFile(owner, repo, path, branch, sha, content, token string) {
	githubRequest(http.MethodPut,
		fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path), token, map[string]string{
			"message": "test: update GitOps fixture",
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
			"branch":  branch,
			"sha":     sha,
		})
}

func deleteGitHubBranch(repositoryURL, branch string) {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return
	}
	owner, repo := githubRepository(repositoryURL)
	githubRequest(http.MethodDelete,
		fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, url.PathEscape(branch)), token, nil)
}
