package pixcake

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// 目录结构（像素蛋糕的云项目不是真目录，这里做一层投影）：
//
//	/                     -> 所有云项目，一个项目一个文件夹
//	/<项目名>/            -> 项目里的图片
//	/<项目名>/<文件名>    -> 单张图片
//
// 对象 ID 前缀：p 表示项目，f 表示图片，内部再用真实 ID 定位。

const (
	idPrefixProject = "p"
	idPrefixFile    = "f:"
)

func projectIDFromObj(obj model.Obj) (int64, bool) {
	if id, ok := strings.CutPrefix(obj.GetID(), idPrefixProject); ok {
		v, err := strconv.ParseInt(id, 10, 64)
		return v, err == nil
	}
	return 0, false
}

func fileIDFromObj(obj model.Obj) (string, bool) {
	if id, ok := strings.CutPrefix(obj.GetID(), idPrefixFile); ok {
		return id, true
	}
	return "", false
}

func (d *PixCake) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if dir.GetPath() == "" || dir.GetPath() == "/" {
		return d.listRoot(ctx)
	}
	pid, ok := projectIDFromObj(dir)
	if !ok {
		// 兜底：按路径反查项目。
		p, err := d.projectByPath(ctx, dir.GetPath())
		if err != nil {
			return nil, err
		}
		pid = p.ID
	}
	return d.listProject(ctx, pid, dir.GetPath())
}

func (d *PixCake) listRoot(ctx context.Context) ([]model.Obj, error) {
	ps, err := d.listProjects(ctx)
	if err != nil {
		return nil, err
	}
	used := make(map[string]int, len(ps))
	objs := make([]model.Obj, 0, len(ps))
	for _, p := range ps {
		name := sanitizeName(p.Name)
		if name == "" {
			name = strconv.FormatInt(p.ID, 10)
		}
		// 同名项目用 ID 后缀区分，避免路径冲突。
		if _, dup := used[name]; dup {
			name = fmt.Sprintf("%s (%d)", name, p.ID)
		}
		used[name] = 1
		objs = append(objs, &model.Object{
			ID:       idPrefixProject + strconv.FormatInt(p.ID, 10),
			Path:     path.Join("/", name),
			Name:     name,
			IsFolder: true,
			Modified: parseTime(p.UpdatedAt),
			Ctime:    parseTime(p.CreatedAt),
		})
	}
	return objs, nil
}

func (d *PixCake) listProject(ctx context.Context, pid int64, dirPath string) ([]model.Obj, error) {
	imgs, err := d.listImages(ctx, pid)
	if err != nil {
		return nil, err
	}
	used := make(map[string]int, len(imgs))
	objs := make([]model.Obj, 0, len(imgs))
	for _, im := range imgs {
		name := sanitizeName(im.FileName)
		if name == "" {
			name = im.FileID
			if im.FileExt != "" {
				name += "." + im.FileExt
			}
		}
		if _, dup := used[name]; dup {
			name = fmt.Sprintf("%s (%s)", name, short(im.FileID))
		}
		used[name] = 1
		obj := &model.Object{
			ID:       idPrefixFile + im.FileID,
			Path:     path.Join(dirPath, name),
			Name:     name,
			Size:     im.FileSize,
			Modified: parseTime(im.UpdatedAt),
			Ctime:    parseTime(im.CreatedAt),
		}
		objs = append(objs, &thumbObj{Object: obj, thumb: im.OriThumbPath})
	}
	return objs, nil
}

// thumbObj 让列表页直接拿到缩略图，不用每次点开才请求。
type thumbObj struct {
	*model.Object
	thumb string
}

func (o *thumbObj) Thumb() string { return o.thumb }

func (d *PixCake) Get(ctx context.Context, p string) (model.Obj, error) {
	clean := strings.Trim(path.Clean("/"+p), "/")
	if clean == "" || clean == "." {
		return &model.Object{ID: "", Path: "/", Name: "root", IsFolder: true}, nil
	}
	parts := strings.Split(clean, "/")
	proj, err := d.projectByPath(ctx, "/"+parts[0])
	if err != nil {
		return nil, err
	}
	if len(parts) == 1 {
		return &model.Object{
			ID:       idPrefixProject + strconv.FormatInt(proj.ID, 10),
			Path:     "/" + parts[0],
			Name:     parts[0],
			IsFolder: true,
			Modified: parseTime(proj.UpdatedAt),
		}, nil
	}
	// 项目内目前只有一层图片，多级路径直接不支持，避免猜错。
	if len(parts) > 2 {
		return nil, errs.NotSupport
	}
	imgs, err := d.listImages(ctx, proj.ID)
	if err != nil {
		return nil, err
	}
	want := parts[1]
	for _, im := range imgs {
		if sanitizeName(im.FileName) == want || im.FileID == want || short(im.FileID) == want {
			obj := &model.Object{
				ID:       idPrefixFile + im.FileID,
				Path:     "/" + path.Join(parts[0], want),
				Name:     want,
				Size:     im.FileSize,
				Modified: parseTime(im.UpdatedAt),
				Ctime:    parseTime(im.CreatedAt),
			}
			return &thumbObj{Object: obj, thumb: im.OriThumbPath}, nil
		}
	}
	return nil, fmt.Errorf("图片 %s 不存在", want)
}

