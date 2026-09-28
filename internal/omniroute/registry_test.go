package omniroute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRegistryProxyFromURL(t *testing.T) {
	u, _ := url.Parse("socks5h://alice:s3cret@10.0.0.1:1080")
	p, err := RegistryProxyFromURL(u, " United Kingdom ")
	if err != nil {
		t.Fatal(err)
	}
	want := RegistryProxy{Name: "ws-10.0.0.1", Type: "socks5", Host: "10.0.0.1", Port: 1080, Username: "alice", Password: "s3cret", Region: "United Kingdom"}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}

	noPort, _ := url.Parse("http://proxy.example.com")
	if _, err := RegistryProxyFromURL(noPort, ""); err == nil {
		t.Error("a proxy without an explicit port must be rejected")
	}
}

func TestCreateProxy_PostsToRegistry(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody RegistryProxy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithToken("manage-key"))
	p := RegistryProxy{Name: "ws-1.2.3.4", Type: "http", Host: "1.2.3.4", Port: 8080, Username: "alice", Password: "s3cret"}
	if err := c.CreateProxy(context.Background(), p); err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if gotPath != "/api/v1/management/proxies" || gotAuth != "Bearer manage-key" || gotBody != p {
		t.Errorf("request: path=%q auth=%q body=%+v", gotPath, gotAuth, gotBody)
	}
}

// OmniRoute's error body may echo the request; the password must not reach the caller's message.
func TestCreateProxy_ErrorNeverEchoesPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body) // echo the payload back, password included
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithToken("manage-key"))
	err := c.CreateProxy(context.Background(), RegistryProxy{Name: "x", Type: "http", Host: "1.2.3.4", Port: 8080, Username: "alice", Password: "s3cretPW"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cretPW") {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should still carry the status: %v", err)
	}
}
