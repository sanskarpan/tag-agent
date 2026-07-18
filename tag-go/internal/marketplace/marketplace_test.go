package marketplace

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateFetchURL_Accepts(t *testing.T) {
	good := []string{
		"https://example.com/x",
		"http://example.com/profile.yaml",
		"https://raw.githubusercontent.com/user/repo/main/profile.yaml",
		"https://8.8.8.8/x", // public IP literal
	}
	for _, u := range good {
		if err := ValidateFetchURL(u); err != nil {
			t.Errorf("expected %q to be accepted, got error: %v", u, err)
		}
	}
}

func TestValidateFetchURL_Rejects(t *testing.T) {
	bad := []struct {
		name string
		url  string
	}{
		{"bad-scheme-file", "file:///etc/passwd"},
		{"bad-scheme-ftp", "ftp://example.com/x"},
		{"empty-host", "http:///path"},
		{"loopback-v4", "http://127.0.0.1/x"},
		{"loopback-127-8", "http://127.5.5.5/x"},
		{"loopback-v6", "http://[::1]/x"},
		{"link-local", "http://169.254.1.1/x"},
		{"link-local-v6", "http://[fe80::1]/x"},
		{"metadata", "http://169.254.169.254/latest/meta-data/"},
		{"private-10", "http://10.0.0.1/x"},
		{"private-172", "http://172.16.0.1/x"},
		{"private-192", "http://192.168.1.1/x"},
		{"unspecified", "http://0.0.0.0/x"},
	}
	for _, tc := range bad {
		if err := ValidateFetchURL(tc.url); err == nil {
			t.Errorf("[%s] expected %q to be rejected, but it passed", tc.name, tc.url)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	// sha256("") known value
	got := SHA256Hex([]byte(""))
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("SHA256Hex empty = %q, want %q", got, want)
	}
}

func TestFetchBlocksRedirectToLoopback(t *testing.T) {
	// A "public" server that 302-redirects to a loopback target must be refused
	// at the socket level — the pre-flight URL guard alone would miss this.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("SECRET INTERNAL DATA"))
	}))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := Fetch(redirector.URL, 5*time.Second)
	if err == nil {
		t.Fatal("Fetch must refuse a redirect to a loopback address")
	}
	if !strings.Contains(err.Error(), "non-public") && !strings.Contains(err.Error(), "refusing") {
		t.Errorf("expected an SSRF refusal, got: %v", err)
	}
}

func TestFetchDirectLoopbackBlockedAtSocket(t *testing.T) {
	// Even without the pre-flight guard, dialing a loopback httptest server
	// directly must be refused by the Control hook.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer srv.Close()
	if _, err := Fetch(srv.URL, 5*time.Second); err == nil {
		t.Error("Fetch must refuse a direct loopback connection")
	}
}

func TestPushJSON_Success(t *testing.T) {
	// Allow the loopback httptest server for this test only.
	allowLoopbackForTest = true
	defer func() { allowLoopbackForTest = false }()

	var gotBody []byte
	var gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	res, err := PushJSON(srv.URL, []byte(`{"name":"x"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("PushJSON: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if string(gotBody) != `{"name":"x"}` {
		t.Errorf("server got body %q", gotBody)
	}
	if res.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", res.StatusCode)
	}
	if !strings.Contains(string(res.Body), "ok") {
		t.Errorf("response body = %q", res.Body)
	}
}

func TestPushJSON_Non2xxReturnsError(t *testing.T) {
	allowLoopbackForTest = true
	defer func() { allowLoopbackForTest = false }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	res, err := PushJSON(srv.URL, []byte(`{}`), 5*time.Second)
	if err == nil {
		t.Fatal("expected error on non-2xx")
	}
	if res == nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("expected PushResult carrying 400, got %+v", res)
	}
	if !strings.Contains(string(res.Body), "nope") {
		t.Errorf("expected server message in body, got %q", res.Body)
	}
}

func TestPushJSON_BlocksLoopbackAtSocket(t *testing.T) {
	// With the guard active (default), pushing to a loopback server is refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, err := PushJSON(srv.URL, []byte(`{}`), 5*time.Second); err == nil {
		t.Error("PushJSON must refuse a direct loopback connection")
	}
}

func TestPushJSON_BlocksRedirectToLoopback(t *testing.T) {
	allowLoopbackForTest = true
	defer func() { allowLoopbackForTest = false }()
	// The redirect target is a private (non-loopback) address that stays blocked
	// even with the loopback exemption on — proving redirect re-validation.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/internal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	if _, err := PushJSON(redirector.URL, []byte(`{}`), 5*time.Second); err == nil {
		t.Error("PushJSON must refuse a redirect to a private address")
	}
}

func TestReservedRangesBlocked(t *testing.T) {
	for _, s := range []string{"100.64.0.1", "192.0.2.5", "240.0.0.1", "198.18.0.9"} {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked (reserved range)", s)
		}
	}
}
