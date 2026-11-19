package fnosphoto

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	Address     string `json:"address" required:"true" help:"飞牛 NAS 地址（含协议与端口），例如 https://192.168.1.10:5667，支持反代子路径"`
	Username    string `json:"username" required:"false" help:"飞牛登录用户名（AccessToken 未填写时必填）"`
	Password    string `json:"password" required:"false" default:"" help:"飞牛登录密码；账号开启两步验证时无法自动登录，请改填 AccessToken"`
	AccessToken string `json:"access_token" default:"" help:"可选：直接填飞牛网页端的 AccessToken 请求头（浏览器 F12 抓包），填写后跳过账号密码登录"`
	ShowShared  bool   `json:"show_shared" type:"bool" default:"true" help:"在根目录显示共享给当前账号的相册"`
	SkipVerify  bool   `json:"skip_verify" type:"bool" default:"true" help:"跳过 TLS 证书校验（飞牛默认自签证书，保持开启）"`
}

var config = driver.Config{
	Name:      "FnOSPhoto",
	LocalSort: true,
	NoUpload:  true,
	// 媒体直链必须携带 AccessToken 请求头，302 直链无法带上，
	// 因此声明 OnlyProxy：下载/播放始终经 OpenList 代理，无需用户手动开 Web 代理
	OnlyProxy: true,
	Alert:     `info|飞牛相册为只读挂载；媒体直链需要 AccessToken 请求头，本驱动已强制走 OpenList 代理（等效自动开启 Web 代理），无需任何手动设置。`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &FnOSPhoto{}
	})
}
