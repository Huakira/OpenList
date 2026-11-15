package pixcake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"golang.org/x/time/rate"
)

type uploadRoundTripper func(*http.Request) (*http.Response, error)

func (f uploadRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func uploadTestPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	im := stdimage.NewRGBA(stdimage.Rect(0, 0, 32, 24))
	im.Set(0, 0, color.RGBA{42, 120, 230, 255})
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func uploadStream(ctx context.Context, name string, data []byte) *stream.FileStream {
	return &stream.FileStream{Ctx: ctx, Obj: &model.Object{Name: name, Size: int64(len(data))}, Reader: bytes.NewReader(data)}
}

func TestUploadRegistersBareKeysAndRequiresReadback(t *testing.T) {
	old := stream.ServerUploadLimit
	stream.ServerUploadLimit = rate.NewLimiter(rate.Inf, 65536)
	defer func() { stream.ServerUploadLimit = old }()
	for _, visible := range []bool{true, false} {
		t.Run(fmt.Sprint(visible), func(t *testing.T) {
			var registration map[string]any
			staged := map[string][]byte{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/api/project_image/upload":
					if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
						t.Error(err)
					}
					// The real service returns zero-valued registration data on success.
					fmt.Fprint(w, `{"code":0,"data":{"file_id":"","ori_path":""}}`)
				case "/v1/api/project_image/list":
					if visible && registration != nil {
						json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{map[string]any{"file_id": registration["file_id"], "file_size": registration["ori_size"], "ori_path": "https://download.invalid/image"}}})
					} else {
						fmt.Fprint(w, `{"code":0,"data":[]}`)
					}
				default:
					t.Errorf("unexpected API path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			d := &PixCake{Addition: Addition{APIBase: srv.URL}, token: "test-session", sts: &stsToken{AccessKeyID: "test-ak", AccessKeySecret: "test-sk", SecurityToken: "test-security", Expiration: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Bucket: "pixcheese", Prefix: "node/test/upload", Region: "https://oss-cn-hangzhou"}, stsAt: time.Now()}
			d.http = &http.Client{Transport: uploadRoundTripper(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Host, ".aliyuncs.com") {
					if r.URL.Host != "pixcheese.oss-cn-hangzhou.aliyuncs.com" {
						t.Errorf("OSS host %s", r.URL.Host)
					}
					if !strings.HasPrefix(r.Header.Get("Authorization"), "OSS test-ak:") {
						t.Error("missing OSS signature")
					}
					if r.Header.Get("x-oss-security-token") != "test-security" {
						t.Error("missing STS header")
					}
					b, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					staged[strings.TrimPrefix(r.URL.Path, "/")] = b
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			data := uploadTestPNG(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			progress := 0.0
			err := d.Put(ctx, &model.Object{ID: "p123", Path: "/test", IsFolder: true}, uploadStream(ctx, "a.png", data), func(p float64) { progress = p })
			if !visible {
				if err == nil || !strings.Contains(err.Error(), "未找到图片") {
					t.Fatalf("missing readback accepted: %v", err)
				}
				if progress >= 100 {
					t.Fatal("reported completion without readback")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if progress != 100 {
				t.Fatalf("progress %v", progress)
			}
			fid, _ := registration["file_id"].(string)
			want := "node/test/upload/123/UploadImage/Image/" + fid + "/a.png"
			if registration["ori_path"] != want {
				t.Fatalf("ori_path %v", registration["ori_path"])
			}
			if !bytes.Equal(staged[want], data) {
				t.Fatal("original content changed")
			}
			if len(staged) != 2 {
				t.Fatalf("expected two stages, got %d", len(staged))
			}
			if registration["name"] != "a.png" || registration["file_size"] != strconv.Itoa(len(data)) {
				t.Fatal("name/size mismatch")
			}
			for _, key := range []string{"operate_type", "history_operate_type", "opt_style", "history_opt_style"} {
				if registration[key] != float64(1) {
					t.Errorf("%s=%v", key, registration[key])
				}
			}
		})
	}
}

func TestUploadThumbnailAndEndpoint(t *testing.T) {
	im := stdimage.NewNRGBA(stdimage.Rect(10, 20, 1034, 532))
	im.SetNRGBA(10, 20, color.NRGBA{0, 0, 0, 0})
	b, err := uploadThumbnail(im)
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if thumb.Bounds().Dx() != 512 || thumb.Bounds().Dy() != 256 {
		t.Fatalf("thumbnail dimensions %v", thumb.Bounds())
	}
	r, g, bl, _ := thumb.At(0, 0).RGBA()
	if r < 60000 || g < 60000 || bl < 60000 {
		t.Fatal("transparent pixel not composited on white")
	}
	for _, region := range []string{"https://oss-cn-hangzhou", "cn-hangzhou", "oss-cn-hangzhou"} {
		u, err := uploadOSSEndpoint(&stsToken{Bucket: "pixcheese", Region: region})
		if err != nil || u.String() != "https://pixcheese.oss-cn-hangzhou.aliyuncs.com" {
			t.Fatalf("endpoint %v %v", u, err)
		}
	}
}

// Opt-in only: writes uniquely named fixtures to the explicitly selected test
// project. It never deletes existing content or prints credentials.
func TestPixCakeLiveUpload(t *testing.T) {
	token := os.Getenv("PIXCAKE_TEST_TOKEN")
	projectID := os.Getenv("PIXCAKE_TEST_PROJECT_ID")
	if token == "" || projectID == "" {
		t.Skip("set PIXCAKE_TEST_TOKEN and PIXCAKE_TEST_PROJECT_ID for live fixture uploads")
	}
	pid, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || pid <= 0 {
		t.Fatal("invalid test project ID")
	}
	old := stream.ServerUploadLimit
	stream.ServerUploadLimit = rate.NewLimiter(rate.Inf, 65536)
	defer func() { stream.ServerUploadLimit = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d := &PixCake{Addition: Addition{Token: token}}
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	data := uploadTestPNG(t)
	for _, format := range []string{"png", "jpg"} {
		t.Run(format, func(t *testing.T) {
			original := data
			if format == "jpg" {
				im, _, err := stdimage.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				var b bytes.Buffer
				if err := jpeg.Encode(&b, im, nil); err != nil {
					t.Fatal(err)
				}
				original = b.Bytes()
			}
			nonce, err := uploadUUID()
			if err != nil {
				t.Fatal(err)
			}
			name := "openlist-go-verify-" + nonce + "." + format
			dst := &model.Object{ID: "p" + projectID, Path: "/test", IsFolder: true}
			if err := d.Put(ctx, dst, uploadStream(ctx, name, original), nil); err != nil {
				t.Fatal(err)
			}
			imgs, err := d.listImages(ctx, pid)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range imgs {
				if im.FileName != name {
					continue
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, im.OriPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := d.http.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("download HTTP %d", resp.StatusCode)
				}
				download, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(original))+1))
				if err != nil {
					t.Fatal(err)
				}
				if sha256.Sum256(download) != sha256.Sum256(original) {
					t.Fatal("download SHA256 mismatch")
				}
				t.Logf("verified %s file_id=%s bytes=%d", format, im.FileID, len(download))
				return
			}
			t.Fatal("uploaded filename not found")
		})
	}
}
