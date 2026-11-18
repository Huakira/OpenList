package immich

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// Addition is the storage form. Labels/tips live in custom/i18n/.
type Addition struct {
	driver.RootID
	Address       string `json:"address" required:"true" help:"Immich base URL, e.g. https://immich.example.com"`
	ApiKey        string `json:"api_key" required:"true" help:"API key from Immich Account Settings → API Keys. Sent as x-api-key."`
	ShowType      string `json:"show_type" type:"select" options:"root,root_only_album,root_only_file" default:"root_only_album" help:"root: albums plus an All folder; root_only_album: albums only; root_only_file: every asset at the root."`
	AlbumID       string `json:"album_id" help:"Mount a single album (its UUID). Empty lists all albums."`
	HideLiveVideo bool   `json:"hide_live_video" type:"bool" default:"true" help:"Hide the extra .MOV that Immich stores next to a Live Photo. The still is still listed."`
	ReadOnly      bool   `json:"read_only" type:"bool" default:"false" help:"Refuse upload, mkdir, delete, rename, copy, move."`
	DeviceID      string `json:"device_id" default:"openlist" help:"deviceId sent with uploads. Immich uses deviceId+deviceAssetId to detect duplicates."`
}

var config = driver.Config{
	Name:        "Immich",
	LocalSort:   true,
	DefaultRoot: "root",
	CheckStatus: true,
	OnlyProxy:   true,
	Alert:       "info|Albums are folders. Upload into an album (or All) sends the original file to Immich. Downloads always proxy through OpenList so the API key stays here. Live Photo stills list as one file; the paired video is hidden by default.",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Immich{}
	})
}