func (d *PixCake) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	fid, ok := fileIDFromObj(file)
	if !ok {
		return nil, errs.NotSupport
	}
	proj, err := d.projectByPath(ctx, path.Dir(file.GetPath()))
	if err != nil {
		return nil, err
	}
	im, err := d.imageByID(ctx, proj.ID, fid)
	if err != nil {
		return nil, err
	}
	// ori_path 是 OSS 预签名地址，本身带有效期，直接透传。
	if im.OriPath != "" {
		return &model.Link{URL: im.OriPath}, nil
	}
	if im.PreviewPath != "" {
		return &model.Link{URL: im.PreviewPath}, nil
	}
	return nil, fmt.Errorf("该图片没有可用的下载地址")
}

// errReadOnly 是只读模式下所有写操作的统一返回。
var errReadOnly = errs.NewErr(errs.PermissionDenied, "read-only mode is on for this storage")

// MakeDir 在根目录创建云项目；项目内不支持建子目录。
func (d *PixCake) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	if d.ReadOnly {
		return errReadOnly
	}
	if p := parentDir.GetPath(); p != "" && p != "/" {
		return errs.NewErr(errs.NotSupport, "像素蛋糕云项目内部不支持创建子目录")
	}
	name := strings.TrimSpace(dirName)
	if name == "" {
		return fmt.Errorf("项目名不能为空")
	}
	body := map[string]any{"name": name}
	if err := d.request(ctx, http.MethodPost, "/v1/api/project/create", nil, body, nil); err != nil {
		return err
	}
	return nil
}

// Remove 删除图片或整个项目。删除项目会连图片一起删，OpenList 会先递归子项，
// 所以这里对目录直接调项目删除接口即可。
func (d *PixCake) Remove(ctx context.Context, obj model.Obj) error {
	if d.ReadOnly {
		return errReadOnly
	}
	if obj.IsDir() {
		pid, ok := projectIDFromObj(obj)
		if !ok {
			p, err := d.projectByPath(ctx, obj.GetPath())
			if err != nil {
				return err
			}
			pid = p.ID
		}
		body := map[string]any{"ids": []int64{pid}}
		return d.request(ctx, http.MethodPost, "/v1/api/project/delete", nil, body, nil)
	}
	fid, ok := fileIDFromObj(obj)
	if !ok {
		return errs.NotSupport
	}
	proj, err := d.projectByPath(ctx, path.Dir(obj.GetPath()))
	if err != nil {
		return err
	}
	body := map[string]any{
		"project_id": proj.ID,
		"file_ids":   []string{fid},
	}
	return d.request(ctx, http.MethodPost, "/v1/api/project_image/delete", nil, body, nil)
}

// 以下能力像素蛋糕的接口没有提供，明确拒绝而不是静默失败。

func (d *PixCake) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return errs.NewErr(errs.NotSupport, "像素蛋糕没有改项目名/文件名的接口")
}

func (d *PixCake) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NewErr(errs.NotSupport, "像素蛋糕没有移动图片的接口")
}

func (d *PixCake) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NewErr(errs.NotSupport, "像素蛋糕没有复制图片的接口")
}

// projectByPath 从路径第一段反查项目（同名时按后缀 ID 再匹配一次）。
func (d *PixCake) projectByPath(ctx context.Context, p string) (*project, error) {
	seg := strings.Trim(strings.TrimSpace(p), "/")
	if seg == "" {
		return nil, fmt.Errorf("路径为空")
	}
	// 允许 "<名字> (<id>)" 形式。
	if i := strings.LastIndex(seg, "("); i > 0 && strings.HasSuffix(seg, ")") {
		if id, err := strconv.ParseInt(strings.TrimSuffix(seg[i+1:], ")"), 10, 64); err == nil {
			return d.projectByID(ctx, id)
		}
	}
	ps, err := d.listProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if sanitizeName(ps[i].Name) == seg {
			return &ps[i], nil
		}
	}
	// 再按数字 ID 试一次，方便直接 /123456789 访问。
	if id, err := strconv.ParseInt(seg, 10, 64); err == nil {
		for i := range ps {
			if ps[i].ID == id {
				return &ps[i], nil
			}
		}
	}
	return nil, fmt.Errorf("项目 %s 不存在或无权访问", seg)
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	if s == "." || s == ".." {
		return ""
	}
	return s
}

func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

var (
	_ driver.Driver      = (*PixCake)(nil)
	_ driver.Getter      = (*PixCake)(nil)
	_ driver.Mkdir       = (*PixCake)(nil)
	_ driver.Remove      = (*PixCake)(nil)
	_ driver.Rename      = (*PixCake)(nil)
	_ driver.Move        = (*PixCake)(nil)
	_ driver.Copy        = (*PixCake)(nil)
	_ driver.Put         = (*PixCake)(nil)
	_ driver.WithDetails = (*PixCake)(nil)
	_ model.Thumb        = (*thumbObj)(nil)
)
