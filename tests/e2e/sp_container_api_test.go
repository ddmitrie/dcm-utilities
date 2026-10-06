//go:build e2e

package e2e_test

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Container SP API", Label("sp", "container"), func() {
	BeforeEach(func() {
		requireContainerSP()
	})

	Context("registration", func() {
		It("registers with DCM as a container agent", func() {
			resp, err := doRequest(http.MethodGet, "/agents", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("agents"))

			agents, ok := body["agents"].([]interface{})
			Expect(ok).To(BeTrue())

			var found map[string]interface{}
			for _, a := range agents {
				agent := a.(map[string]interface{})
				serviceTypes, _ := agent["service_types"].([]interface{})
				for _, st := range serviceTypes {
					if s, _ := st.(string); s == "container" {
						found = agent
						break
					}
				}
				if found != nil {
					break
				}
			}
			Expect(found).NotTo(BeNil(), "no agent with service_types containing 'container' found")
		})
	})

	Context("health", func() {
		It("returns healthy status", func() {
			resp, err := doContainerSPRequest(http.MethodGet, "/containers/health", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body["status"]).To(Equal("healthy"))
			Expect(body).To(HaveKey("uptime"))
			Expect(body).To(HaveKey("version"))
			Expect(body).To(HaveKey("type"))
		})
	})

	Context("CRUD lifecycle", Ordered, func() {
		var containerIDs []string

		AfterAll(func() {
			for _, id := range containerIDs {
				deleteTestContainer(id)
			}
		})

		It("creates a container with valid spec", func() {
			name := uniqueName("e2e-create")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))

			id := body["id"].(string)
			Expect(id).NotTo(BeEmpty())
			containerIDs = append(containerIDs, id)
		})

		It("creates a container with a custom ID", func() {
			skipUnlessDirectContainerSP()
			customID := uniqueName("e2e-custom")
			resp, err := doContainerSPRequest(http.MethodPost,
				"/containers?id="+customID,
				containerSpec(customID, "docker.io/library/nginx:alpine", false))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body["id"]).To(Equal(customID))
			containerIDs = append(containerIDs, customID)
		})

		It("generates an ID when none is provided", func() {
			name := uniqueName("e2e-autoid")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))

			id := body["id"].(string)
			Expect(id).NotTo(BeEmpty())
			containerIDs = append(containerIDs, id)
		})

		It("creates a container with ports and provisions a Service", func() {
			skipUnlessDirectContainerSP()
			name := uniqueName("e2e-ports")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", true))

			id := body["id"].(string)
			containerIDs = append(containerIDs, id)

			Eventually(func() interface{} {
				resp, err := doContainerSPRequest(http.MethodGet, "/containers/"+id, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				var getBody map[string]interface{}
				decodeJSON(resp, &getBody)
				return getBody["service"]
			}).WithTimeout(15*time.Second).WithPolling(2*time.Second).ShouldNot(BeNil(),
				"service field should be populated on GET after Service creation")
		})

		It("gets an existing container by ID", func() {
			Expect(containerIDs).NotTo(BeEmpty())
			id := containerIDs[0]

			Eventually(func() string {
				resp, err := doContainerSPRequest(http.MethodGet, "/containers/"+id, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				var body map[string]interface{}
				decodeJSON(resp, &body)
				s, _ := body["status"].(string)
				return s
			}).WithTimeout(120*time.Second).WithPolling(3*time.Second).Should(Equal("RUNNING"),
				"container should reach RUNNING status")
		})

		It("lists all managed containers", func() {
			resp, err := doContainerSPRequest(http.MethodGet, "/containers", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			Expect(body).To(HaveKey("containers"))

			containers, ok := body["containers"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(len(containers)).To(BeNumerically(">=", len(containerIDs)),
				"list should include at least all containers we created")
		})

		It("paginates container list", func() {
			resp, err := doContainerSPRequest(http.MethodGet, "/containers?max_page_size=2", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var page1 map[string]interface{}
			decodeJSON(resp, &page1)
			containers := page1["containers"].([]interface{})
			Expect(len(containers)).To(BeNumerically("<=", 2))

			if token, ok := page1["next_page_token"].(string); ok && token != "" {
				resp2, err := doContainerSPRequest(http.MethodGet,
					"/containers?max_page_size=2&page_token="+token, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp2.StatusCode).To(Equal(http.StatusOK))

				var page2 map[string]interface{}
				decodeJSON(resp2, &page2)
				Expect(page2).To(HaveKey("containers"))
			}
		})
	})

	Context("RFC 9457 error format", Label("contract"), func() {
		BeforeEach(func() {
			skipUnlessDirectContainerSP()
		})

		It("returns problem+json with status and project URI on validation error", func() {
			resp, err := doContainerSPRequest(http.MethodPost, "/containers", `{}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			expectRFC9457Problem(resp, problemDetailExpectation{
				Status:     http.StatusBadRequest,
				TypeSuffix: "invalid-argument",
				Title:      invalidArgumentTitle,
			})
		})

		It("returns errors array on multi-field validation failure", func() {
			body := `{
				"spec": {
					"service_type": "container",
					"metadata": {
						"name": "e2e-multi-err",
						"labels": {"dcm.project/managed-by": "bad"}
					},
					"image": {"reference": "docker.io/library/nginx:alpine"},
					"resources": {
						"cpu": {"min": "10", "max": "5"},
						"memory": {"min": "128MB", "max": "256MB"}
					}
				}
			}`
			resp, err := doContainerSPRequest(http.MethodPost, "/containers", body)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			problem := readContainerProblemDetail(resp, http.StatusBadRequest)
			Expect(problem.ProblemDetail).To(Equal(ProblemDetail{
				Type:   problemTypeBaseURI + "invalid-argument",
				Title:  invalidArgumentTitle,
				Status: http.StatusBadRequest,
				Detail: containerMultiErrorDetail,
			}))
			Expect(problem.Errors).To(HaveLen(2))
			Expect(problem.Errors[0].Detail).To(ContainSubstring("cpu.min"))
			Expect(problem.Errors[0].Pointer).NotTo(BeNil())
			Expect(*problem.Errors[0].Pointer).To(Equal("#/spec/resources/cpu/min"))
			Expect(problem.Errors[1].Detail).To(ContainSubstring("dcm.project/managed-by"))
			Expect(problem.Errors[1].Pointer).NotTo(BeNil())
			Expect(*problem.Errors[1].Pointer).To(Equal("#/spec/metadata/labels/dcm.project~1managed-by"))
		})
	})

	Context("validation errors", func() {
		It("rejects an empty body", func() {
			resp, err := doContainerSPRequest(http.MethodPost, "/containers", "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("rejects a body with missing required fields", func() {
			resp, err := doContainerSPRequest(http.MethodPost, "/containers", `{"spec":{}}`)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("rejects invalid field types", func() {
			resp, err := doContainerSPRequest(http.MethodPost, "/containers", `{"spec": {"service_type": 12345}}`)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("rejects wrong content type", func() {
			skipUnlessDirectContainerSP()
			url := containerSPBaseURL + "/containers"
			req, err := http.NewRequest(http.MethodPost, url, nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "text/plain")

			resp, err := unauthenticatedClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})
	})

	Context("not found", func() {
		It("returns 404 for GET on non-existent container", func() {
			resp, err := doContainerSPRequest(http.MethodGet, "/containers/does-not-exist", "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("returns 404 for DELETE on non-existent container", func() {
			resp, err := doContainerSPRequest(http.MethodDelete, "/containers/does-not-exist", "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})

	// Verify the SP's label selector excludes non-DCM Deployments.
	Context("label filtering", Label("cluster"), Ordered, func() {
		manualDeployName := uniqueName("e2e-manual")

		BeforeAll(func() {
			requireKubectl()
		})

		AfterAll(func() {
			_, _ = runKubectl("delete", "deployment", manualDeployName, "--ignore-not-found")
		})

		It("excludes non-DCM Deployments from list", func() {
			By("creating a Deployment without DCM labels via kubectl")
			_, err := runKubectl("create", "deployment", manualDeployName, "--image=docker.io/library/nginx:alpine")
			Expect(err).NotTo(HaveOccurred())

			By("listing containers via the SP API")
			resp, err := doContainerSPRequest(http.MethodGet, "/containers", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			decodeJSON(resp, &body)
			containers, _ := body["containers"].([]interface{})

			for _, c := range containers {
				container := c.(map[string]interface{})
				spec, _ := container["spec"].(map[string]interface{})
				meta, _ := spec["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				Expect(name).NotTo(Equal(manualDeployName),
					"non-DCM Deployment %q should not appear in SP list", manualDeployName)
			}
		})
	})

	Context("delete lifecycle", Ordered, func() {
		var deleteID string

		It("creates a container to delete", func() {
			name := uniqueName("e2e-delete")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", true))
			deleteID = body["id"].(string)
		})

		It("deletes the container and its K8s resources", func() {
			Expect(deleteID).NotTo(BeEmpty())

			resp, err := doContainerSPRequest(http.MethodDelete, "/containers/"+deleteID, "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

			By("confirming the container is gone")
			resp, err = doContainerSPRequest(http.MethodGet, "/containers/"+deleteID, "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})
})
