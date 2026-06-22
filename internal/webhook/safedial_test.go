package webhook

import "testing"

// ValidateURL must reject internal/loopback/link-local targets and non-http(s)
// schemes — the creation-time half of the SSRF guard (the dialer is the other half).
func TestValidateURLRejectsInternalAndBadSchemes(t *testing.T) {
	bad := []string{
		"http://localhost/hook",
		"http://127.0.0.1/hook",
		"https://127.0.0.1/hook",
		"http://10.1.2.3/hook",
		"http://192.168.0.1/hook",
		"http://172.16.5.4/hook",
		"http://169.254.169.254/latest/meta-data", // cloud metadata service
		"http://[::1]/hook",
		"https://[fe80::1]/hook",
		"ftp://example.com/hook",
		"://nope",
		"https:///nohost",
	}
	for _, u := range bad {
		if err := ValidateURL(u, false); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", u)
		}
	}
}

// Public hosts (names and routable IPs) must pass; http is allowed only when https
// is not required.
func TestValidateURLAllowsPublic(t *testing.T) {
	ok := []string{
		"https://example.com/hook",
		"https://hooks.example.com:8443/path",
		"http://example.com/hook",
		"http://8.8.8.8/hook",
	}
	for _, u := range ok {
		if err := ValidateURL(u, false); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateURLRequireHTTPS(t *testing.T) {
	if err := ValidateURL("http://example.com/hook", true); err == nil {
		t.Error("http must be rejected when https is required")
	}
	if err := ValidateURL("https://example.com/hook", true); err != nil {
		t.Errorf("https should pass when https is required: %v", err)
	}
}
