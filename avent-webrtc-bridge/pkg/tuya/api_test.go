package tuya

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refused token request used to return no token and no error, and
// PasswordLogin then dereferenced the missing token (issue #162).
func TestGetLoginTokenRefusedIsAnError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"errorMsg":"account does not exist"}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")

	resp, err := GetLoginToken(srv.Client(), host, "viewer@example.com", "39")
	if err == nil || resp != nil {
		t.Fatalf("GetLoginToken = %v, %v; want no token and an error", resp, err)
	}
	if !strings.Contains(err.Error(), "account does not exist") {
		t.Errorf("error %q does not carry the API's message", err)
	}

	if _, err := PasswordLogin(srv.Client(), host, "viewer@example.com", "secret", "39"); err == nil {
		t.Fatal("PasswordLogin succeeded with a refused token request")
	}
}
