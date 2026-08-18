package cloudflare

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/livresolucoes/dockflare/internal/logger"
)

func encodeToken(t *testing.T, payload string, enc *base64.Encoding) string {
	t.Helper()
	return enc.EncodeToString([]byte(payload))
}

func TestParseConnectorToken(t *testing.T) {
	payload := `{"a":"acct123","t":"tunnel456","s":"supersecret"}`

	encodings := map[string]*base64.Encoding{
		"std":    base64.StdEncoding,
		"rawStd": base64.RawStdEncoding,
		"url":    base64.URLEncoding,
		"rawURL": base64.RawURLEncoding,
	}
	for name, enc := range encodings {
		t.Run(name, func(t *testing.T) {
			tok, err := ParseConnectorToken(encodeToken(t, payload, enc))
			if err != nil {
				t.Fatal(err)
			}
			if tok.AccountTag != "acct123" || tok.TunnelID != "tunnel456" {
				t.Errorf("got %+v", tok)
			}
		})
	}
}

func TestParseConnectorToken_TrimsWhitespace(t *testing.T) {
	raw := "  " + encodeToken(t, `{"a":"a","t":"b","s":"c"}`, base64.StdEncoding) + "\n"
	if _, err := ParseConnectorToken(raw); err != nil {
		t.Fatal(err)
	}
}

