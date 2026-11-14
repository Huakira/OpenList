package icloud_photos

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
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
		Thumbnail: model.Thumbnail{Thumbnail: e.Thumb},
	}
}
