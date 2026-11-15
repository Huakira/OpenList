package icloud_photos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// useFakeBridge points the process manager at an httptest server.
func useFakeBridge(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	prevHook, prevBridges := startBridgeHook, bridges
	startBridgeHook = func(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error) {
		return &bridgeProc{baseURL: srv.URL, token: "tok", appleID: d.AppleID}, nil
	}
	bridges = map[string]*bridgeProc{}
	t.Cleanup(func() {
		startBridgeHook, bridges = prevHook, prevBridges
		srv.Close()
	})
	return srv.URL
}

// The sign-in flow spans two saves: the first one makes Apple push a code, the
// second one submits it. The half-finished state lives in the bridge process,
// which is why the process must survive between the two Init calls.
func TestLoginAsksForTheCodeThenWorks(t *testing.T) {
	var pending, loggedIn atomic.Bool
	var seen []string
	useFakeBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			body := map[string]any{"ok": loggedIn.Load(), "pending_login": ""}
			if pending.Load() {
				body["pending_login"] = "a@b.c"
			}
			json.NewEncoder(w).Encode(body)
		case "/v1/login":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			seen = append(seen, b["password"]+"|"+b["code"])
			if b["code"] == "" {
				pending.Store(true) // Apple sent the code
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "needs_2fa": true})
				return
			}
			if b["code"] != "123456" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "bad code"})
				return
			}
			pending.Store(false)
			loggedIn.Store(true)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))

	d := &ICloudPhotos{Addition: Addition{AppleID: "a@b.c", Password: "pw", DataDir: t.TempDir()}}
	err := d.Init(context.Background())
	if err == nil || !strings.Contains(err.Error(), "验证码") {
		t.Fatalf("first save must ask for the code, got %v", err)
	}

	d.TwoFactorCode = "123456"
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("second save with the code: %v", err)
	}
	if len(seen) != 2 || seen[0] != "pw|" || seen[1] != "|123456" {
		t.Fatalf("unexpected login calls: %v", seen)
	}
}

func TestLoginWithoutPasswordSaysWhatToDo(t *testing.T) {
	useFakeBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "PyiCloudBackend: no session"})
	}))
	d := &ICloudPhotos{Addition: Addition{AppleID: "a@b.c", DataDir: t.TempDir()}}
	err := d.Init(context.Background())
	if err == nil || !strings.Contains(err.Error(), "密码") {
		t.Fatalf("expected a hint about the password, got %v", err)
	}
}

func TestExtractBridge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bridge")
	if err := extractBridge(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icloud_bridge", "__init__.py")); err != nil {
		t.Fatalf("embedded bridge not extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icloud_bridge", "__main__.py")); err != nil {
		t.Fatalf("__main__.py missing, python3 -m icloud_bridge cannot start: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, ".version"))
	if err != nil || len(strings.TrimSpace(string(marker))) != 16 {
		t.Fatalf("version marker: %v %q", err, marker)
	}
	// Second run is a no-op, and a stale marker forces a rewrite.
	if err := extractBridge(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".version"), []byte("stale\n"), 0o644)
	if err := extractBridge(dir); err != nil {
		t.Fatal(err)
	}
}

// Region, proxy and smart albums reach the bridge as environment variables, so
// flipping one has to restart the process; the 2FA step must not.
func TestOnlyEnvironmentChangesRestartTheBridge(t *testing.T) {
	var starts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	prevHook, prevBridges := startBridgeHook, bridges
	startBridgeHook = func(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error) {
		atomic.AddInt32(&starts, 1)
		return &bridgeProc{baseURL: srv.URL, token: "tok", appleID: d.AppleID}, nil
	}
	bridges = map[string]*bridgeProc{}
	t.Cleanup(func() { startBridgeHook, bridges = prevHook, prevBridges })

	d := &ICloudPhotos{Addition: Addition{AppleID: "a@b.c", DataDir: t.TempDir()}}
	mustInit := func() {
		t.Helper()
		if err := d.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mustInit()
	mustInit() // same config: reused
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("same config restarted the bridge %d times", n)
	}
	d.TwoFactorCode = "123456" // the 2FA step must keep the process
	mustInit()
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("the 2FA step restarted the bridge (%d starts)", n)
	}
	d.China = true // environment change
	mustInit()
	if n := atomic.LoadInt32(&starts); n != 2 {
		t.Fatalf("changing the region did not restart the bridge (%d starts)", n)
	}
	d.EgressProxy = "socks5h://127.0.0.1:1080"
	mustInit()
	if n := atomic.LoadInt32(&starts); n != 3 {
		t.Fatalf("changing the proxy did not restart the bridge (%d starts)", n)
	}
}
