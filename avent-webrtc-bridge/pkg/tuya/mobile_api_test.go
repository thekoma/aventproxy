package tuya

import (
	"strings"
	"testing"
)

func TestParseAPIResponseSurfacesErrorCode(t *testing.T) {
	body := []byte(`{"success":false,"errorMsg":"No access","errorCode":"NO_AUTH"}`)
	_, err := parseAPIResponse(body)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "No access") {
		t.Errorf("error should contain errorMsg: %v", err)
	}
	if !strings.Contains(err.Error(), "NO_AUTH") {
		t.Errorf("error should surface errorCode: %v", err)
	}
}

func TestParseAPIResponseWithoutErrorCode(t *testing.T) {
	body := []byte(`{"success":false,"errorMsg":"No access"}`)
	_, err := parseAPIResponse(body)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "API error: No access" {
		t.Errorf("unexpected error message: %q", got)
	}
}

func TestParseAPIResponseSuccess(t *testing.T) {
	body := []byte(`{"success":true,"result":{"motoId":"abc"}}`)
	raw, err := parseAPIResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "motoId") {
		t.Errorf("result not returned: %s", raw)
	}
}

func TestParseAPIResponseShortInvalidBody(t *testing.T) {
	// Must not panic on bodies shorter than the 200-byte snippet cap.
	_, err := parseAPIResponse([]byte(`not json`))
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

func TestNormalizeAPIHost(t *testing.T) {
	cases := map[string]string{
		"a1.tuyaus.com":                  "a1.tuyaus.com",
		"https://a1.tuyain.com/api.json": "a1.tuyain.com",
		"http://a1.tuyacn.com/":          "a1.tuyacn.com",
		"  a1.tuyaeu.com  ":              "a1.tuyaeu.com",
		"":                               DefaultAPIHost,
		"   ":                            DefaultAPIHost,
	}
	for in, want := range cases {
		if got := NormalizeAPIHost(in); got != want {
			t.Errorf("NormalizeAPIHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPIBaseURL(t *testing.T) {
	if got := APIBaseURL("a1.tuyaus.com"); got != "https://a1.tuyaus.com/api.json" {
		t.Errorf("APIBaseURL = %q", got)
	}
	// An empty host keeps the pre-routing behaviour: Central Europe.
	if got := APIBaseURL(""); got != "https://a1.tuyaeu.com/api.json" {
		t.Errorf("APIBaseURL(\"\") = %q, want the EU host", got)
	}
	// A value already in URL form must not be doubled up.
	if got := APIBaseURL("https://a1.tuyain.com/api.json"); got != "https://a1.tuyain.com/api.json" {
		t.Errorf("APIBaseURL(url) = %q", got)
	}
}

func TestNewMobileSDKClientDefaultsToEU(t *testing.T) {
	c := NewMobileSDKClient("sk", "sid", "ak", "dev", "ch")
	if c.BaseURL != "https://a1.tuyaeu.com/api.json" {
		t.Errorf("BaseURL = %q, want the EU host by default", c.BaseURL)
	}
}
