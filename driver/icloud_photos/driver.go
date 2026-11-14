// Package icloud_photos is a thin OpenList driver for iCloud Photos.
// Every rule lives in icloud-photos-bridge; this file only forwards calls.
package icloud_photos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
}

func (d *ICloudPhotos) Config() driver.Config         { return config }
func (d *ICloudPhotos) GetAddition() driver.Additional { return &d.Addition }

func (d *ICloudPhotos) Init(ctx context.Context) error {
	if d.Address == "" || d.Token == "" {
		return fmt.Errorf("address and token are required")
	}
	if d.RootFolderID == "" {
		d.RootFolderID = "root"
	}
	d.client = &http.Client{Timeout: 6 * time.Hour} // uploads of long videos
	var h map[string]any
	if err := d.call(ctx, http.MethodGet, "/v1/health", nil, nil, &h); err != nil {
		return fmt.Errorf("bridge unhealthy: %w", err)
	}
	return nil
}

func (d *ICloudPhotos) Drop(ctx context.Context) error { return nil }

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

// Link never hands out an iCloud URL: every byte goes through the bridge, which
// fetches from iCloud over its own egress (no 302 to the client). OpenList's
// proxy then serves it, Range included.
func (d *ICloudPhotos) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	q := url.Values{"id": {file.GetID()}}
	rr := stream.RangeReaderFunc(func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		u := strings.TrimRight(d.Address, "/") + "/v1/raw?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header = http_range.ApplyRangeToHttpHeader(hr, req.Header)
		req.Header.Set("Authorization", "Bearer "+d.Token)
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

// Other exposes favorite|unfavorite|hide|unhide|recover|purge. File
// operations can never reach these; they need an explicit /api/fs/other call.
func (d *ICloudPhotos) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	switch args.Method {
	case "favorite", "unfavorite", "hide", "unhide", "recover", "purge":
	case "refresh":
		var r map[string]any
		return r, d.call(ctx, http.MethodPost, "/v1/refresh", nil, map[string]string{}, &r)
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
