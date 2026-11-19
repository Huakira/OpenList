package fnosphoto

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
	"github.com/gorilla/websocket"
)

type FnOSPhoto struct {
	model.Storage
	Addition

	// 会话状态
	mu          sync.Mutex // 登录 / 会话状态
	wsWriteMu   sync.Mutex // WebSocket 并发写
	wsConn      *websocket.Conn
	pingCancel  context.CancelFunc
	token       string
	backID      string
	did         string
	trimKey     []byte // 登录 secret 解出的 HMAC key（/upload 的 Trim-Sign 用）
	signEncoded bool   // authx 签名规则：false=RAW（默认），true=ENCODED

	baseURL string // https://host:port（含反代子路径）
	wsURL   string // wss://host:port/websocket?type=main
	client  *resty.Client
}

var _ driver.Driver = (*FnOSPhoto)(nil)
var _ driver.GetRooter = (*FnOSPhoto)(nil)
var _ driver.Put = (*FnOSPhoto)(nil)     // Put 上传
var _ driver.Remove = (*FnOSPhoto)(nil) // Remove 删除（进回收站）
var _ driver.Mkdir = (*FnOSPhoto)(nil)  // MakeDir 建相册

func (d *FnOSPhoto) Config() driver.Config {
	return config
}

func (d *FnOSPhoto) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *FnOSPhoto) Init(ctx context.Context) error {
	if err := d.normalizeAddress(); err != nil {
		return err
	}
	d.client = resty.New().
		SetTimeout(time.Minute).
		SetTLSClientConfig(&tls.Config{InsecureSkipVerify: d.SkipVerify})
	if d.did == "" {
		d.did = randomHex(8) // 固定设备 ID：服务端按它记住「信任此设备」
	}
	d.backID = fnZeroBackID
	d.token = strings.TrimSpace(d.AccessToken)
	if d.token == "" {
		if d.Username == "" || d.Password == "" {
			return errors.New("请填写用户名与密码，或在「AccessToken」里直接填入网页端抓到的 AccessToken")
		}
		if err := d.login(ctx); err != nil {
			return err
		}
	}
	// 探活：顺便完成 authx RAW/ENCODED 签名规则协商
	q := url.Values{
		"offset":         {"0"},
		"limit":          {"1"},
		"sort_by":        {"date_time"},
		"sort_direction": {"desc"},
	}
	var data fnListData[fnAlbum]
	if err := d.request(ctx, "GET", "/p/api/v1/album/list", q, nil, &data); err != nil {
		return fmt.Errorf("飞牛相册接口验证失败（请确认地址可达、账号可登录、相册应用已启用）: %w", err)
	}
	return nil
}

func (d *FnOSPhoto) GetRoot(ctx context.Context) (model.Obj, error) {
	return &model.Object{
		ID:       "root",
		Name:     "root",
		Modified: d.Modified,
		IsFolder: true,
	}, nil
}

func (d *FnOSPhoto) Drop(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopPingLocked()
	d.token = ""
	d.client = nil
	return nil
}

// List 根目录 = 相册列表（+「全部照片」）；相册目录 = 相册内照片（按拍摄时间倒序）
func (d *FnOSPhoto) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	id := dir.GetID()
	if id == "" || id == "root" {
		return d.listRoot(ctx)
	}
	if albumID, ok := strings.CutPrefix(id, "album:"); ok {
		return d.listAlbumPhotos(ctx, albumID)
	}
	if id == "gallery:all" {
		return d.listAllPhotos(ctx)
	}
	return nil, errs.NotSupport
}

// 全部照片虚拟目录（按拍摄时间倒序）
func (d *FnOSPhoto) listAllPhotos(ctx context.Context) ([]model.Obj, error) {
	photos, err := d.listGallery(ctx)
	if err != nil {
		return nil, err
	}
	seenName := make(map[string]int, len(photos))
	objs := make([]model.Obj, 0, len(photos))
	for _, p := range photos {
		objs = append(objs, d.photoToObj(p, seenName))
	}
	return objs, nil
}

func (d *FnOSPhoto) listRoot(ctx context.Context) ([]model.Obj, error) {
	own, err := d.listAlbums(ctx, "/p/api/v1/album/list", url.Values{
		"sort_by":        {"date_time"},
		"sort_direction": {"desc"},
	})
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(own))
	objs := make([]model.Obj, 0, len(own)+16)
	for _, a := range own {
		seen[a.AlbumID] = true
		objs = append(objs, albumToObj(a, d.Modified))
	}
	if d.ShowShared {
		shared, err := d.listAlbums(ctx, "/p/api/v1/album_grant/list_to_me", url.Values{
			"sort_by":        {"share_mod_time"},
			"sort_direction": {"desc"},
		})
		if err != nil {
			return nil, err
		}
		for _, a := range shared {
			if seen[a.AlbumID] {
				continue
			}
			seen[a.AlbumID] = true
			objs = append(objs, albumToObj(a, d.Modified))
		}
	}
	// 「全部照片」虚拟目录（gallery/timeline + getList）
	if d.ShowAll {
		objs = append(objs, &model.Object{
			ID:       "gallery:all",
			Name:     "全部照片",
			Modified: d.Modified,
			IsFolder: true,
		})
	}
	return objs, nil
}

func albumToObj(a fnAlbum, fallbackTime time.Time) model.Obj {
	name := a.AlbumName
	if name == "" {
		name = "未命名相册"
	}
	mod := fallbackTime
	if a.ModifyTime > 0 {
		if t := time.Unix(a.ModifyTime, 0); t.After(time.Unix(0, 0)) {
			mod = t
		}
	}
	return &model.Object{
		ID:       "album:" + int64String(a.AlbumID),
		Name:     name,
		Size:     int64(a.PhotoCount + a.VideoCount),
		Modified: mod,
		IsFolder: true,
	}
}

