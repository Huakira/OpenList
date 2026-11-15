package icloud_photos

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	// Field labels and tips come from the frontend language pack
	// (drivers.iCloudPhotos.<field>[-tips]), see custom/i18n/. A non-empty
	// help tag is what makes the frontend show the tip.
	Address         string `json:"address" type:"string" required:"true" default:"http://icloud-bridge:8080" help:"icloud-photos-bridge base URL, reachable from the OpenList container."`
	Token           string `json:"token" type:"string" required:"true" help:"BRIDGE_TOKEN from the bridge's deploy/.env."`
	ShowSmartAlbums bool   `json:"show_smart_albums" type:"bool" default:"true" help:"Show smart albums (Favorites, Videos, ...) next to your own albums."`
}

var config = driver.Config{
	Name:        "iCloudPhotos",
	LocalSort:   true,
	DefaultRoot: "root",
	CheckStatus: true,
	Alert:       "info|Needs the icloud-photos-bridge service. Writes only touch albums the bridge created; deleting archives instead of removing.",
	// Downloads are always proxied through the bridge; no 302.
	OnlyProxy: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &ICloudPhotos{}
	})
}
