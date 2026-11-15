package pixcake

import (
	"encoding/json"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
)

// Addition is the storage configuration shown in the OpenList admin panel.
type Addition struct {
	driver.RootPath
	// AuthToken 是像素蛋糕的登录 JWT。
	//
	// 取值方式有两种：
	//   1. 直接粘贴 JWT 原文（345 字符左右，形如 eyJhbGciOi...）；
	//   2. 粘贴本地库里的 tokenV2 密文 + 硬件 UUID，由驱动自行解密。
	//
	// 管理面板里的字段名和提示文字不读这里的 help 标签，而是查前端语言包
	// drivers.<驱动名>.<字段>[-tips]；help 只要非空，前端就会显示对应提示。
	// 语言包见 custom/i18n/，由 custom/patch-frontend.py 在构建时写入前端。
	Token string `json:"token" type:"text" required:"false" help:"Login JWT (starts with eyJ). Use this OR token_v2 + hardware_uuid."`
	// TokenV2 是 macOS 客户端 base.db 中 user.tokenV2 字段的十六进制密文。
	TokenV2 string `json:"token_v2" type:"text" required:"false" help:"Hex ciphertext of user.tokenV2 from the macOS client; needs hardware_uuid."`
	// HardwareUUID 是本机硬件 UUID，tokenV2 的 AES 密钥由它派生。
	HardwareUUID string `json:"hardware_uuid" type:"string" required:"false" help:"Hardware UUID of the Mac the token came from (system_profiler SPHardwareDataType)."`

	// SupportAppID 决定能看见哪些项目，客户端默认固定 1。
	SupportAppID int `json:"support_app_id" type:"number" default:"1" help:"app_id of the projects; the client always uses 1."`

	// APIBase 允许自建反代，默认官方地址。
	APIBase string `json:"api_base" type:"string" default:"https://link-api.pixcakeai.com" help:"API base URL; normally leave as default."`

	// ShowTeam 是否列出团队项目。
	ShowTeam bool `json:"show_team" type:"bool" default:"false" help:"Also list team projects."`

	// RootFolderID 可选：只挂载某个项目（填项目 ID）。
	RootFolderID string `json:"root_folder_id" type:"string" required:"false" help:"Mount a single project by its ID; empty mounts all projects."`
}

// 常用时间格式，像素蛋糕后端统一返回带时区的 ISO8601。
const timeLayout = time.RFC3339

// 接口返回结构。字段只取驱动用得到的那部分。

type apiResp struct {
	Code    *int            `json:"code"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type spaceInfo struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	MemberType     int    `json:"member_type"`
	IsAdmin        bool   `json:"is_admin"`
	TotalSpaceSize int64  `json:"total_space_size"`
	SpaceUsed      int64  `json:"space_used"`
}

type cloudInfo struct {
	TotalSpace       int64 `json:"total_space"`
	UsedSpace        int64 `json:"used_space"`
	HasCreateProject bool  `json:"has_create_project"`
	HasCloudSpace    bool  `json:"has_cloud_space"`
}

type project struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	AppID      int      `json:"app_id"`
	IsDeleted  bool     `json:"is_deleted"`
	TotalCount int      `json:"total_count"`
	OriCount   int      `json:"ori_count"`
	Covers     []string `json:"covers"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	CreateAppVersion string `json:"create_app_version"`
}

type image struct {
	GroupID       int    `json:"group_id"`
	FileID        string `json:"file_id"`
	OriFileID     string `json:"ori_file_id"`
	FileName      string `json:"file_name"`
	FileSize      int64  `json:"file_size"`
	FileExt       string `json:"file_ext"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	CapturedAt    string `json:"captured_at"`
	ImgWidth      int    `json:"img_width"`
	ImgHeight     int    `json:"img_height"`
	OriPath       string `json:"ori_path"`
	OriThumbPath  string `json:"ori_thumb_path"`
	PreviewPath   string `json:"preview_path"`
	IsDeleted     bool   `json:"is_deleted"`
	RetouchType   int    `json:"retouch_type"`
	UploadUserID  int64  `json:"upload_user_id"`
	CloudUpdateAt string `json:"cloud_update_time"`
}

type stsToken struct {
	StatusCode      int    `json:"status_code"`
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
	Bucket          string `json:"bucket"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Prefix          string `json:"default_prefix_dir"`
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
