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
	ShowAll     bool   `json:"show_all" type:"bool" default:"true" help:"在根目录显示「全部照片」虚拟目录（按拍摄时间倒序列出全部照片/视频，不含相册分组）"`
	SkipVerify  bool   `json:"skip_verify" type:"bool" default:"true" help:"跳过 TLS 证书校验（飞牛默认自签证书，保持开启）"`
}

var config = driver.Config{
	Name:      "FnOSPhoto",
	LocalSort: true,
	// 写支持：Put 上传（/upload+notice）、Remove 走回收站、MakeDir 建相册。
	// 飞牛相册无版本历史：同名不覆盖（报错），删除仅入回收站可恢复。
	// 媒体直链必须携带 AccessToken 请求头，302 直链无法带上，
	// 因此声明 OnlyProxy：下载/播放始终经 OpenList 代理，无需用户手动开 Web 代理
	OnlyProxy: true,
	Alert:     `info|上传=落盘飞牛相册库并归入相册；删除=移入飞牛回收站（可恢复，不永久删）；根目录新建文件夹=建相册。同名文件不覆盖。媒体直链需 AccessToken 头，本驱动已强制走 OpenList 代理，无需手动设置。`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &FnOSPhoto{}
	})
}
