package icloud_photos

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	Address         string `json:"address" required:"true" default:"http://icloud-bridge:8080" help:"icloud-photos-bridge base URL (internal network)"`
	Token           string `json:"token" required:"true" help:"BRIDGE_TOKEN of the bridge"`
	ShowSmartAlbums bool   `json:"show_smart_albums" default:"true"`
}

var config = driver.Config{
	Name:        "iCloudPhotos",
	LocalSort:   true,
	DefaultRoot: "root",
	CheckStatus: true,
	// Downloads are always proxied through the bridge; no 302.
	OnlyProxy: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &ICloudPhotos{}
	})
}
