package icloud_photos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
)

// call sends one JSON request to the bridge and decodes the JSON reply.
func (d *ICloudPhotos) call(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	return d.do(ctx, method, path, query, rd, -1, "application/json", out)
}

func (d *ICloudPhotos) do(ctx context.Context, method, path string, query url.Values, body io.Reader, size int64, ctype string, out any) error {
	u := strings.TrimRight(d.Address, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e errResp
		_ = json.Unmarshal(data, &e)
		return mapErr(res.StatusCode, e)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func mapErr(status int, e errResp) error {
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", errs.ObjectNotFound, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", errs.ObjectAlreadyExists, msg)
	case http.StatusForbidden, http.StatusLocked:
		return fmt.Errorf("%w: %s", errs.PermissionDenied, msg)
	}
	return fmt.Errorf("icloud bridge %d %s: %s", status, e.Kind, msg)
}
