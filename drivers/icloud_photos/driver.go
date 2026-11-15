// Package icloud_photos is an OpenList driver for iCloud Photos.
//
// The iCloud protocol lives in a small Python service (the bridge, pyicloud)
// that is embedded in the binary and started as a child process on Init; this
// file only forwards calls to it over HTTP on 127.0.0.1.
package icloud_photos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

type ICloudPhotos struct {
	model.Storage
	Addition
	client *http.Client
	// internal: the local bridge this instance talks to, set by Init
	address string
	token   string
}

func (d *ICloudPhotos) Config() driver.Config           { return config }
func (d *ICloudPhotos) GetAddition() driver.Additional { return &d.Addition }

func (d *ICloudPhotos) Init(ctx context.Context) error {
	if strings.TrimSpace(d.AppleID) == "" {
		return fmt.Errorf("请填写 Apple ID")
	}
	if d.RootFolderID == "" {
		d.RootFolderID = "root"
	}
	if strings.TrimSpace(d.DataDir) == "" {
		d.DataDir = filepath.Join(conf.DataDir, "icloud-photos")
	}
	// The bridge runs with this as its working directory; keep it absolute so a
	// later chdir cannot split session and index across two places.
	if abs, err := filepath.Abs(d.DataDir); err == nil {
		d.DataDir = abs
	}
	d.client = &http.Client{Timeout: 6 * time.Hour} // uploads of long videos
	p, err := ensureBridge(ctx, d)
	if err != nil {
		return err
	}
	d.address, d.token = p.baseURL, p.token
	return d.ensureSession(ctx)
}

// Drop deliberately leaves the bridge process running: a sign-in waiting for a
// 2FA code keeps its half-finished state inside that process, and Init runs
// again between the two saves.
func (d *ICloudPhotos) Drop(ctx context.Context) error { return nil }

type healthResp struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error"`
	Session      string `json:"session"`
	PendingLogin string `json:"pending_login"`
}

type loginResp struct {
	OK       bool `json:"ok"`
	Needs2FA bool `json:"needs_2fa"`
}

func (d *ICloudPhotos) health(ctx context.Context) (*healthResp, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.address+"/v1/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("内置 bridge 无响应: %w", err)
	}
	defer res.Body.Close()
	var h healthResp
	_ = json.NewDecoder(res.Body).Decode(&h)
	if res.StatusCode >= 400 && h.Error == "" {
		h.Error = res.Status
	}
	return &h, nil
}

// ensureSession makes sure the bridge holds a working iCloud session, walking
// the owner through sign-in from the storage form when it does not:
// save with Apple ID + password → Apple pushes a code → save again with the code.
func (d *ICloudPhotos) ensureSession(ctx context.Context) error {
	h, err := d.health(ctx)
	if err != nil {
		return err
	}
	if h.OK {
		return nil
	}
	code := strings.TrimSpace(d.TwoFactorCode)
	askCode := fmt.Errorf("Apple 已向你的设备发送两步验证提示：请把设备上显示的 6 位验证码填进「两步验证码」，再保存一次")
	switch {
	case h.PendingLogin != "" && code == "":
		return askCode
	case h.PendingLogin == "" && strings.TrimSpace(d.Password) == "":
		return fmt.Errorf("还没有 iCloud 会话：请填写 Apple ID 和密码后保存，Apple 会推送两步验证")
	}
	body := map[string]string{"apple_id": d.AppleID}
	if h.PendingLogin != "" {
		body["code"] = code
	} else {
		body["password"] = d.Password
	}
	var r loginResp
	if err := d.call(ctx, http.MethodPost, "/v1/login", nil, body, &r); err != nil {
		return fmt.Errorf("iCloud 登录失败：%w（若提示账号或密码错误，先确认「中国区账号」开关与 Apple ID 地区一致：中国大陆填 icloud.com.cn，其余地区用国际版）", err)
	}
	if r.Needs2FA {
		return askCode
	}
	return nil
}

