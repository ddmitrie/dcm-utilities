//go:build e2e

package e2e_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DCM API authentication", Label("smoke", "auth", "p0"), func() {
	It("rejects an unauthenticated catalog request", func() {
		if !authEnabled {
			Skip("authentication is disabled")
		}
		resp, err := doUnauthenticatedRequest(http.MethodGet, "/catalog-items", "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("allows an authenticated catalog request", func() {
		if !authEnabled {
			Skip("authentication is disabled")
		}
		resp, err := doRequest(http.MethodGet, "/catalog-items", "")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})
