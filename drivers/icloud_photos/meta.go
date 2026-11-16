package icloud_photos

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	// Field labels, tips and the banner come from the frontend language pack
	// (drivers.iCloudPhotos.<field>[-tips], drivers.config.iCloudPhotos.alert);
	// see custom/i18n/. A non-empty help tag is what makes the frontend show a
	// tip, but the text itself has to be in that language pack.
	AppleID         string `json:"apple_id" type:"string" required:"true" help:"Apple ID of the account that owns the photo library."`
	Password        string `json:"password" type:"string" required:"false" help:"Apple ID password. Only needed to sign in; the session is saved afterwards."`
	TwoFactorCode   string `json:"two_factor_code" type:"string" required:"false" help:"Six-digit code Apple sends. Fill it in and save again when asked."`
	China           bool   `json:"china" type:"bool" default:"false" help:"The Apple ID's region is China mainland (icloud.com.cn)."`
	EgressProxy     string `json:"proxy" type:"string" required:"false" help:"Proxy for the iCloud connection, e.g. socks5h://host:1080. Empty keeps the container's proxy settings."`
	ShowSmartAlbums bool   `json:"show_smart_albums" type:"bool" default:"true" help:"Show smart albums (Favorites, Videos, ...) next to your own albums."`
	WriteExisting   bool   `json:"write_existing" type:"bool" default:"false" help:"Also allow changes to albums and photos this storage did not create."`
	ReadOnly        bool   `json:"read_only" type:"bool" default:"false" help:"Refuse every write: upload, delete, rename, move, copy."`
	DataDir         string `json:"data_dir" type:"string" required:"false" help:"Where the bridge keeps its session and index. Defaults to data/icloud-photos."`
}

var config = driver.Config{
	Name:        "iCloudPhotos",
	LocalSort:   true,
	DefaultRoot: "root",
	CheckStatus: true,
	Alert:       "info|Self-contained: the bridge runs inside this container (python3 + pyicloud), nothing else to deploy. Writes only touch albums created here; deleting archives a photo instead of removing it.",
	// Downloads are always proxied through the bridge; no 302.
	OnlyProxy: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &ICloudPhotos{}
	})
}
