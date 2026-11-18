package immich

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/go-resty/resty/v2"
)

func (d *Immich) baseURL() string {
	return strings.TrimRight(strings.TrimSpace(d.Address), "/")
}

func (d *Immich) device() string {
	if s := strings.TrimSpace(d.DeviceID); s != "" {
		return s
	}
	return "openlist"
}

func (d *Immich) newClient() *resty.Client {
	return resty.New().
		SetBaseURL(d.baseURL()).
		SetHeader("x-api-key", d.ApiKey).
		SetHeader("Accept", "application/json").
		SetDisableWarn(true)
}

func (d *Immich) do(ctx context.Context, method, path string, body any, out any) error {
	req := d.client.R().SetContext(ctx)
	if body != nil {
		req.SetBody(body)
	}
	var ae apiErr
	req.SetError(&ae)
	if out != nil {
		req.SetResult(out)
	}
	resp, err := req.Execute(method, path)
	if err != nil {
		return err
	}
	if resp.IsError() {
		if ae.Error() != "immich api error" {
			return fmt.Errorf("immich %s %s: %s", method, path, ae.Error())
		}
		return fmt.Errorf("immich %s %s: HTTP %d", method, path, resp.StatusCode())
	}
	return nil
}

func (d *Immich) listAlbums(ctx context.Context) ([]albumDTO, error) {
	var albums []albumDTO
	if err := d.do(ctx, http.MethodGet, "/api/albums", nil, &albums); err != nil {
		return nil, err
	}
	return albums, nil
}

func (d *Immich) getAlbum(ctx context.Context, id string) (*albumDTO, error) {
	var a albumDTO
	if err := d.do(ctx, http.MethodGet, "/api/albums/"+url.PathEscape(id), nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *Immich) searchAssets(ctx context.Context, albumID string) ([]assetDTO, error) {
	var all []assetDTO
	page := 1
	for {
		payload := map[string]any{
			"size":     250,
			"page":     page,
			"withExif": true,
		}
		if albumID != "" && albumID != idAll {
			payload["albumIds"] = []string{albumID}
		}
		var sr searchResp
		if err := d.do(ctx, http.MethodPost, "/api/search/metadata", payload, &sr); err != nil {
			return nil, err
		}
		all = append(all, sr.Assets.Items...)
		if sr.Assets.NextPage == nil || strings.TrimSpace(*sr.Assets.NextPage) == "" {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(*sr.Assets.NextPage))
		if err != nil || n <= page {
			break
		}
		page = n
	}
	return all, nil
}

func (d *Immich) createAlbum(ctx context.Context, name string) (*albumDTO, error) {
	var a albumDTO
	if err := d.do(ctx, http.MethodPost, "/api/albums", map[string]any{"albumName": name}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *Immich) addToAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	if albumID == "" || albumID == idAll || albumID == idRoot || len(assetIDs) == 0 {
		return nil
	}
	return d.do(ctx, http.MethodPut, "/api/albums/"+url.PathEscape(albumID)+"/assets", map[string]any{"ids": assetIDs}, nil)
}

func (d *Immich) removeFromAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	return d.do(ctx, http.MethodDelete, "/api/albums/"+url.PathEscape(albumID)+"/assets", map[string]any{"ids": assetIDs}, nil)
}

func (d *Immich) trashAssets(ctx context.Context, ids []string) error {
	return d.do(ctx, http.MethodDelete, "/api/assets", map[string]any{"ids": ids}, nil)
}

func (d *Immich) deleteAlbum(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/api/albums/"+url.PathEscape(id), nil, nil)
}

func (d *Immich) renameAlbum(ctx context.Context, id, name string) (*albumDTO, error) {
	var a albumDTO
	if err := d.do(ctx, http.MethodPatch, "/api/albums/"+url.PathEscape(id), map[string]any{"albumName": name}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *Immich) uploadAsset(ctx context.Context, file model.FileStreamer, up driver.UpdateProgress) (*uploadResp, error) {
	name := file.GetName()
	mod := file.ModTime()
	if mod.IsZero() {
		mod = file.CreateTime()
	}
	ts := mod.UTC().Format("2006-01-02T15:04:05.000Z")
	deviceAssetID := fmt.Sprintf("openlist-%s-%d-%d", name, file.GetSize(), mod.Unix())
	if len(deviceAssetID) > 250 {
		deviceAssetID = deviceAssetID[:250]
	}
	body := driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	})
	var out uploadResp
	var ae apiErr
	resp, err := d.client.R().
		SetContext(ctx).
		SetFileReader("assetData", name, body).
		SetFormData(map[string]string{
			"deviceAssetId": deviceAssetID,
			"deviceId":      d.device(),
			"fileCreatedAt": ts,
			"fileModifiedAt": ts,
		}).
		SetResult(&out).
		SetError(&ae).
		Post("/api/assets")
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		if ae.Error() != "immich api error" {
			return nil, fmt.Errorf("immich upload: %s", ae.Error())
		}
		return nil, fmt.Errorf("immich upload: HTTP %d", resp.StatusCode())
	}
	if out.ID == "" {
		return nil, fmt.Errorf("immich upload: empty asset id")
	}
	return &out, nil
}

func (d *Immich) originalLink(ctx context.Context, id string, size int64) *model.Link {
	u := d.baseURL() + "/api/assets/" + url.PathEscape(id) + "/original"
	key := d.ApiKey
	cli := d.http
	rr := stream.RangeReaderFunc(func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header = http_range.ApplyRangeToHttpHeader(hr, req.Header)
		req.Header.Set("x-api-key", key)
		res, err := cli.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			res.Body.Close()
			return nil, fmt.Errorf("immich original: HTTP %d %s", res.StatusCode, strings.TrimSpace(string(b)))
		}
		return res.Body, nil
	})
	return &model.Link{RangeReader: rr, ContentLength: size}
}

func (d *Immich) thumbLink(ctx context.Context, id string) (*model.Link, error) {
	u := d.baseURL() + "/api/assets/" + url.PathEscape(id) + "/thumbnail?size=thumbnail"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", d.ApiKey)
	res, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("immich thumbnail: HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 8<<20 {
		return nil, fmt.Errorf("immich thumbnail: unexpected size %d", len(data))
	}
	ctype := res.Header.Get("Content-Type")
	size := int64(len(data))
	rr := stream.RangeReaderFunc(func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		start := hr.Start
		if start < 0 {
			start = 0
		}
		if start > size {
			start = size
		}
		length := hr.Length
		if length < 0 || start+length > size {
			length = size - start
		}
		return io.NopCloser(io.NewSectionReader(bytes.NewReader(data), start, length)), nil
	})
	link := &model.Link{ContentLength: size, RangeReader: rr}
	if ctype != "" {
		link.Header = http.Header{"Content-Type": []string{ctype}}
	}
	return link, nil
}