func (d *ICloudPhotos) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	var r listResp
	if err := d.call(ctx, http.MethodGet, "/v1/list", url.Values{"id": {dir.GetID()}}, nil, &r); err != nil {
		return nil, err
	}
	out := make([]model.Obj, 0, len(r.Items))
	for _, e := range r.Items {
		if !d.ShowSmartAlbums && strings.HasPrefix(e.ID, "smart:") {
			continue
		}
		out = append(out, e.toObj())
	}
	return out, nil
}

func (d *ICloudPhotos) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	// Always stream through the bridge (OnlyProxy): the bridge's egress is the
	// one that reaches iCloud, and signed URLs would be fetched by the client.
	q := url.Values{"id": {file.GetID()}}
	rr := stream.RangeReaderFunc(func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		u := strings.TrimRight(d.address, "/") + "/v1/raw?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header = http_range.ApplyRangeToHttpHeader(hr, req.Header)
		req.Header.Set("Authorization", "Bearer "+d.token)
		res, err := d.client.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= 400 {
			res.Body.Close()
			return nil, fmt.Errorf("bridge raw: %s", res.Status)
		}
		return res.Body, nil
	})
	return &model.Link{RangeReader: rr}, nil
}

func (d *ICloudPhotos) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	var e entry
	err := d.call(ctx, http.MethodPost, "/v1/mkdir", nil, map[string]string{"parent": parentDir.GetID(), "name": dirName}, &e)
	if err != nil {
		return nil, err
	}
	return e.toObj(), nil
}

func (d *ICloudPhotos) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	var e entry
	err := d.call(ctx, http.MethodPost, "/v1/rename", nil, map[string]string{"id": srcObj.GetID(), "name": newName}, &e)
	if err != nil {
		return nil, err
	}
	return e.toObj(), nil
}

func (d *ICloudPhotos) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.call(ctx, http.MethodPost, "/v1/move", nil, map[string]string{"id": srcObj.GetID(), "to": dstDir.GetID()}, nil)
}

func (d *ICloudPhotos) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.call(ctx, http.MethodPost, "/v1/copy", nil, map[string]string{"id": srcObj.GetID(), "to": dstDir.GetID()}, nil)
}

func (d *ICloudPhotos) Remove(ctx context.Context, obj model.Obj) error {
	return d.call(ctx, http.MethodPost, "/v1/remove", nil, map[string]string{"id": obj.GetID()}, nil)
}

func (d *ICloudPhotos) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	body := driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	})
	q := url.Values{"parent": {dstDir.GetID()}, "name": {file.GetName()}}
	var e entry
	if err := d.do(ctx, http.MethodPut, "/v1/upload", q, body, file.GetSize(), "application/octet-stream", &e); err != nil {
		return nil, err
	}
	return e.toObj(), nil
}

// Other exposes favorite|unfavorite|hide|unhide|recover|purge, plus refresh
// (re-read the library) and login (retry the sign-in flow). File operations can
// never reach these; they need an explicit /api/fs/other call.
func (d *ICloudPhotos) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	switch args.Method {
	case "favorite", "unfavorite", "hide", "unhide", "recover", "purge":
	case "refresh":
		var r map[string]any
		return r, d.call(ctx, http.MethodPost, "/v1/refresh", nil, map[string]string{}, &r)
	case "login":
		if err := d.ensureSession(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	default:
		return nil, errs.NotSupport
	}
	var r map[string]any
	err := d.call(ctx, http.MethodPost, "/v1/other", nil, map[string]string{"op": args.Method, "id": args.Obj.GetID()}, &r)
	return r, err
}

var _ driver.Driver = (*ICloudPhotos)(nil)
var _ driver.MkdirResult = (*ICloudPhotos)(nil)
var _ driver.RenameResult = (*ICloudPhotos)(nil)
var _ driver.Move = (*ICloudPhotos)(nil)
var _ driver.Copy = (*ICloudPhotos)(nil)
var _ driver.Remove = (*ICloudPhotos)(nil)
var _ driver.PutResult = (*ICloudPhotos)(nil)
var _ driver.Other = (*ICloudPhotos)(nil)
