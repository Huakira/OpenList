package pixcake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	stdimage "image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// Put stages an original and thumbnail, then registers their bare OSS keys.
// The registration response may contain zero values even after success; listing
// the new file ID is the commit check. A failed check must never be retried with
// a new ID automatically, since the first registration may have committed.
func (d *PixCake) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	if d.ReadOnly {
		return errReadOnly
	}
	pid, ok := projectIDFromObj(dstDir)
	if !ok {
		if dstDir.GetPath() == "" || dstDir.GetPath() == "/" {
			return fmt.Errorf("请先创建或选择云项目，再上传图片")
		}
		p, err := d.projectByPath(ctx, dstDir.GetPath())
		if err != nil {
			return err
		}
		pid = p.ID
	}
	name := file.GetName()
	if name == "" || name != path.Base(name) || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("图片文件名无效")
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if ext != "jpg" && ext != "jpeg" && ext != "png" && ext != "gif" {
		return fmt.Errorf("上传目前仅支持 JPEG、PNG、GIF 图片")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Spool once: FileStreamer is not necessarily seekable. Only our temporary
	// copy is removed; the caller retains ownership of the source stream.
	tmp, err := os.CreateTemp("", "openlist-pixcake-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	digest := md5.New()
	size, err := io.Copy(io.MultiWriter(tmp, digest), &driver.ReaderWithCtx{Reader: file, Ctx: ctx})
	if err != nil {
		return fmt.Errorf("缓存图片：%w", err)
	}
	if size <= 0 {
		return fmt.Errorf("图片为空")
	}
	if file.GetSize() >= 0 && size != file.GetSize() {
		return fmt.Errorf("图片大小不符：预期 %d，读取 %d", file.GetSize(), size)
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	cfg, format, err := stdimage.DecodeConfig(tmp)
	if err != nil {
		return fmt.Errorf("读取图片尺寸：%w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 64_000_000 {
		return fmt.Errorf("图片尺寸超出当前上传支持范围（最多 6400 万像素）")
	}
	if format != "jpeg" && format != "png" && format != "gif" {
		return fmt.Errorf("不支持的图片编码：%s", format)
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoded, _, err := stdimage.Decode(&driver.ReaderWithCtx{Reader: tmp, Ctx: ctx})
	if err != nil {
		return fmt.Errorf("解码缩略图：%w", err)
	}
	thumb, err := uploadThumbnail(decoded)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sts, err := d.uploadToken(ctx)
	if err != nil {
		return err
	}
	fid, err := uploadUUID()
	if err != nil {
		return err
	}
	folder := strings.TrimRight(sts.Prefix, "/") + "/" + strconv.FormatInt(pid, 10) + "/UploadImage/Image/" + fid
	ori, icon := folder+"/"+name, folder+"/thumb/"+name+".jpg"
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if up == nil {
		up = func(float64) {}
	}
	progress := &uploadProgress{total: size, up: up}
	original := driver.NewLimitedUploadStream(ctx, io.TeeReader(tmp, progress))
	if err = d.putOSS(ctx, sts, ori, original, size, "link_ori"); err != nil {
		return fmt.Errorf("上传原图：%w", err)
	}
	if err = d.putOSS(ctx, sts, icon, driver.NewLimitedUploadStream(ctx, bytes.NewReader(thumb)), int64(len(thumb)), "link_icon"); err != nil {
		return fmt.Errorf("上传缩略图：%w", err)
	}
	body := map[string]any{
		"project_id": pid, "icon_path": icon, "icon_file_ext": "jpg", "ori_path": ori,
		"preset_path": "", "exif": "{}", "color": 0, "rating": 0, "flag": 0,
		"icon_size": len(thumb), "ori_size": size, "preset_size": 0, "name": name,
		"width": cfg.Width, "height": cfg.Height, "captured_at": time.Now().Unix(),
		"file_ext": ext, "file_id": fid, "history_uuid": "", "operate_type": 1,
		"opt_style": 1, "history_operate_type": 1, "history_opt_style": 1,
		"ori_md5": hex.EncodeToString(digest.Sum(nil)), "image_uuid": fid,
		// This compatibility value was verified against client 10.1.0-343. It is
		// not a claim that every version constructs its suffix this way.
		"req_version": "ars_q19-10.1.0-343", "image_preview_path": "",
		"image_preview_file_ext": "", "ext": "", "file_size": strconv.FormatInt(size, 10),
		"modify_time": "", "ori_file_id": "",
	}
	if err = d.request(ctx, http.MethodPost, "/v2/api/project_image/upload", nil, body, nil); err != nil {
		return fmt.Errorf("登记图片 %s（请核对项目后再重试）：%w", fid, err)
	}
	if err = d.verifyUpload(ctx, pid, fid, size); err != nil {
		return err
	}
	up(100)
	return nil
}

type uploadProgress struct {
	total, done int64
	up          driver.UpdateProgress
}

func (p *uploadProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	v := float64(p.done) * 95 / float64(p.total)
	if v > 95 {
		v = 95
	}
	p.up(v)
	return len(b), nil
}

func uploadUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func uploadThumbnail(src stdimage.Image) ([]byte, error) {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("图片尺寸为空")
	}
	if w > 512 || h > 512 {
		if w >= h {
			h = max(1, h*512/w)
			w = 512
		} else {
			w = max(1, w*512/h)
			h = 512
		}
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := src.At(bounds.Min.X+x*bounds.Dx()/w, bounds.Min.Y+y*bounds.Dy()/h).RGBA()
			// Composite premultiplied RGBA over white for JPEG's missing alpha.
			dst.SetRGBA(x, y, color.RGBA{uint8((r + 65535 - a) >> 8), uint8((g + 65535 - a) >> 8), uint8((b + 65535 - a) >> 8), 255})
		}
	}
	var out bytes.Buffer
	err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85})
	return out.Bytes(), err
}

