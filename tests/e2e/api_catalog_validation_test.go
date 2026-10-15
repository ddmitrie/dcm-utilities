//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const duplicateFieldPathMessage = "duplicate field path in resource"

func catalogItemPayload(displayName string, resources interface{}) string {
	data, err := json.Marshal(map[string]interface{}{
		"api_version":  "v1alpha1",
		"display_name": displayName,
		"spec":         map[string]interface{}{"resources": resources},
	})
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func catalogResource(name, serviceType string, fields []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"name":         name,
		"service_type": serviceType,
		"fields":       fields,
	}
}

func field(path, displayName, defaultValue string) map[string]interface{} {
	return map[string]interface{}{
		"path":         path,
		"display_name": displayName,
		"editable":     true,
		"default":      defaultValue,
	}
}

func responseBodyString(resp *http.Response) string {
	data := readBody(resp)
	return string(data)
}

var _ = Describe("Catalog field-path validation", Label("catalog", "validation", "p0"), Ordered, func() {
	It("rejects duplicate field paths during catalog-item creation [FLPATH-4931]", func() {
		name := uniqueName("e2e-duplicate-create")
		resources := []map[string]interface{}{
			catalogResource("main", "container", []map[string]interface{}{
				field("metadata.name", "first", "one"),
				field("metadata.name", "second", "two"),
			}),
		}

		resp, err := doRequest(http.MethodPost, "/catalog-items", catalogItemPayload(name, resources))
		Expect(err).NotTo(HaveOccurred())
		body := responseBodyString(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(strings.ToLower(body)).To(ContainSubstring(duplicateFieldPathMessage))
		Expect(strings.ToLower(body)).To(ContainSubstring("metadata.name"))
	})

	It("rejects duplicate field paths during catalog-item update and preserves the original [FLPATH-4931]", func() {
		name := uniqueName("e2e-duplicate-update")
		validResources := []map[string]interface{}{
			catalogResource("main", "container", []map[string]interface{}{
				field("metadata.name", "name", name),
				field("image.reference", "image", "docker.io/library/nginx:alpine"),
			}),
		}
		resp, err := doRequest(http.MethodPost, "/catalog-items", catalogItemPayload(name, validResources))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		var created map[string]interface{}
		decodeJSON(resp, &created)
		uid, _ := created["uid"].(string)
		Expect(uid).NotTo(BeEmpty())
		DeferCleanup(func() {
			cleanup, cleanupErr := doRequest(http.MethodDelete, "/catalog-items/"+uid, "")
			if cleanupErr == nil && cleanup != nil {
				cleanup.Body.Close()
			}
		})

		invalid := map[string]interface{}{
			"spec": map[string]interface{}{"resources": []map[string]interface{}{
				catalogResource("main", "container", []map[string]interface{}{
					field("metadata.name", "first", "one"),
					field("metadata.name", "second", "two"),
				}),
			}},
		}
		patch, err := json.Marshal(invalid)
		Expect(err).NotTo(HaveOccurred())
		request, err := http.NewRequest(http.MethodPatch, gatewayBaseURL+"/catalog-items/"+uid, strings.NewReader(string(patch)))
		Expect(err).NotTo(HaveOccurred())
		request.Header.Set("Content-Type", "application/merge-patch+json")
		update, err := httpClient.Do(request)
		Expect(err).NotTo(HaveOccurred())
		updateBody := responseBodyString(update)
		Expect(update.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(strings.ToLower(updateBody)).To(ContainSubstring(duplicateFieldPathMessage))

		read, err := doRequest(http.MethodGet, "/catalog-items/"+uid, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(read.StatusCode).To(Equal(http.StatusOK))
		var current map[string]interface{}
		decodeJSON(read, &current)
		Expect(fmt.Sprint(current["display_name"])).To(Equal(name))
	})

	It("allows the same field path in separate resources [FLPATH-4931]", func() {
		name := uniqueName("e2e-duplicate-cross-resource")
		resources := []map[string]interface{}{
			catalogResource("network", "network", []map[string]interface{}{field("metadata.name", "name", "network")}),
			catalogResource("app", "container", []map[string]interface{}{field("metadata.name", "name", "app")}),
		}
		resp, err := doRequest(http.MethodPost, "/catalog-items", catalogItemPayload(name, resources))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		var created map[string]interface{}
		decodeJSON(resp, &created)
		uid, _ := created["uid"].(string)
		Expect(uid).NotTo(BeEmpty())
		DeferCleanup(func() {
			cleanup, cleanupErr := doRequest(http.MethodDelete, "/catalog-items/"+uid, "")
			if cleanupErr == nil && cleanup != nil {
				cleanup.Body.Close()
			}
		})
	})
})
