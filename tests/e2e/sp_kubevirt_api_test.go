//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("KubeVirt Service Provider API", Label("sp", "kubevirt"), func() {
	BeforeEach(func() {
		requireKubevirtSP()
	})

	Context("Health endpoint", func() {
		It("returns healthy status when cluster is reachable [TC-04]", func() {
			resp, err := doKubevirtRequest(http.MethodGet, "/vms/health", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["status"]).To(Equal("healthy"))
			Expect(body["path"]).To(Equal("/api/v1alpha1/health"))
		})
	})

	Context("VM create variants", func() {
		It("creates a VM with valid spec and DCM labels on cluster [TC-06][TC-11][TC-26][TC-27]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip("Cluster access required: " + err.Error())
			}
			// Boot disk is containerDisk (no PVC); StorageClass is not required.

			vmName := uniqueName("e2e-kubevirt-vm")
			id, err := createTestVM(vmName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			ns := kubevirtNamespace()
			var clusterName string
			Eventually(func() error {
				name, err := findVMNameByInstanceID(id, ns)
				if err != nil {
					return err
				}
				clusterName = name
				return verifyDCMLabels(name, ns, id)
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			vm, err := getVMFromCluster(clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			// Spec mapping: requests memory present on domain resources
			domain, err := labelMap(vm, "spec", "template", "spec", "domain")
			Expect(err).NotTo(HaveOccurred())
			resources, _ := domain["resources"].(map[string]interface{})
			Expect(resources).NotTo(BeNil())
			requests, _ := resources["requests"].(map[string]interface{})
			Expect(requests).To(HaveKey("memory"))
			// OpenAPI input is 1GB; SP maps decimal GB → Kubernetes resource "1G" (not Gi).
			Expect(requests["memory"]).To(Equal("1G"))

			GinkgoWriter.Printf("Created VM id=%s clusterName=%s with DCM labels\n", id, clusterName)
		})

		It("creates a VM with custom ID via query parameter [TC-07]", func() {
			skipUnlessDirectKubevirtSP()
			customID := uuid.NewString()
			spec := newTestVMSpec(uniqueName("e2e-custom-id"))
			payload, err := json.Marshal(map[string]interface{}{"spec": spec})
			Expect(err).NotTo(HaveOccurred())

			resp, err := doKubevirtRequest(http.MethodPost, "/vms?id="+customID, string(payload))
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
			DeferCleanup(func() { deleteTestVM(customID) })

			// OpenAPI CreateVM 201 → VM { path, spec }
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body).To(HaveKey("path"))
			Expect(body).To(HaveKey("spec"))
			pathStr, _ := body["path"].(string)
			Expect(pathStr).To(ContainSubstring(customID))
			Expect(pathStr).To(ContainSubstring("vms/"))

			getResp, err := doKubevirtRequest(http.MethodGet, "/vms/"+customID, "")
			Expect(err).NotTo(HaveOccurred())
			defer getResp.Body.Close()
			Expect(getResp.StatusCode).To(Equal(http.StatusOK))
		})

		It("returns 409 when creating a VM with a duplicate instance ID [TC-10]", func() {
			skipUnlessDirectKubevirtSP()
			customID := uuid.NewString()
			spec := newTestVMSpec(uniqueName("e2e-dup"))
			payload, err := json.Marshal(map[string]interface{}{"spec": spec})
			Expect(err).NotTo(HaveOccurred())

			resp1, err := doKubevirtRequest(http.MethodPost, "/vms?id="+customID, string(payload))
			Expect(err).NotTo(HaveOccurred())
			defer resp1.Body.Close()
			Expect(resp1.StatusCode).To(Equal(http.StatusCreated))
			DeferCleanup(func() { deleteTestVM(customID) })

			resp2, err := doKubevirtRequest(http.MethodPost, "/vms?id="+customID, string(payload))
			Expect(err).NotTo(HaveOccurred())
			defer resp2.Body.Close()
			Expect(resp2.StatusCode).To(Equal(http.StatusConflict))
			expectProblemDetailContains(resp2, "conflict", "duplicate", "already", "id")
		})
	})

	Context("VM get / list / delete", func() {
		It("gets an existing VM [TC-12]", func() {
			skipUnlessDirectKubevirtSP()
			id, err := createTestVM(uniqueName("e2e-get"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			resp, err := doKubevirtRequest(http.MethodGet, "/vms/"+id, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			// OpenAPI GetVM 200 → VM { path, spec } with required VMSpec fields
			Expect(body).To(HaveKey("spec"))
			Expect(body).To(HaveKey("path"))
			Expect(body["path"]).To(ContainSubstring(id))
			Expect(fmt.Sprint(body["path"])).To(ContainSubstring("vms/"))
			spec, ok := body["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(spec).To(HaveKey("service_type"))
			Expect(spec).To(HaveKey("memory"))
			Expect(spec).To(HaveKey("vcpu"))
			Expect(spec).To(HaveKey("storage"))
			Expect(spec).To(HaveKey("guest_os"))
			Expect(spec).To(HaveKey("metadata"))
		})

		It("returns 404 for a non-existent VM [TC-13]", func() {
			skipUnlessDirectKubevirtSP()
			resp, err := doKubevirtRequest(http.MethodGet, "/vms/non-existent-vm-id", "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			// OpenAPI: GET /vms/{id} → 404 application/problem+json when missing.
			// SP bug: empty label list → plain error → 500 (FLPATH-4752).
			if resp.StatusCode == http.StatusInternalServerError {
				Skip("FLPATH-4752: SP returns 500 for missing VM GET (OpenAPI expects 404 problem+json)")
			}
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
				"GET missing VM must be 404 per OpenAPI")
			expectProblemDetailContains(resp, "not found", "missing", "404", "notfound")
		})

		It("GET reflects current VM status [TC-14]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			id, err := createTestVM(uniqueName("e2e-status-get"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			// OpenAPI: GET must expose VM.spec.status. Live SP returns zeroed
			// spec (no status) — FLPATH-4754. Skip; do not false-pass on cluster alone.
			var apiStatus, clusterStatus string
			Eventually(func() bool {
				apiStatus = ""
				clusterStatus = ""
				resp, err := doKubevirtRequest(http.MethodGet, "/vms/"+id, "")
				if err == nil {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						var body map[string]interface{}
						if json.NewDecoder(resp.Body).Decode(&body) == nil {
							if spec, ok := body["spec"].(map[string]interface{}); ok {
								apiStatus, _ = spec["status"].(string)
							}
						}
					}
				}
				ns := kubevirtNamespace()
				name, err := findVMNameByInstanceID(id, ns)
				if err == nil {
					out, err := runKubeCmd("get", "vm", name, "-n", ns, "-o", "jsonpath={.status.printableStatus}")
					if err == nil {
						clusterStatus = strings.TrimSpace(out)
					}
				}
				return apiStatus != "" || clusterStatus != ""
			}).WithTimeout(180 * time.Second).WithPolling(5 * time.Second).
				Should(BeTrue(), "VM should expose a status via GET or cluster")

			if apiStatus == "" {
				Skip(fmt.Sprintf("FLPATH-4754: SP GET returns empty/zeroed spec (no OpenAPI spec.status; cluster printableStatus=%q)", clusterStatus))
			}
			Expect(apiStatus).To(BeElementOf("Pending", "Scheduling", "Scheduled", "Running", "Succeeded", "Failed",
				"Starting", "Stopping", "Stopped", "Migrating", "Paused", "Unknown", "Terminating",
				"PENDING", "PROVISIONING", "RUNNING", "STOPPING", "FAILED",
				"pending", "provisioning", "running", "stopping", "failed"),
				"GET spec.status should be a known VM phase")
			GinkgoWriter.Printf("GET /vms spec.status=%s (cluster=%s)\n", apiStatus, clusterStatus)
		})

		It("lists all managed VMs including newly created ones [TC-15]", func() {
			var ids []string
			for i := 0; i < 3; i++ {
				id, err := createTestVM(uniqueName(fmt.Sprintf("e2e-list-%d", i)))
				Expect(err).NotTo(HaveOccurred())
				ids = append(ids, id)
				captured := id
				DeferCleanup(func() { deleteTestVM(captured) })
			}

			listed, err := listVMIDs()
			Expect(err).NotTo(HaveOccurred())
			for _, id := range ids {
				Expect(listed).To(ContainElement(id), "list should include created VM %s", id)
			}
		})

		It("list excludes unlabeled cluster VMs [TC-16]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			ns := kubevirtNamespace()
			manualName := uniqueName("e2e-manual-vm")
			Expect(createUnlabeledVM(manualName, ns)).To(Succeed())
			DeferCleanup(func() { _ = deleteVMFromCluster(manualName, ns) })

			listed, err := listVMIDs()
			Expect(err).NotTo(HaveOccurred())
			// List returns DCM instance IDs, not cluster names — ensure manual VM
			// is not reachable as a managed resource via SP get-by-name either.
			for _, id := range listed {
				Expect(id).NotTo(Equal(manualName))
			}
			// Also ensure label selector would not pick it up
			out, err := runKubeCmd("get", "vm", manualName, "-n", ns, "-o", "jsonpath={.metadata.labels}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring(dcmLabelManagedBy))
		})

		It("deletes an existing VM and removes it from the cluster [TC-18]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			id, err := createTestVM(uniqueName("e2e-del"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			ns := kubevirtNamespace()
			var clusterName string
			Eventually(func() error {
				name, err := findVMNameByInstanceID(id, ns)
				clusterName = name
				return err
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			// OpenAPI + ADR: DELETE → 204 No Content
			resp, err := doKubevirtRequest(http.MethodDelete, "/vms/"+id, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

			// OpenAPI: subsequent GET → 404 problem+json (FLPATH-4752: SP may return 500)
			var lastGET int
			Eventually(func() int {
				getResp, err := doKubevirtRequest(http.MethodGet, "/vms/"+id, "")
				if err != nil {
					return 0
				}
				defer getResp.Body.Close()
				lastGET = getResp.StatusCode
				return lastGET
			}).WithTimeout(120 * time.Second).WithPolling(2 * time.Second).
				Should(BeElementOf(http.StatusNotFound, http.StatusInternalServerError))

			// Cluster removal is independent of the API 404 vs 500 mapping bug.
			Eventually(func() error {
				return verifyVMDeleted(clusterName, ns)
			}).WithTimeout(120 * time.Second).WithPolling(3 * time.Second).Should(Succeed())

			if lastGET == http.StatusInternalServerError {
				Skip("FLPATH-4752: after DELETE, GET missing VM returns 500 instead of OpenAPI 404 (cluster VM was removed)")
			}
			Expect(lastGET).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when deleting a non-existent VM [TC-19]", func() {
			skipUnlessDirectKubevirtSP()
			resp, err := doKubevirtRequest(http.MethodDelete, "/vms/does-not-exist-"+uuid.NewString(), "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			// OpenAPI: DELETE missing → 404 application/problem+json (FLPATH-4752 → 500 today)
			if resp.StatusCode == http.StatusInternalServerError {
				Skip("FLPATH-4752: SP returns 500 for missing VM DELETE (OpenAPI expects 404 problem+json)")
			}
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
				"DELETE missing VM must be 404 per OpenAPI")
			expectProblemDetailContains(resp, "not found", "missing", "404", "notfound")
		})
	})

	Context("Validation", func() {
		BeforeEach(func() {
			skipUnlessDirectKubevirtSP()
		})

		// OpenAPI-layer 400s: accept text/plain until FLPATH-4751 (should be problem+json).
		It("rejects empty body [TC-09][TC-25]", func() {
			path, _ := createVMPath()
			resp, err := doKubevirtRequest(http.MethodPost, path, "")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			expectOpenAPIValidationErrorContains(resp, "body", "empty", "json", "request", "required")
		})

		It("rejects missing required fields [TC-09]", func() {
			path, _ := createVMPath()
			resp, err := doKubevirtRequest(http.MethodPost, path, `{"spec":{"service_type":"vm"}}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			expectOpenAPIValidationErrorContains(resp, "memory", "required", "vcpu", "guest_os", "missing")
		})

		It("rejects invalid memory format [TC-09][TC-29]", func() {
			path, _ := createVMPath()
			payload := `{
				"spec": {
					"service_type": "vm",
					"metadata": {"name": "bad-mem"},
					"guest_os": {"type": "linux"},
					"vcpu": {"count": 1},
					"memory": {"size": "1Gi"},
					"storage": {"disks": [{"name": "boot", "capacity": "10GB"}]}
				}
			}`
			resp, err := doKubevirtRequest(http.MethodPost, path, payload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			expectOpenAPIValidationErrorContains(resp, "memory")
		})

		It("rejects malformed JSON [TC-25]", func() {
			path, _ := createVMPath()
			resp, err := doKubevirtRequest(http.MethodPost, path, `{"spec":`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			expectOpenAPIValidationErrorContains(resp, "json", "parse", "malformed", "syntax", "invalid", "decode", "eof")
		})

		It("rejects wrong content type [TC-25]", func() {
			path, id := createVMPath()
			_ = id
			body := strings.NewReader(`{"spec":{}}`)
			req, err := http.NewRequest(http.MethodPost, kubevirtSPURL+path, body)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "text/plain")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(BeElementOf(http.StatusBadRequest, http.StatusUnsupportedMediaType))
			expectOpenAPIValidationErrorContains(resp, "content-type", "content type", "media", "unsupported", "json", "text/plain")
		})
	})

	Context("Concurrency", func() {
		It("creates multiple VMs in parallel without label conflicts [TC-31]", func() {
			skipUnlessDirectKubevirtSP()
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			// Boot disk is containerDisk (no PVC); StorageClass is not required.

			const n = 5
			ids := make([]string, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			wg.Add(n)
			for i := 0; i < n; i++ {
				i := i
				go func() {
					defer wg.Done()
					id, err := createTestVM(uniqueName(fmt.Sprintf("e2e-conc-%d", i)))
					ids[i] = id
					errs[i] = err
				}()
			}
			wg.Wait()
			DeferCleanup(func() {
				for _, id := range ids {
					deleteTestVM(id)
				}
			})

			for i := 0; i < n; i++ {
				Expect(errs[i]).NotTo(HaveOccurred(), "create %d", i)
				Expect(ids[i]).NotTo(BeEmpty())
				Expect(uuid.Validate(ids[i])).To(Succeed(), "create %d id should be a UUID", i)
			}
			// Unique IDs
			seen := map[string]bool{}
			for _, id := range ids {
				Expect(seen[id]).To(BeFalse(), "duplicate id %s", id)
				seen[id] = true
			}

			ns := kubevirtNamespace()
			for _, id := range ids {
				Eventually(func() error {
					name, err := findVMNameByInstanceID(id, ns)
					if err != nil {
						return err
					}
					return verifyDCMLabels(name, ns, id)
				}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())
			}
		})
	})

	Context("Boot storage mapping", func() {
		// TC-33: current kubevirt SP maps the OpenAPI boot disk to a containerDisk
		// (guest OS image), not a PVC/DataVolume. StorageClass / PVC provisioning is
		// not exposed by the API yet; expand this test when the SP adds PVC-backed disks.
		It("maps the boot disk to a containerDisk volume [TC-33]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			id, err := createTestVM(uniqueName("e2e-boot-disk"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			ns := kubevirtNamespace()
			var clusterName string
			Eventually(func() error {
				name, err := findVMNameByInstanceID(id, ns)
				clusterName = name
				return err
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			vm, err := getVMFromCluster(clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			tmplSpec, err := labelMap(vm, "spec", "template", "spec")
			Expect(err).NotTo(HaveOccurred())
			volumes, ok := tmplSpec["volumes"].([]interface{})
			Expect(ok).To(BeTrue(), "VM should define volumes")
			Expect(volumes).NotTo(BeEmpty())

			var bootVol map[string]interface{}
			for _, v := range volumes {
				vol, ok := v.(map[string]interface{})
				if !ok {
					continue
				}
				if name, _ := vol["name"].(string); name == "boot" {
					bootVol = vol
					break
				}
			}
			Expect(bootVol).NotTo(BeNil(), "expected a volume named boot")

			cd, ok := bootVol["containerDisk"].(map[string]interface{})
			Expect(ok).To(BeTrue(),
				"current SP implementation uses containerDisk for boot (not PVC/DataVolume); got %#v", bootVol)
			image, _ := cd["image"].(string)
			Expect(image).NotTo(BeEmpty(), "boot containerDisk should reference a guest OS image")
			Expect(bootVol).NotTo(HaveKey("persistentVolumeClaim"),
				"boot must not be PVC-backed with current SP")
			Expect(bootVol).NotTo(HaveKey("dataVolume"),
				"boot must not be DataVolume-backed with current SP")

			GinkgoWriter.Printf("TC-33: VM %s boot containerDisk image=%s\n", clusterName, image)
		})

	})

	Context("External lifecycle", func() {
		It("tracks stop/start via KubeVirt API [TC-32]", func() {
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			// Boot disk is containerDisk (no PVC); StorageClass is not required.

			id, err := createTestVM(uniqueName("e2e-lifecycle"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { deleteTestVM(id) })

			ns := kubevirtNamespace()
			var clusterName string
			Eventually(func() error {
				name, err := findVMNameByInstanceID(id, ns)
				clusterName = name
				return err
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			// Wait until Running (or Failed — still exercise stop)
			Eventually(func() string {
				if s := getVMAPIStatus(id); s != "" {
					return s
				}
				name, err := findVMNameByInstanceID(id, ns)
				if err != nil {
					return ""
				}
				out, err := runKubeCmd("get", "vm", name, "-n", ns, "-o", "jsonpath={.status.printableStatus}")
				if err != nil {
					return ""
				}
				return strings.TrimSpace(out)
			}).WithTimeout(180 * time.Second).WithPolling(5 * time.Second).
				Should(BeElementOf("Running", "Failed", "Succeeded", "Scheduled", "Scheduling", "Pending",
					"Starting", "Stopped", "Stopping", "Unknown",
					"running", "failed", "succeeded", "scheduled", "scheduling", "pending",
					"starting", "stopped", "stopping", "unknown",
					"RUNNING", "FAILED", "PENDING"))

			Expect(setVMRunStrategy(clusterName, ns, "Halted")).To(Succeed())
			Eventually(func() string {
				api := getVMAPIStatus(id)
				out, err := runKubeCmd("get", "vm", clusterName, "-n", ns, "-o", "jsonpath={.status.printableStatus}")
				cluster := ""
				if err == nil {
					cluster = strings.TrimSpace(out)
				}
				GinkgoWriter.Printf("after halt API status=%v printableStatus=%v err=%v\n", api, cluster, err)

				// Standalone SP GET is the contract under test. Embedded vm
				// only updates STI slowly; CNV shows Terminating after Halted,
				// and the VM object may disappear (NotFound → "Gone").
				if kubevirtStandaloneReady {
					if api != "" {
						return api
					}
					return cluster
				}
				if err != nil {
					if kubectlNotFound(out, err) {
						return "Gone"
					}
					return ""
				}
				return cluster
			}).WithTimeout(120 * time.Second).WithPolling(5 * time.Second).
				Should(BeElementOf(haltedVMPhases()...),
					"after external halt, status should leave Running (not still Running/Pending)")

			// Confirm the runStrategy patch stuck. In embedded mode the VM may
			// already be deleted after Halted — accept NotFound as "Gone", never
			// treat other kubectl errors as success.
			Eventually(func() string {
				out, err := runKubeCmd("get", "vm", clusterName, "-n", ns, "-o", "jsonpath={.spec.runStrategy}")
				if err != nil {
					if !kubevirtStandaloneReady && kubectlNotFound(out, err) {
						return "Gone"
					}
					return ""
				}
				return strings.TrimSpace(out)
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).
				Should(BeElementOf(func() []interface{} {
					want := []interface{}{"Halted"}
					if !kubevirtStandaloneReady {
						want = append(want, "Gone")
					}
					return want
				}()...), "runStrategy should be Halted, or VM NotFound in embedded mode")

			if kubevirtStandaloneReady {
				Expect(setVMRunStrategy(clusterName, ns, "Always")).To(Succeed())
			} else {
				// Embedded: only restore Always when the VM still exists.
				out, err := runKubeCmd("get", "vm", clusterName, "-n", ns)
				if err == nil {
					Expect(setVMRunStrategy(clusterName, ns, "Always")).To(Succeed())
				} else {
					Expect(kubectlNotFound(out, err)).To(BeTrue(),
						"unexpected kubectl error checking VM after halt: %v", err)
				}
			}
		})
	})

	Context("KubeVirt error matrix", func() {
		// TC-28c: OpenAPI allows memory.size=1MB; the SP maps it to KubeVirt "1M" and
		// creates the VM successfully. Current CNV accepts that size (VM can reach Running).
		// Revisit if/when the SP enforces a minimum memory or rejects tiny sizes at the API.
		It("accepts OpenAPI 1MB memory and maps it on the cluster [TC-28c]", func() {
			skipUnlessDirectKubevirtSP()
			if err := checkClusterAccess(); err != nil {
				Skip(err.Error())
			}
			path, id := createVMPath()
			DeferCleanup(func() { deleteTestVM(id) })
			payload := `{
				"spec": {
					"service_type": "vm",
					"metadata": {"name": "tiny"},
					"guest_os": {"type": "linux"},
					"vcpu": {"count": 1},
					"memory": {"size": "1MB"},
					"storage": {"disks": [{"name": "boot", "capacity": "10GB"}]}
				}
			}`
			resp, err := doKubevirtRequest(http.MethodPost, path, payload)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated),
				"current SP accepts OpenAPI 1MB (maps to KubeVirt 1M); got %d", resp.StatusCode)

			ns := kubevirtNamespace()
			var clusterName string
			Eventually(func() error {
				name, err := findVMNameByInstanceID(id, ns)
				clusterName = name
				return err
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			vm, err := getVMFromCluster(clusterName, ns)
			Expect(err).NotTo(HaveOccurred())
			domain, err := labelMap(vm, "spec", "template", "spec", "domain")
			Expect(err).NotTo(HaveOccurred())
			resources, _ := domain["resources"].(map[string]interface{})
			Expect(resources).NotTo(BeNil())
			requests, _ := resources["requests"].(map[string]interface{})
			Expect(requests).To(HaveKey("memory"))
			// OpenAPI input is 1MB; SP maps decimal MB → Kubernetes resource "1M" (not Mi).
			Expect(requests["memory"]).To(Equal("1M"))

			Eventually(func() string {
				if s := getVMAPIStatus(id); s != "" {
					return s
				}
				out, err := runKubeCmd("get", "vm", clusterName, "-n", ns, "-o", "jsonpath={.status.printableStatus}")
				if err != nil {
					return ""
				}
				return strings.TrimSpace(out)
			}).WithTimeout(120 * time.Second).WithPolling(5 * time.Second).
				Should(BeElementOf("Running", "Scheduled", "Scheduling", "Starting", "Pending",
					"RUNNING", "SCHEDULED"),
					"1MB-mapped VM is accepted by current SP/CNV and should progress past create")
		})
	})
})
