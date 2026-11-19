package fnosphoto

import "encoding/json"

// fnListData /p/api/v1 列表接口的通用返回结构：{"list":[...],"total":n}
type fnListData[T any] struct {
	List  []T   `json:"list"`
	Total int64 `json:"total"`
}

// fnAlbum album/list 与 album_grant/list_to_me 返回的相册
type fnAlbum struct {
	AlbumID      int64  `json:"albumId"`
	AlbumName    string `json:"albumName"`
	PosterImgURL string `json:"posterImgUrl"`
	PosterURL    string `json:"posterUrl"`
	PhotoCount   int    `json:"photoCount"`
	VideoCount   int    `json:"videoCount"`
	OwnerName    string `json:"ownerName"`
	ModifyTime   int64  `json:"modifyTime"`
}

// fnThumb photo.additional.thumbnail，服务器生成的各规格媒体地址（相对路径）
type fnThumb struct {
	MURL        string `json:"mUrl"`
	SURL        string `json:"sUrl"`
	XsURL       string `json:"xsUrl"`
	OriginalURL string `json:"originalUrl"`
	VideoURL    string `json:"videoUrl"`
}

// fnPhoto album/photos 返回的照片/视频条目
type fnPhoto struct {
	ID            int64  `json:"id"`
	FileName      string `json:"fileName"`
	Category      string `json:"category"` // "video" / "photo"
	DateTime      string `json:"dateTime"`
	PhotoDateTime string `json:"photoDateTime"`
	FileSize      int64  `json:"fileSize"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	IsLive        int    `json:"isLive"`
	IsCollect     int    `json:"isCollect"`
	Additional    struct {
		Thumbnail fnThumb `json:"thumbnail"`
	} `json:"additional"`
}

// galleryDay gallery/timeline 返回的单天统计

type galleryDay struct {
	Year      int `json:"year"`
	Month     int `json:"month"`
	Day       int `json:"day"`
	ItemCount int `json:"itemCount"`
}

// fnBaseResp 业务接口通用响应：正常时 code==0（部分接口用 errno），数据在 data
type fnBaseResp struct {
	Code   int             `json:"code"`
	Errno  int             `json:"errno"`
	Msg    string          `json:"msg"`
	Result string          `json:"result"`
	Data   json.RawMessage `json:"data"`
}

// wsReply WebSocket 握手回复
type wsReply struct {
	Errno  int    `json:"errno"`
	Result string `json:"result"`
	Pub    string `json:"pub"`
	Si     string `json:"si"`
	Token  string `json:"token"`
	BackID string `json:"backId"`
	Secret string `json:"secret"` // base64(AES-CBC(Trim-Sign HMAC key))，用登录信封的 key/iv 解
	Msg    string `json:"msg"`
}

func (r *fnBaseResp) code() int {
	if r.Code != 0 {
		return r.Code
	}
	return r.Errno
}

func (r *fnBaseResp) message() string {
	if r.Msg != "" {
		return r.Msg
	}
	return r.Result
}
