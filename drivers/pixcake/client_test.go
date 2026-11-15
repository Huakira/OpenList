package pixcake

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestRejectsInvalidSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		status int
		body string
	}{
		{"http failure", 500, `{"code":0,"data":{}}`},
		{"missing code", 200, `{"data":{}}`},
		{"null code", 200, `{"code":null}`},
		{"invalid JSON", 200, `<html>error</html>`},
		{"API error", 200, `{"code":1,"error":"denied"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "test-token" { t.Error("expected raw token") }
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			d := &PixCake{Addition: Addition{APIBase:srv.URL}, token:"test-token", http:srv.Client()}
			if err := d.request(context.Background(), http.MethodGet, "/test", nil, nil, nil); err == nil { t.Fatal("accepted invalid success") }
		})
	}
}

func TestRequestPreservesIntegerPrecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"id":9007199254740993}}`)
	}))
	defer srv.Close()
	d := &PixCake{Addition: Addition{APIBase:srv.URL}, http:srv.Client()}
	var got struct { ID int64 `json:"id"` }
	if err := d.request(context.Background(), http.MethodGet, "/test", nil, nil, &got); err != nil { t.Fatal(err) }
	if got.ID != 9007199254740993 { t.Fatalf("lost integer precision: %d", got.ID) }
}

func TestUploadTokenRefreshesBeforeExpiry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"code":0,"data":{"access_key_id":"id","access_key_secret":"secret","security_token":"token","bucket":"bucket","default_prefix_dir":"user/upload","region":"https://oss-cn-hangzhou","expiration":%q}}`,time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	d := &PixCake{Addition: Addition{APIBase:srv.URL}, http:srv.Client()}
	first, err := d.uploadToken(context.Background())
	if err != nil { t.Fatal(err) }
	if first.Prefix != "user/upload" { t.Fatal("did not decode default_prefix_dir") }
	if _, err = d.uploadToken(context.Background()); err != nil { t.Fatal(err) }
	if calls != 1 { t.Fatalf("cache missed: %d",calls) }
	d.sts.Expiration = time.Now().Add(30*time.Second).UTC().Format(time.RFC3339)
	if _, err = d.uploadToken(context.Background()); err != nil { t.Fatal(err) }
	if calls != 2 { t.Fatalf("did not refresh near expiry: %d",calls) }
}

func TestUploadTokenRejectsIncompleteCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"access_key_id":"id","security_token":"token"}}`)
	}))
	defer srv.Close()
	d := &PixCake{Addition: Addition{APIBase:srv.URL}, http:srv.Client()}
	if _, err := d.uploadToken(context.Background()); err == nil || !strings.Contains(err.Error(),"不完整") { t.Fatalf("expected incomplete STS error, got %v",err) }
}
