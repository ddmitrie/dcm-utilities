//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Container SP Status Events", Label("sp", "container", "nats"), func() {
	BeforeEach(func() {
		requireContainerSP()
	})

	Context("CloudEvent format", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		It("publishes a CloudEvent on container creation", func() {
			name := uniqueName("e2e-ce")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			Eventually(func() int {
				return len(collector.EventsForInstance(containerID))
			}).WithTimeout(30 * time.Second).WithPolling(1 * time.Second).Should(BeNumerically(">", 0),
				"expected at least one NATS event for container %s", containerID)

			events := collector.EventsForInstance(containerID)
			evt := events[0]

			By("validating CloudEvent required fields")
			Expect(evt.SpecVersion).To(Equal("1.0"))
			Expect(evt.ID).NotTo(BeEmpty())
			Expect(evt.Source).To(HavePrefix("dcm/providers/"), "source should be dcm/providers/<providerName>")
			Expect(evt.Type).NotTo(BeEmpty())
			Expect(evt.Time).NotTo(BeEmpty())
			Expect(evt.DataContentType).To(Equal("application/json"))
		})
	})

	Context("healthy container lifecycle", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		It("transitions to RUNNING and emits status events", func() {
			name := uniqueName("e2e-run")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			By("waiting for RUNNING status via NATS")
			evt := collector.WaitForStatus(containerID, "RUNNING", 60*time.Second)
			Expect(evt.Data["status"]).To(Equal("RUNNING"))
			Expect(evt.Data["id"]).To(Equal(containerID))

			By("confirming GET also shows RUNNING")
			resp, err := doContainerSPRequest(http.MethodGet, "/containers/"+containerID, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var getBody map[string]interface{}
			decodeJSON(resp, &getBody)
			Expect(getBody["status"]).To(Equal("RUNNING"))
		})

		// PENDING may be debounced away if the Pod starts quickly (cached image).
		// Verify the event sequence only contains valid statuses and ends with RUNNING.
		It("has a valid status progression ending in RUNNING", func() {
			events := collector.EventsForInstance(containerID)
			Expect(events).NotTo(BeEmpty())

			validStatuses := map[string]bool{"PENDING": true, "RUNNING": true}
			for _, e := range events {
				s, _ := e.Data["status"].(string)
				Expect(validStatuses).To(HaveKey(s), "unexpected status %q in event history", s)
			}
			last := events[len(events)-1]
			Expect(last.Data["status"]).To(Equal("RUNNING"))
		})
	})

	Context("image pull failure", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		// The SP maps ImagePullBackOff → PENDING (not FAILED).
		// This is by design: the status informer sees it as a transient waiting state.
		It("reports PENDING for an invalid image (ImagePullBackOff)", func() {
			name := uniqueName("e2e-bad")
			badImage := fmt.Sprintf("quay.io/nonexistent/image-%d:fake", time.Now().UnixNano())
			body := createTestContainer(containerSpec(name, badImage, false))
			containerID = body["id"].(string)

			By("waiting for a PENDING event")
			evt := collector.WaitForStatus(containerID, "PENDING", 60*time.Second)
			Expect(evt.Data["status"]).To(Equal("PENDING"))

			By("confirming it stays PENDING and does not transition to RUNNING")
			Consistently(func() string {
				events := collector.EventsForInstance(containerID)
				for _, e := range events {
					if s, _ := e.Data["status"].(string); s == "RUNNING" {
						return "RUNNING"
					}
				}
				return "PENDING"
			}).WithTimeout(15 * time.Second).WithPolling(3 * time.Second).Should(Equal("PENDING"))
		})
	})

	// CrashLoopBackOff does NOT produce FAILED because the SP maps Pod *phase*,
	// not container-level waiting reasons. With restartPolicy=Always (Deployment
	// default), the Pod phase stays "Running" even while the container is in
	// CrashLoopBackOff. The SP would need to inspect ContainerStatuses to detect
	// this — currently a known limitation (see ReconcileStatus in the SP repo).
	Context("crashing container", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		It("reports RUNNING (not FAILED) for a CrashLoopBackOff container", func() {
			name := uniqueName("e2e-crash")
			body := createTestContainer(containerSpecWith(name, "docker.io/library/busybox:latest", containerSpecOpts{
				command: []string{"false"},
			}))
			containerID = body["id"].(string)

			By("waiting for RUNNING status — Pod phase stays Running despite CrashLoopBackOff")
			evt := collector.WaitForStatus(containerID, "RUNNING", 60*time.Second)
			Expect(evt.Data["status"]).To(Equal("RUNNING"))
			Expect(evt.Data["id"]).To(Equal(containerID))
		})
	})

	Context("first observed status", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		// The first observed event is PENDING when the Pod takes time to start,
		// or RUNNING if the image is cached and the Pod starts within the
		// debounce window (500ms default). Both are valid.
		It("emits PENDING or RUNNING as the first observed status", func() {
			name := uniqueName("e2e-init")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			Eventually(func() int {
				return len(collector.EventsForInstance(containerID))
			}).WithTimeout(15 * time.Second).WithPolling(1 * time.Second).Should(BeNumerically(">", 0))

			events := collector.EventsForInstance(containerID)
			first := events[0].Data["status"].(string)
			Expect(first).To(SatisfyAny(Equal("PENDING"), Equal("RUNNING")),
				"first event should be either PENDING or RUNNING, got %q", first)
		})
	})

	Context("delete status event", Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
		})

		It("creates a container, waits for RUNNING, then deletes", func() {
			name := uniqueName("e2e-del")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			collector.WaitForStatus(containerID, "RUNNING", 60*time.Second)

			resp, err := doContainerSPRequest(http.MethodDelete, "/containers/"+containerID, "")
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

			By("waiting for a DELETED or TERMINATED status event")
			Eventually(func() bool {
				for _, e := range collector.EventsForInstance(containerID) {
					s, _ := e.Data["status"].(string)
					if s == "DELETED" || s == "TERMINATED" || s == "STOPPED" {
						return true
					}
				}
				return false
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
				"expected a terminal status event after deletion")
		})
	})

	// Non-DCM Deployments should not trigger status events.
	Context("label filtering", Label("cluster"), Ordered, func() {
		var collector *NATSCollector
		manualDeployName := uniqueName("e2e-unlbl")

		BeforeAll(func() {
			requireKubectl()
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			_, _ = runKubectl("delete", "deployment", manualDeployName, "--ignore-not-found")
		})

		It("does not emit events for non-DCM Deployments", func() {
			By("creating a Deployment without DCM labels via kubectl")
			_, err := runKubectl("create", "deployment", manualDeployName, "--image=docker.io/library/nginx:alpine")
			Expect(err).NotTo(HaveOccurred())

			By("waiting and confirming no events appear for the manual Deployment")
			Consistently(func() int {
				return len(collector.EventsForInstance(manualDeployName))
			}).WithTimeout(15 * time.Second).WithPolling(2 * time.Second).Should(Equal(0),
				"no NATS events should be emitted for non-DCM Deployment %s", manualDeployName)
		})
	})

	// Scaling a DCM-managed Deployment to zero should produce a status change.
	Context("scaled to zero", Label("cluster"), Ordered, func() {
		var collector *NATSCollector
		var containerID string

		BeforeAll(func() {
			requireKubectl()
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		It("reflects status change when Deployment is scaled to zero", func() {
			name := uniqueName("e2e-scale")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			By("waiting for RUNNING status")
			collector.WaitForStatus(containerID, "RUNNING", 60*time.Second)

			By("resolving the actual Deployment name (SP uses GenerateName)")
			deployName := findDeploymentName(containerID)

			By("scaling the Deployment to zero via kubectl")
			_, err := runKubectl("scale", "deployment", deployName, "--replicas=0")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for a non-RUNNING status event")
			Eventually(func() bool {
				events := collector.EventsForInstance(containerID)
				var sawRunning bool
				for _, e := range events {
					s, _ := e.Data["status"].(string)
					if s == "RUNNING" {
						sawRunning = true
					}
					if sawRunning && s != "RUNNING" {
						return true
					}
				}
				return false
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
				"expected status to change after scaling to zero")
		})
	})

	// Verify the SP retries NATS publishes after a transient NATS outage.
	Context("NATS resilience", Label("disruptive"), Ordered, func() {
		var containerID string

		BeforeAll(func() {
			requireContainerSP()
			requirePodman()
		})

		AfterAll(func() {
			if containerID != "" {
				deleteTestContainer(containerID)
			}
		})

		It("delivers status events after NATS restart", func() {
			natsContainer := findComposeContainer("nats")

			By("subscribing before create, then confirming status RUNNING")
			collector1 := newNATSCollector(natsStatusSubject)
			name := uniqueName("e2e-nats")
			body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
			containerID = body["id"].(string)

			collector1.WaitForStatus(containerID, "RUNNING", 60*time.Second)
			collector1.Close()

			By("restarting the NATS container")
			out, err := exec.Command(podmanBin, "restart", "-t", "5", natsContainer).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "failed to restart NATS: %s", string(out))

			By("verifying the NATS container is running after restart")
			Eventually(func() string {
				state, _ := exec.Command(podmanBin, "inspect", "--format", "{{.State.Status}}", natsContainer).CombinedOutput()
				return strings.TrimSpace(string(state))
			}).WithTimeout(15 * time.Second).WithPolling(1 * time.Second).Should(Equal("running"),
				"NATS container should be in running state after restart")

			By("waiting for NATS to be connectable again")
			Eventually(func() error {
				conn, connErr := nats.Connect(natsURL, nats.Timeout(2*time.Second))
				if connErr != nil {
					return connErr
				}
				conn.Close()
				return nil
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			By("subscribing and triggering a new status change")
			collector2 := newNATSCollector(natsStatusSubject)
			defer collector2.Close()

			deleteTestContainer(containerID)

			Eventually(func() bool {
				for _, e := range collector2.EventsForInstance(containerID) {
					s, _ := e.Data["status"].(string)
					if s == "DELETED" || s == "TERMINATED" || s == "STOPPED" {
						return true
					}
				}
				return false
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
				"expected terminal status event after NATS recovery")
			containerID = ""
		})
	})

	Context("concurrent containers", Ordered, func() {
		var collector *NATSCollector
		var containerIDs []string

		BeforeAll(func() {
			collector = newNATSCollector(natsStatusSubject)
		})

		AfterAll(func() {
			collector.Close()
			for _, id := range containerIDs {
				deleteTestContainer(id)
			}
		})

		It("emits independent event streams for each container", func() {
			const count = 3
			for i := 0; i < count; i++ {
				name := uniqueName(fmt.Sprintf("e2e-con%d", i))
				body := createTestContainer(containerSpec(name, "docker.io/library/nginx:alpine", false))
				containerIDs = append(containerIDs, body["id"].(string))
			}

			for _, id := range containerIDs {
				Eventually(func() int {
					return len(collector.EventsForInstance(id))
				}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(BeNumerically(">", 0),
					"expected at least one event for container %s", id)
			}

			By("verifying events are isolated per instance")
			for _, id := range containerIDs {
				events := collector.EventsForInstance(id)
				for _, e := range events {
					Expect(e.Data["id"]).To(Equal(id),
						"event for container %s contains wrong instance ID", id)
				}
			}
		})
	})
})
