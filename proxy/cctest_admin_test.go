package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-proxy/config"
)

func TestCCTestAdminSwitchRoundTrip(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	w := httptest.NewRecorder()
	h.apiCreateApiKey(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"key":"shared-admin-fixture","cctestTextHandlingEnabled":true}`)))
	if w.Code != 200 {
		t.Fatalf("create status=%d", w.Code)
	}
	entry := config.FindApiKeyByValue("shared-admin-fixture")
	if entry == nil || !entry.CCTestTextHandling() {
		t.Fatal("admin create did not save the probe switch")
	}
	for _, tc := range []struct {
		patch   string
		enabled bool
	}{
		{`{"name":"renamed"}`, true},
		{`{"cctestTextHandlingEnabled":false}`, false},
		{`{"cctestTextHandlingEnabled":true}`, true},
	} {
		w = httptest.NewRecorder()
		h.apiUpdateApiKey(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.patch)), entry.ID)
		if w.Code != 200 {
			t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
		}
		if config.GetApiKeyEntry(entry.ID).CCTestTextHandling() != tc.enabled {
			t.Fatal("admin update did not retain switch")
		}
		var response struct {
			APIKey struct {
				Enabled bool `json:"cctestTextHandlingEnabled"`
			} `json:"apiKey"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.APIKey.Enabled != tc.enabled {
			t.Fatal("switch missing from admin view")
		}
	}
}
