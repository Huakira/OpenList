package icloud_photos

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// fakeBridge mimics icloud-photos-bridge and records what the driver sent.
func fakeBridge(t *testing.T, seen *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		*seen = append(*seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"ok":true}`))
		case "/v1/list":
			w.Write([]byte(`{"items":[
			 {"id":"all","name":"全部照片","is_dir":true},
			 {"id":"smart:Favorites","name":"收藏","is_dir":true},
			 {"id":"f|all|A1","name":"IMG_1.HEIC","size":7,"modified":"2001-01-01T00:00:00+00:00","thumb":"https://t/x"}]}`))
		case "/v1/link":
			if strings.Contains(r.URL.RawQuery, "pending") {
				w.Write([]byte(`{"local":true}`))
			} else {
				w.Write([]byte(`{"local":false,"url":"https://cvws.icloud-content.com/s"}`))
			}
		case "/v1/raw":
			if strings.Contains(r.URL.RawQuery, "version=thumb") {
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write([]byte("thumbbytes"))
				return
			}
			if r.Header.Get("Range") != "bytes=2-4" {
				t.Errorf("range not forwarded: %q", r.Header.Get("Range"))
			}
			w.Write([]byte("cde"))
		case "/v1/remove":
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"whitelist","kind":"Forbidden"}`))
		case "/v1/mkdir":
			w.Write([]byte(`{"id":"album:L9","name":"__bridge_test_a","is_dir":true}`))
		case "/v1/upload":
			w.Write([]byte(`{"id":"f|album:L9|pending-1","name":"x.jpg","size":3,"status":"processing"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
}

func newDriver(t *testing.T, url string, smart bool) *ICloudPhotos {
	// The driver signs its thumbnail urls from the global settings; seed the
	// cache so the test does not need an initialised database.
	op.Cache.SetSetting(conf.LinkExpiration, &model.SettingItem{Key: conf.LinkExpiration, Value: "0"})
	op.Cache.SetSetting(conf.Token, &model.SettingItem{Key: conf.Token, Value: "test-token"})
	// Point the driver at the fake bridge instead of spawning Python.
	prevHook, prevBridges := startBridgeHook, bridges
	startBridgeHook = func(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error) {
		return &bridgeProc{baseURL: url, token: "tok", appleID: d.AppleID}, nil
	}
	bridges = map[string]*bridgeProc{}
	t.Cleanup(func() { startBridgeHook, bridges = prevHook, prevBridges })
	d := &ICloudPhotos{Addition: Addition{AppleID: "test@example.com", ShowSmartAlbums: smart,
		DataDir: t.TempDir()}}
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestListAndLink(t *testing.T) {
	var seen []string
	srv := fakeBridge(t, &seen)
	defer srv.Close()
	d := newDriver(t, srv.URL, false)
	ctx := context.Background()

	objs, err := d.List(ctx, &model.Object{ID: "root"}, model.ListArgs{ReqPath: "/photos/album"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 { // smart album filtered out
		t.Fatalf("got %d objs", len(objs))
	}
	f := objs[1]
	if f.GetName() != "IMG_1.HEIC" || f.GetSize() != 7 || f.IsDir() || f.ModTime().Year() != 2001 {
		t.Fatalf("bad obj %+v", f)
	}
	th, ok := f.(model.Thumb)
	if !ok {
		t.Fatal("file must expose a thumbnail")
	}
	if got := th.Thumb(); !strings.HasPrefix(got, "/d/photos/album/IMG_1.HEIC?") ||
		!strings.Contains(got, "type=thumb") || !strings.Contains(got, "sign=") {
		t.Fatalf("thumbnail must be a local /d url, got %q", got)
	}
	if objs[0].(model.Thumb).Thumb() != "" {
		t.Fatal("a folder has no thumbnail")
	}

	l, err := d.Link(ctx, f, model.LinkArgs{})
	if err != nil || l.URL != "" || l.RangeReader == nil {
		t.Fatalf("link must be proxied through the bridge, got %+v %v", l, err)
	}
	rc, err := l.RangeReader.RangeRead(ctx, http_range.Range{Start: 2, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "cde" {
		t.Fatalf("raw %q", b)
	}

	tl, err := d.Link(ctx, f, model.LinkArgs{Type: "thumb"})
	if err != nil {
		t.Fatal(err)
	}
	if tl.ContentLength != int64(len("thumbbytes")) || tl.RangeReader == nil {
		t.Fatalf("bad thumb link %+v", tl)
	}
	tr, err := tl.RangeReader.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := io.ReadAll(tr)
	tr.Close()
	if string(tb) != "thumbbytes" {
		t.Fatalf("thumb %q", tb)
	}
}

func TestWritesForwardAndErrorsMap(t *testing.T) {
	var seen []string
	srv := fakeBridge(t, &seen)
	defer srv.Close()
	d := newDriver(t, srv.URL, true)
	ctx := context.Background()

	err := d.Remove(ctx, &model.Object{ID: "f|all|A1"})
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	dir, err := d.MakeDir(ctx, &model.Object{ID: "root"}, "__bridge_test_a")
	if err != nil || dir.GetID() != "album:L9" || !dir.IsDir() {
		t.Fatalf("mkdir %v %v", dir, err)
	}
	if err := d.Move(ctx, &model.Object{ID: "f|album:L1|A1"}, dir); err != nil {
		t.Fatal(err)
	}
	last := seen[len(seen)-1]
	var body map[string]string
	_ = json.Unmarshal([]byte(last[strings.Index(last, "{"):]), &body)
	if body["id"] != "f|album:L1|A1" || body["to"] != "album:L9" {
		t.Fatalf("move body %v", body)
	}
	if _, err := d.Other(ctx, model.OtherArgs{Method: "rm -rf", Obj: dir}); !errors.Is(err, errs.NotSupport) {
		t.Fatalf("unknown other op must be refused, got %v", err)
	}
}
