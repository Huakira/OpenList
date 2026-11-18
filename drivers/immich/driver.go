package immich

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
)

var errReadOnly = errs.NewErr(errs.PermissionDenied, "read-only mode is on for this storage")

type Immich struct {
	model.Storage
	Addition
	client *resty.Client
	http   *http.Client
}

func (d *Immich) Config() driver.Config           { return config }
func (d *Immich) GetAddition() driver.Additional { return &d.Addition }

func (d *Immich) Init(ctx context.Context) error {
	if strings.TrimSpace(d.Address) == "" {
		return fmt.Errorf("请填写 Immich 地址")
	}
	if strings.TrimSpace(d.ApiKey) == "" {
		return fmt.Errorf("请填写 Immich API Key")
	}
	if d.ShowType == "" {
		d.ShowType = "root_only_album"
	}
	d.client = d.newClient()
	d.http = &http.Client{Timeout: 6 * time.Hour}
	if d.AlbumID != "" {
		if _, err := d.getAlbum(ctx, d.AlbumID); err != nil {
			return fmt.Errorf("album_id: %w", err)
		}
		return nil
	}
	if _, err := d.listAlbums(ctx); err != nil {
		return err
	}
	return nil
}

func (d *Immich) Drop(ctx context.Context) error { return nil }

func (d *Immich) GetRoot(ctx context.Context) (model.Obj, error) {
	return &folderObj{Object: model.Object{ID: idRoot, Name: "root", IsFolder: true}}, nil
}

func (d *Immich) writeOK() error {
	if d.ReadOnly {
		return errReadOnly
	}
	return nil
}

func isRoot(obj model.Obj) bool {
	id := obj.GetID()
	return id == "" || id == idRoot
}

func (d *Immich) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if d.AlbumID != "" && isRoot(dir) {
		return d.listAssets(ctx, d.AlbumID)
	}
	if isRoot(dir) {
		return d.listRoot(ctx)
	}
	if dir.GetID() == idAll {
		return d.listAssets(ctx, "")
	}
	return d.listAssets(ctx, dir.GetID())
}

func (d *Immich) listRoot(ctx context.Context) ([]model.Obj, error) {
	var out []model.Obj
	if d.ShowType != "root_only_file" {
		albums, err := d.listAlbums(ctx)
		if err != nil {
			return nil, err
		}
		used := map[string]int{}
		for _, a := range albums {
			name := strings.TrimSpace(a.AlbumName)
			if name == "" {
				name = a.ID
			}
			if _, ok := used[name]; ok {
				name = fmt.Sprintf("%s (%s)", name, shortID(a.ID))
			}
			used[name]++
			out = append(out, a.toObj(name))
		}
		if d.ShowType == "root" {
			out = append([]model.Obj{&folderObj{Object: model.Object{
				ID:       idAll,
				Name:     "All",
				IsFolder: true,
			}}}, out...)
		}
	}
	if d.ShowType != "root_only_album" {
		files, err := d.listAssets(ctx, "")
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

func (d *Immich) listAssets(ctx context.Context, albumID string) ([]model.Obj, error) {
	items, err := d.searchAssets(ctx, albumID)
	if err != nil {
		return nil, err
	}
	hide := map[string]struct{}{}
	if d.HideLiveVideo {
		for _, a := range items {
			if a.LivePhotoVideoId != "" {
				hide[a.LivePhotoVideoId] = struct{}{}
			}
		}
	}
	used := map[string]int{}
	out := make([]model.Obj, 0, len(items))
	for _, a := range items {
		if _, skip := hide[a.ID]; skip {
			continue
		}
		name := strings.TrimSpace(a.OriginalFileName)
		if name == "" {
			name = a.ID
		}
		if _, ok := used[name]; ok {
			name = fmt.Sprintf("%s (%s)", stemName(name), shortID(a.ID))
		}
		used[name]++
		out = append(out, a.toObj(name))
	}
	return out, nil
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func stemName(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return name
	}
	return name[:i]
}

func (d *Immich) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.IsDir() {
		return nil, errs.NotFile
	}
	if args.Type == "thumb" {
		return d.thumbLink(ctx, file.GetID())
	}
	return d.originalLink(ctx, file.GetID(), file.GetSize()), nil
}

func (d *Immich) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	if err := d.writeOK(); err != nil {
		return nil, err
	}
	if d.AlbumID != "" {
		return nil, errs.NewErr(errs.NotSupport, "this storage is pinned to one album")
	}
	if !isRoot(parentDir) {
		return nil, errs.NewErr(errs.NotSupport, "Immich albums cannot be nested")
	}
	a, err := d.createAlbum(ctx, dirName)
	if err != nil {
		return nil, err
	}
	return a.toObj(a.AlbumName), nil
}

func (d *Immich) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if err := d.writeOK(); err != nil {
		return nil, err
	}
	if file.GetSize() == 0 {
		return nil, fmt.Errorf("file size cannot be zero")
	}
	up(0)
	out, err := d.uploadAsset(ctx, file, up)
	if err != nil {
		return nil, err
	}
	albumID := d.AlbumID
	if albumID == "" && !isRoot(dstDir) && dstDir.GetID() != idAll {
		albumID = dstDir.GetID()
	}
	if err := d.addToAlbum(ctx, albumID, []string{out.ID}); err != nil {
		return nil, err
	}
	up(100)
	name := file.GetName()
	return &fileObj{Object: model.Object{
		ID:       out.ID,
		Name:     name,
		Size:     file.GetSize(),
		Modified: file.ModTime(),
	}}, nil
}

func (d *Immich) Remove(ctx context.Context, obj model.Obj) error {
	if err := d.writeOK(); err != nil {
		return err
	}
	if obj.IsDir() {
		if obj.GetID() == idAll {
			return errs.NotSupport
		}
		return d.deleteAlbum(ctx, obj.GetID())
	}
	return d.trashAssets(ctx, []string{obj.GetID()})
}

func (d *Immich) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	if err := d.writeOK(); err != nil {
		return nil, err
	}
	if !srcObj.IsDir() || srcObj.GetID() == idAll {
		return nil, errs.NewErr(errs.NotSupport, "only albums can be renamed")
	}
	a, err := d.renameAlbum(ctx, srcObj.GetID(), newName)
	if err != nil {
		return nil, err
	}
	return a.toObj(a.AlbumName), nil
}

func (d *Immich) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	if err := d.writeOK(); err != nil {
		return err
	}
	if srcObj.IsDir() {
		return errs.NotSupport
	}
	albumID := dstDir.GetID()
	if isRoot(dstDir) || albumID == idAll {
		return errs.NewErr(errs.NotSupport, "copy a photo into an album")
	}
	return d.addToAlbum(ctx, albumID, []string{srcObj.GetID()})
}

func (d *Immich) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	if err := d.writeOK(); err != nil {
		return err
	}
	if srcObj.IsDir() {
		return errs.NotSupport
	}
	// Immich assets do not live in a path; "move" = add to dest album.
	return d.Copy(ctx, srcObj, dstDir)
}

var _ driver.Driver = (*Immich)(nil)
var _ driver.GetRooter = (*Immich)(nil)
var _ driver.MkdirResult = (*Immich)(nil)
var _ driver.PutResult = (*Immich)(nil)
var _ driver.RenameResult = (*Immich)(nil)
var _ driver.Remove = (*Immich)(nil)
var _ driver.Copy = (*Immich)(nil)
var _ driver.Move = (*Immich)(nil)
