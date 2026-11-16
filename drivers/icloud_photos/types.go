package icloud_photos

import (
	"context"
	"net/url"
	stdpath "path"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

type entry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Created  string `json:"created"`
	Thumb    string `json:"thumb"`
	Status   string `json:"status"`
}

type listResp struct {
	Items []entry `json:"items"`
}

type linkResp struct {
	URL   string `json:"url"`
	Local bool   `json:"local"`
}

type errResp struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// thumbURL points a listing tile at this server instead of iCloud: the signed
// iCloud URL expires in minutes and would be fetched by the browser, while /d
// goes through the bridge, which keeps the bytes on disk after the first view.
func thumbURL(ctx context.Context, reqPath, name string) string {
	if reqPath == "" || name == "" {
		return ""
	}
	base, ok := ctx.Value(conf.ThumbnailBaseURLKey).(*url.URL)
	if !ok {
		base = &url.URL{
			Path: stdpath.Join("/d", reqPath),
			RawQuery: url.Values{
				"type": {"thumb"},
				"sign": {sign.Sign(stdpath.Join(reqPath, name))},
			}.Encode(),
		}
	}
	return common.GetApiUrl(ctx) + base.JoinPath(url.PathEscape(name)).String()
}

func (e entry) toObj() model.Obj {
	return &model.ObjThumb{
		Object: model.Object{
			ID:       e.ID,
			Name:     e.Name,
			Size:     e.Size,
			Modified: parseTime(e.Modified),
			Ctime:    parseTime(e.Created),
			IsFolder: e.IsDir,
		},
	}
}

func objWithThumb(ctx context.Context, e entry, reqPath string) model.Obj {
	o := &model.ObjThumb{
		Object: model.Object{
			ID:       e.ID,
			Name:     e.Name,
			Size:     e.Size,
			Modified: parseTime(e.Modified),
			Ctime:    parseTime(e.Created),
			IsFolder: e.IsDir,
		},
	}
	if !e.IsDir && e.Status != "processing" {
		o.Thumbnail = model.Thumbnail{Thumbnail: thumbURL(ctx, reqPath, e.Name)}
	}
	return o
}
