package immich

import (
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

const (
	idRoot = "root"
	idAll  = "all"
)

type albumDTO struct {
	ID         string `json:"id"`
	AlbumName  string `json:"albumName"`
	AssetCount int    `json:"assetCount"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

type assetDTO struct {
	ID                 string `json:"id"`
	OriginalFileName   string `json:"originalFileName"`
	OriginalMimeType   string `json:"originalMimeType"`
	FileCreatedAt      string `json:"fileCreatedAt"`
	FileModifiedAt     string `json:"fileModifiedAt"`
	LocalDateTime      string `json:"localDateTime"`
	Type               string `json:"type"`
	LivePhotoVideoId   string `json:"livePhotoVideoId"`
	ExifInfo           *exifDTO `json:"exifInfo"`
}

type exifDTO struct {
	FileSizeInByte int64 `json:"fileSizeInByte"`
}

type searchResp struct {
	Assets struct {
		Total    int        `json:"total"`
		Count    int        `json:"count"`
		Items    []assetDTO `json:"items"`
		NextPage *string    `json:"nextPage"`
	} `json:"assets"`
}

type uploadResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type apiErr struct {
	Message    any    `json:"message"`
	Err        string `json:"error"`
	StatusCode int    `json:"statusCode"`
}

func (e apiErr) Error() string {
	switch m := e.Message.(type) {
	case string:
		if m != "" {
			return m
		}
	case []any:
		parts := make([]string, 0, len(m))
		for _, x := range m {
			parts = append(parts, strings.TrimSpace(stringify(x)))
		}
		if s := strings.Join(parts, "; "); s != "" {
			return s
		}
	}
	if e.Err != "" {
		return e.Err
	}
	return "immich api error"
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

type folderObj struct {
	model.Object
}

type fileObj struct {
	model.Object
	mime string
	live string
}

func (f *fileObj) GetHash() utils.HashInfo { return utils.HashInfo{} }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (a albumDTO) toObj(name string) model.Obj {
	mod := parseTime(a.UpdatedAt)
	if mod.IsZero() {
		mod = parseTime(a.CreatedAt)
	}
	return &folderObj{Object: model.Object{
		ID:       a.ID,
		Name:     name,
		IsFolder: true,
		Modified: mod,
	}}
}

func (a assetDTO) size() int64 {
	if a.ExifInfo != nil {
		return a.ExifInfo.FileSizeInByte
	}
	return 0
}

func (a assetDTO) toObj(name string) model.Obj {
	mod := parseTime(a.FileModifiedAt)
	if mod.IsZero() {
		mod = parseTime(a.FileCreatedAt)
	}
	return &fileObj{
		Object: model.Object{
			ID:       a.ID,
			Name:     name,
			Size:     a.size(),
			Modified: mod,
		},
		mime: a.OriginalMimeType,
		live: a.LivePhotoVideoId,
	}
}