func uploadOSSEndpoint(sts *stsToken) (*url.URL, error) {
	endpoint := strings.TrimRight(sts.Endpoint, "/")
	if endpoint == "" {
		region := strings.TrimPrefix(strings.TrimPrefix(sts.Region, "https://"), "http://")
		region = strings.TrimSuffix(region, ".aliyuncs.com")
		if region == "" {
			region = "oss-cn-hangzhou"
		}
		if !strings.HasPrefix(region, "oss-") {
			region = "oss-" + region
		}
		endpoint = "https://" + region + ".aliyuncs.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("STS OSS endpoint 无效")
	}
	if !strings.HasPrefix(u.Host, sts.Bucket+".") {
		u.Host = sts.Bucket + "." + u.Host
	}
	u.Path = ""
	return u, nil
}

func (d *PixCake) putOSS(ctx context.Context, sts *stsToken, key string, body io.Reader, size int64, resource string) error {
	u, err := uploadOSSEndpoint(sts)
	if err != nil {
		return err
	}
	u.Path = "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-oss-security-token", sts.SecurityToken)
	req.Header.Set("x-oss-tagging", "resource_status=cache_v1&app_id=1&resource_type="+resource)
	canonical := "PUT\n\n" + req.Header.Get("Content-Type") + "\n" + req.Header.Get("Date") + "\n" +
		"x-oss-security-token:" + sts.SecurityToken + "\n" + "x-oss-tagging:" + req.Header.Get("x-oss-tagging") + "\n" + "/" + sts.Bucket + "/" + key
	mac := hmac.New(sha1.New, []byte(sts.AccessKeySecret))
	mac.Write([]byte(canonical))
	req.Header.Set("Authorization", "OSS "+sts.AccessKeyID+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	// Keep the configured transport (proxy/TLS), but let context control the
	// original's duration instead of imposing the small API request timeout.
	transport := http.DefaultTransport
	if d.http != nil && d.http.Transport != nil {
		transport = d.http.Transport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OSS PUT HTTP %d", resp.StatusCode)
	}
	return nil
}

func (d *PixCake) verifyUpload(ctx context.Context, pid int64, fid string, size int64) error {
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		imgs, err := d.listImages(ctx, pid)
		if err != nil {
			return fmt.Errorf("登记后验证失败（图片 ID %s，请先核对项目）：%w", fid, err)
		}
		for _, im := range imgs {
			if im.FileID == fid {
				if im.FileSize != size || im.OriPath == "" {
					return fmt.Errorf("登记后图片 %s 的大小或原图地址不符，请核对项目", fid)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("登记返回成功，但项目中未找到图片 %s；请核对项目后再重试", fid)
}