func (d *FnOSPhoto) listAlbumPhotos(ctx context.Context, albumID string) ([]model.Obj, error) {
	photos, err := d.listPhotos(ctx, albumID)
	if err != nil {
		return nil, err
	}
	seenName := make(map[string]int, len(photos))
	objs := make([]model.Obj, 0, len(photos))
	for _, p := range photos {
		objs = append(objs, d.photoToObj(p, seenName))
	}
	return objs, nil
}

// photoToObj 把照片/视频条目映射为文件对象；
// Path 存放服务器给的媒体直链（相对路径），Link 时直接用
func (d *FnOSPhoto) photoToObj(p fnPhoto, seenName map[string]int) model.Obj {
	name := p.FileName
	if name == "" {
		if strings.EqualFold(p.Category, "video") {
			name = fmt.Sprintf("video_%d.mp4", p.ID)
		} else {
			name = fmt.Sprintf("photo_%d.jpg", p.ID)
		}
	}
	if seenName[name]++; seenName[name] > 1 {
		// 同名去重：加 id 后缀
		ext := filepath.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-" + int64String(p.ID) + ext
	}

	thumb := firstNonEmpty(p.Additional.Thumbnail.MURL, p.Additional.Thumbnail.SURL, p.Additional.Thumbnail.XsURL)
	media := ""
	if strings.EqualFold(p.Category, "video") {
		media = firstNonEmpty(p.Additional.Thumbnail.VideoURL, fmt.Sprintf("/p/api/v1/stream/v/%d", p.ID))
	} else {
		// 实况照片（isLive=1）同样有 videoUrl，但主体仍按照片挂载
		media = firstNonEmpty(p.Additional.Thumbnail.OriginalURL, p.Additional.Thumbnail.MURL)
	}

	return &model.ObjThumb{
		Object: model.Object{
			ID:       int64String(p.ID),
			Name:     name,
			Size:     p.FileSize,
			Modified: parseFnTime(firstNonEmpty(p.DateTime, p.PhotoDateTime)),
			IsFolder: false,
			Path:     media,
		},
		Thumbnail: model.Thumbnail{Thumbnail: joinURL(d.baseURL, thumb)},
	}
}

// Link 媒体直链（需带 AccessToken 头，请开启 Web 代理使用）
func (d *FnOSPhoto) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.IsDir() {
		return nil, errs.NotFile
	}
	media := file.GetPath()
	if media == "" {
		return nil, fmt.Errorf("该文件没有可用的媒体直链，请刷新目录后重试")
	}
	header := http.Header{}
	header.Set("AccessToken", d.token)
	exp := 15 * time.Minute
	return &model.Link{
		URL:        joinURL(d.baseURL, media),
		Header:     header,
		Expiration: &exp,
	}, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func joinURL(base, path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// ---------------------------------------------------------------------------
// 写操作：Put 上传 / Remove 删除（进回收站）/ MakeDir 建相册
// ---------------------------------------------------------------------------

// Put 上传照片/视频：/upload 落盘 + notice 入库；上传进相册目录时再归档进对应相册。
// 历史版本保护：飞牛相册没有版本历史，同名覆盖会直接丢失旧文件，
// 因此目标已存在同名文件时拒绝上传（须先删除或改名）。
func (d *FnOSPhoto) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	if stream.GetExist() != nil {
		return errors.New("飞牛相册没有文件版本历史，不覆盖同名文件：请先删除或改名后再上传")
	}
	fileName := stream.GetName()
	if fileName == "" {
		return errors.New("上传缺少文件名")
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("上传内容为空")
	}
	mtime := stream.ModTime()
	if mtime.IsZero() {
		mtime = time.Now()
	}

	photoID, err := d.uploadPhotoToLibrary(ctx, fileName, data, mtime, "")
	if err != nil {
		return err
	}

	// 上传目标是相册目录：把新照片归档进该相册（此步失败文件已在库中，不回滚）
	parentID := dstDir.GetID()
	if albumID, ok := strings.CutPrefix(parentID, "album:"); ok {
		aid, _ := strconv.ParseInt(albumID, 10, 64)
		pid, _ := strconv.ParseInt(photoID, 10, 64)
		if err := d.addToAlbum(ctx, pid, aid, dstDir.GetName()); err != nil {
			return fmt.Errorf("照片已上传入库，但加入相册「%s」失败: %w", dstDir.GetName(), err)
		}
	}
	return nil
}

// Remove 删除照片：只走回收站（飞牛没有版本历史，回收站是唯一保护，可恢复）
func (d *FnOSPhoto) Remove(ctx context.Context, obj model.Obj) error {
	if obj.IsDir() {
		return errs.NotSupport // 相册删除接口未逆向（album/remove 无现成实现）
	}
	id, err := strconv.ParseInt(obj.GetID(), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("无法识别照片 id（%q），删除中止", obj.GetID())
	}
	return d.recycleAdd(ctx, id)
}

// MakeDir 在根目录创建用户相册
func (d *FnOSPhoto) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	if id := parentDir.GetID(); id != "" && id != "root" {
		return errs.NotSupport // 只支持在根目录建相册
	}
	if dirName == "" {
		return errors.New("相册名不能为空")
	}
	_, err := d.createAlbum(ctx, dirName)
	return err
}
