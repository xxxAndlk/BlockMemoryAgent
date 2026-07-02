//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSessionSubresources(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	createBody, _ := json.Marshal(map[string]string{"goal": "test subresources"})
	resp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	defer resp.Body.Close()

	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sessionID := sess["id"].(string)

	endpoints := []struct {
		path   string
		method string
		body   string
	}{
		{"/board", "GET", ""},
		{"/agents", "GET", ""},
		{"/metrics", "GET", ""},
		{"/logs", "GET", ""},
		{"/token-metrics", "GET", ""},
		{"/watchdog", "GET", ""},
		{"/mailbox", "GET", ""},
	}

	for _, ep := range endpoints {
		var resp *http.Response
		var err error
		url := f.Server.URL() + "/api/sessions/" + sessionID + ep.path
		if ep.method == "POST" {
			resp, err = http.Post(url, "application/json", bytes.NewReader([]byte(ep.body)))
		} else {
			resp, err = http.Get(url)
		}
		if err != nil {
			t.Errorf("%s %s: %v", ep.method, ep.path, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= http.StatusInternalServerError {
			t.Errorf("%s %s returned server error: %d", ep.method, ep.path, resp.StatusCode)
		}
	}
}
