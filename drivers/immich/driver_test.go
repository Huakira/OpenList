package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

func fakeImmich(t *testing.T) *httptest.Server {
	t.Helper()
	albums := []albumDTO{{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", AlbumName: "Trip", AssetCount: 2, UpdatedAt: "2026-01-02T00:00:00Z"}}
	assets := []assetDTO{
		{ID: "11111111-1111-4111-8111-111111111111", OriginalFileName: "IMG_1.HEIC", FileModifiedAt: "2026-01-01T00:00:00Z", LivePhotoVideoId: "22222222-2222-4222-8222-222222222222", ExifInfo: &exifDTO{FileSizeInByte: 12}},
		{ID: "22222222-2222-4222-8222-222222222222", OriginalFileName: "IMG_1.MOV", FileModifiedAt: "2026-01-01T00:00:00Z", Type: "VIDEO", ExifInfo: &exifDTO{FileSizeInByte: 40}},
		{ID: "33333333-3333-4333-8333-333333333333", OriginalFileName: "shot.jpg", FileModifiedAt: "2026-01-03T00:00:00Z", ExifInfo: &exifDTO{FileSizeInByte: 4}},
	}
	uploaded := []string{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			json.NewEncoder(w).Encode(albums)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/albums/"):
			json.NewEncoder(w).Encode(albums[0])
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
			var body struct {
				AlbumIds []string `json:"albumIds"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			items := assets
			if len(body.AlbumIds) > 0 {
				items = assets[:2]
			}
			json.NewEncoder(w).Encode(map[string]any{
				"assets": map[string]any{"total": len(items), "count": len(items), "items": items, "nextPage": nil},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
				w.WriteHeader(400)
				return
			}
			f, hdr, err := r.FormFile("assetData")
			if err != nil {
				t.Errorf("assetData: %v", err)
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) != "JPEG" {
				t.Errorf("body %q", b)
			}
			if hdr.Filename == "" || r.FormValue("deviceId") == "" {
				t.Errorf("missing upload fields name=%q device=%q", hdr.Filename, r.FormValue("deviceId"))
			}
			uploaded = append(uploaded, hdr.Filename)
			json.NewEncoder(w).Encode(uploadResp{ID: "44444444-4444-4444-8444-444444444444", Status: "created"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/assets"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/albums":
			json.NewEncoder(w).Encode(albumDTO{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", AlbumName: "New"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/original"):
			body := []byte("HEICBYTES")
			if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
				// bytes=1-3 → EIC
				w.Header().Set("Content-Range", "bytes 1-3/9")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body[1:4])
				return
			}
			w.Header().Set("Content-Type", "image/heic")
			w.Write(body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func newTestDriver(t *testing.T, url string) *Immich {
	t.Helper()
	d := &Immich{Addition: Addition{
		Address:       url,
		ApiKey:        "k",
		ShowType:      "root_only_album",
		HideLiveVideo: true,
	}}
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestListAlbumsAndHideLiveVideo(t *testing.T) {
	srv := fakeImmich(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)
	root, err := d.List(context.Background(), &model.Object{ID: "root", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root[0].GetName() != "Trip" || !root[0].IsDir() {
		t.Fatalf("root=%v", names(root))
	}
	files, err := d.List(context.Background(), root[0], model.ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(files); len(got) != 1 || got[0] != "IMG_1.HEIC" {
		t.Fatalf("album files %v (live video should be hidden)", got)
	}
}

func TestPutIntoAlbumAndLink(t *testing.T) {
	srv := fakeImmich(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)
	album := &model.Object{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "Trip", IsFolder: true}
	fs := &stream.FileStream{
		Obj:      &model.Object{Name: "x.jpg", Size: 4},
		Reader:   bytes.NewReader([]byte("JPEG")),
		Mimetype: "image/jpeg",
	}
	obj, err := d.Put(context.Background(), album, fs, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetID() == "" || obj.GetName() != "x.jpg" {
		t.Fatalf("put obj %+v", obj)
	}
	link, err := d.Link(context.Background(), &model.Object{ID: "11111111-1111-4111-8111-111111111111", Size: 12}, model.LinkArgs{})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := link.RangeReader.RangeRead(context.Background(), http_range.Range{Start: 1, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "EIC" {
		t.Fatalf("range body %q", b)
	}
}

func TestMakeDirAndReadOnly(t *testing.T) {
	srv := fakeImmich(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)
	obj, err := d.MakeDir(context.Background(), &model.Object{ID: "root", IsFolder: true}, "New")
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetName() != "New" {
		t.Fatalf("mkdir %s", obj.GetName())
	}
	d.ReadOnly = true
	_, err = d.Put(context.Background(), obj, &stream.FileStream{
		Obj:    &model.Object{Name: "a.jpg", Size: 4},
		Reader: bytes.NewReader([]byte("JPEG")),
	}, func(float64) {})
	if err == nil {
		t.Fatal("expected read-only")
	}
}

func names(objs []model.Obj) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.GetName()
	}
	return out
}