func TestParseConnectorToken_Errors(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"empty", "", "empty"},
		{"not base64", "!!!not base64!!!", "base64"},
		{"not json", base64.StdEncoding.EncodeToString([]byte("hello")), "JSON"},
		{
			"missing tunnel id",
			base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","s":"secret"}`)),
			"missing the account tag or tunnel ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnectorToken(tc.raw)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseConnectorToken_ErrorNeverEchoesTheToken(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct"}`))
	_, err := ParseConnectorToken(raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "acct") {
		t.Errorf("error leaks token material: %v", err)
	}
}

func TestConnectorToken_StringRedactsSecret(t *testing.T) {
	tok := ConnectorToken{AccountTag: "acct", TunnelID: "tun", Secret: "supersecret"}
	s := tok.String()
	if strings.Contains(s, "supersecret") {
		t.Errorf("String() leaks the secret: %s", s)
	}
	if !strings.Contains(s, "[redacted]") {
		t.Errorf("String() = %s, want the secret redacted", s)
	}
}

// stubAPI spins up a Cloudflare-shaped test server.
func stubAPI(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("test-api-token", logger.New(io.Discard))
	c.SetBaseURL(srv.URL)
	return c, srv
}

func writeResult(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"success": true, "errors": []any{}, "result": result}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

func TestGetTunnelConfiguration(t *testing.T) {
	var gotPath, gotAuth string
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		writeResult(t, w, map[string]any{
			"tunnel_id": "tunnel456",
			"version":   3,
			"config": map[string]any{
				"ingress": []map[string]any{
					{"hostname": "api.example.com", "service": "http://api:3000"},
					{"service": "http_status:404"},
				},
			},
		})
	})

	cfg, err := c.GetTunnelConfiguration(context.Background(), "acct123", "tunnel456")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/accounts/acct123/cfd_tunnel/tunnel456/configurations" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-api-token" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if len(cfg.Ingress) != 2 || cfg.Ingress[0].Service != "http://api:3000" {
		t.Errorf("ingress = %+v", cfg.Ingress)
	}
}

func TestGetTunnelConfiguration_NullConfig(t *testing.T) {
	// A tunnel that has never been configured answers with "config": null.
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeResult(t, w, map[string]any{"tunnel_id": "t", "version": 0, "config": nil})
	})
	cfg, err := c.GetTunnelConfiguration(context.Background(), "a", "t")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.Ingress) != 0 {
		t.Errorf("want an empty config, got %+v", cfg)
	}
}

func TestPutTunnelConfiguration_WrapsBodyInConfigKey(t *testing.T) {
	var body map[string]json.RawMessage
	var method string
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		writeResult(t, w, map[string]any{})
	})

	err := c.PutTunnelConfiguration(context.Background(), "acct", "tun", &TunnelConfig{
		Ingress: []IngressRule{
			{Hostname: "api.example.com", Service: "http://api:3000"},
			{Service: "http_status:404"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	raw, ok := body["config"]
	if !ok {
		t.Fatalf("payload has no \"config\" key: %v", body)
	}
	if !strings.Contains(string(raw), `"hostname":"api.example.com"`) {
		t.Errorf("config = %s", raw)
	}
	// The catch-all must serialize without a hostname key.
	if strings.Count(string(raw), `"hostname"`) != 1 {
		t.Errorf("catch-all should have no hostname; got %s", raw)
	}
}

func TestDo_APIErrorIsSurfaced(t *testing.T) {
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
		})
	})
	_, err := c.GetTunnelConfiguration(context.Background(), "a", "t")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"403", "10000", "Authentication error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

func TestDo_ErrorNeverContainsTheAPIToken(t *testing.T) {
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("not json at all"))
	})
	_, err := c.GetTunnelConfiguration(context.Background(), "a", "t")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "test-api-token") {
		t.Errorf("error leaks the API token: %v", err)
	}
}

func TestDo_NoTokenConfigured(t *testing.T) {
	c := NewClient("", logger.New(io.Discard))
	_, err := c.GetTunnelConfiguration(context.Background(), "a", "t")
	if err == nil || !strings.Contains(err.Error(), "no API token") {
		t.Fatalf("want a clear no-token error, got: %v", err)
	}
}

func TestEnsureTunnelCNAME_CreatesMissingRecord(t *testing.T) {
	var created DNSRecord
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{{ID: "z1", Name: "example.com"}})
		case r.URL.Path == "/zones/z1/dns_records" && r.Method == http.MethodGet:
			writeResult(t, w, []DNSRecord{})
		case r.URL.Path == "/zones/z1/dns_records" && r.Method == http.MethodPost:
			json.NewDecoder(r.Body).Decode(&created)
			writeResult(t, w, created)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})

	written, err := c.EnsureTunnelCNAME(context.Background(), "api.example.com", "tunnel456")
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Error("want written=true for a new record")
	}
	if created.Type != "CNAME" || created.Content != "tunnel456.cfargotunnel.com" || !created.Proxied {
		t.Errorf("created = %+v", created)
	}
}

func TestEnsureTunnelCNAME_LeavesCorrectRecordAlone(t *testing.T) {
	writes := 0
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{{ID: "z1", Name: "example.com"}})
		case r.Method == http.MethodGet:
			writeResult(t, w, []DNSRecord{{
				ID: "r1", Type: "CNAME", Name: "api.example.com",
				Content: "tunnel456.cfargotunnel.com", Proxied: true,
			}})
		default:
			writes++
			writeResult(t, w, map[string]any{})
		}
	})

	written, err := c.EnsureTunnelCNAME(context.Background(), "api.example.com", "tunnel456")
	if err != nil {
		t.Fatal(err)
	}
	if written || writes != 0 {
		t.Errorf("written=%v writes=%d, want no write for an already-correct record", written, writes)
	}
}

func TestEnsureTunnelCNAME_RefusesToClobberUnrelatedRecord(t *testing.T) {
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{{ID: "z1", Name: "example.com"}})
		case r.Method == http.MethodGet:
			writeResult(t, w, []DNSRecord{{
				ID: "r1", Type: "A", Name: "api.example.com", Content: "203.0.113.10",
			}})
		default:
			t.Errorf("must not write; got %s %s", r.Method, r.URL)
			writeResult(t, w, map[string]any{})
		}
	})

	_, err := c.EnsureTunnelCNAME(context.Background(), "api.example.com", "tunnel456")
	if err == nil {
		t.Fatal("expected a refusal for a pre-existing unrelated record")
	}
	if !strings.Contains(err.Error(), "leaving it untouched") {
		t.Errorf("error = %v", err)
	}
}

func TestEnsureTunnelCNAME_PicksLongestMatchingZone(t *testing.T) {
	var recordedZone string
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{
				{ID: "parent", Name: "example.com"},
				{ID: "child", Name: "eu.example.com"},
			})
		case r.Method == http.MethodGet:
			recordedZone = strings.Split(r.URL.Path, "/")[2]
			writeResult(t, w, []DNSRecord{})
		default:
			writeResult(t, w, map[string]any{})
		}
	})

	if _, err := c.EnsureTunnelCNAME(context.Background(), "api.eu.example.com", "tun"); err != nil {
		t.Fatal(err)
	}
	if recordedZone != "child" {
		t.Errorf("zone = %q, want the more specific eu.example.com zone", recordedZone)
	}
}

func TestEnsureTunnelCNAME_NoMatchingZone(t *testing.T) {
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeResult(t, w, []Zone{{ID: "z1", Name: "other.com"}})
	})
	_, err := c.EnsureTunnelCNAME(context.Background(), "api.example.com", "tun")
	if err == nil || !strings.Contains(err.Error(), "no Cloudflare zone") {
		t.Fatalf("want a clear no-zone error, got: %v", err)
	}
}
