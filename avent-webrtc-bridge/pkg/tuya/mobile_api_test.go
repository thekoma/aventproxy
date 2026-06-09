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
