package fnosphoto

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/gorilla/websocket"
)

const (
	fnSALT   = "NDzZTVxnRKP8Z0jXg1VAMonaG8akvh"
	fnSECRET = "EAECCF25-80A6-4666-A7C2-A76904A74AB6"

	fnZeroBackID = "0000000000000000"
	fnPageSize   = 200
	fnErrBadSign = 5000 // authx 签名规则不对：服务端有两种历史规则，换个模式重试
	fnKeyChars   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// ---------------------------------------------------------------------------
// 登录：WebSocket 加密握手（RSA 公钥 + AES 信封），参考飞牛 Web 前端与 NasPhotoTV 逆向
// ---------------------------------------------------------------------------

// login 加锁登录；已登录则强制重登
func (d *FnOSPhoto) login(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loginLocked(ctx)
}

func (d *FnOSPhoto) loginLocked(ctx context.Context) error {
	d.backID = fnZeroBackID
	d.stopPingLocked()

	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: d.SkipVerify},
		HandshakeTimeout: 15 * time.Second,
	}
	header := http.Header{}
	header.Set("Origin", d.baseURL)
	header.Set("User-Agent", base.UserAgent)
	conn, _, err := dialer.DialContext(ctx, d.wsURL, header)
	if err != nil {
		return fmt.Errorf("连接飞牛 WebSocket 失败（%s）: %w", d.wsURL, err)
	}
	defer func() {
		if d.wsConn != conn {
			_ = conn.Close()
		}
	}()

	// 第一步：取 RSA 公钥与 si
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if err = d.wsWriteJSON(conn, map[string]any{
		"reqid": d.nextReqID(),
		"req":   "util.crypto.getRSAPub",
	}); err != nil {
		return fmt.Errorf("WebSocket 握手发送失败: %w", err)
	}
	var hello wsReply
	if err = conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("WebSocket 握手读取失败: %w", err)
	}
	if hello.Errno != 0 || hello.Pub == "" || hello.Si == "" {
		return fmt.Errorf("获取 RSA 公钥失败 errno=%d msg=%s", hello.Errno, hello.Msg)
	}

	// 第二步：AES 加密登录报文，AES key 用 RSA 加密，装进信封
	payload := map[string]any{
		"req":        "user.login",
		"reqid":      d.nextReqID(),
		"si":         hello.Si, // 必须按字符串原样回传，按数字解析会丢精度（服务端回 errno 8192）
		"user":       d.Username,
		"password":   d.Password,
		"stay":       true,
		"deviceType": "OpenList",
		"deviceName": "OpenList FnOSPhoto",
		"did":        d.did,
	}
	envelope, aesKey, aesIV, err := buildEnvelope(hello.Pub, payload)
	if err != nil {
		return err
	}
	if err = d.wsWriteJSON(conn, envelope); err != nil {
		return fmt.Errorf("发送登录信封失败: %w", err)
	}
	var reply wsReply
	if err = conn.ReadJSON(&reply); err != nil {
		return fmt.Errorf("读取登录响应失败: %w", err)
	}
	if reply.Errno != 0 || reply.Result != "succ" {
		return fmt.Errorf("飞牛登录失败 errno=%d result=%s msg=%s", reply.Errno, reply.Result, reply.Msg)
	}
	if reply.Token == "" {
		return errors.New("登录成功但未返回 AccessToken：账号可能开启了两步验证，" +
			"请在存储配置的「AccessToken」里直接填入网页端抓到的 AccessToken（浏览器 F12 → 任意 /p/api 请求头）")
	}
	d.token = reply.Token
	if len(reply.BackID) == 16 {
		d.backID = reply.BackID
	}
	// secret = base64(AES-CBC(Trim-Sign HMAC key))：解出明文 key 存下来，上传时签 Trim-Sign
	d.trimKey = nil
	if reply.Secret != "" {
		if enc, err := base64.StdEncoding.DecodeString(reply.Secret); err == nil {
			if key, err := aesDecryptCBC(enc, []byte(aesKey), aesIV); err == nil {
				d.trimKey = key
			}
		}
	}

	// 保持 WebSocket 存活并定时 ping：飞牛的会话与 WS 连接绑定，断开会导致 AccessToken 失效
	_ = conn.SetReadDeadline(time.Time{})
	d.wsConn = conn
	d.startPingLocked()
	return nil
}

// startPingLocked 后台每 25 秒 ping 一次；连接断开时清空会话，下次请求自动重登
func (d *FnOSPhoto) startPingLocked() {
	pingCtx, cancel := context.WithCancel(context.Background())
	d.pingCancel = cancel
	conn := d.wsConn
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				d.wsWriteMu.Lock()
				err := conn.WriteJSON(map[string]string{"req": "ping"})
				d.wsWriteMu.Unlock()
				if err != nil {
					d.mu.Lock()
					if d.wsConn == conn {
						d.wsConn = nil
						d.token = ""
					}
					d.mu.Unlock()
					_ = conn.Close()
					return
				}
			}
		}
	}()
}

func (d *FnOSPhoto) stopPingLocked() {
	if d.pingCancel != nil {
		d.pingCancel()
		d.pingCancel = nil
	}
	if d.wsConn != nil {
		_ = d.wsConn.Close()
		d.wsConn = nil
	}
}

// wsWriteJSON 序列化对 WS 的并发写
func (d *FnOSPhoto) wsWriteJSON(conn *websocket.Conn, v any) error {
	d.wsWriteMu.Lock()
	defer d.wsWriteMu.Unlock()
	return conn.WriteJSON(v)
}

// nextReqID 28 位十六进制 = 8 位随机 + 16 位 backId + 4 位随机
func (d *FnOSPhoto) nextReqID() string {
	return randomHex(4) + d.backID + randomHex(2)
}

// buildEnvelope 构造 {req:"encrypted", iv, rsa, aes} 登录信封：
// AES-256-CBC（随机 32 字符 ASCII key + 随机 IV + PKCS7）加密报文，key 再用服务端 RSA 公钥 PKCS#1 加密
// 返回的 aesKey/iv 供登录后解密 secret（Trim-Sign 的 HMAC key）
func buildEnvelope(pubPEM string, payload map[string]any) (map[string]any, string, []byte, error) {
	aesKey := randomASCII(32)
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, "", nil, err
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, "", nil, err
	}
	block, err := aes.NewCipher([]byte(aesKey))
	if err != nil {
		return nil, "", nil, err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	aesCT := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(aesCT, padded)

	rsaPub, err := parsePublicKey(pubPEM)
	if err != nil {
		return nil, "", nil, err
	}
	rsaCT, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, []byte(aesKey))
	if err != nil {
		return nil, "", nil, err
	}
	return map[string]any{
		"req": "encrypted",
		"iv":  base64.StdEncoding.EncodeToString(iv),
		"rsa": base64.StdEncoding.EncodeToString(rsaCT),
		"aes": base64.StdEncoding.EncodeToString(aesCT),
	}, aesKey, iv, nil
}

// ---------------------------------------------------------------------------
// 写操作：上传（/upload + notice）、加相册、建相册、回收站
// （参考鸿蒙客户端 FMphoto jonas-pi/FMphoto 的逆向实现）
// ---------------------------------------------------------------------------

// trimJoinPath 飞牛 Trim-Path 风格的路径拼接
func trimJoinPath(dir, name string) string {
	return strings.TrimSuffix(dir, "/") + "/" + strings.TrimPrefix(name, "/")
}

// encodeTrimPath 逐段 encodeURIComponent（Trim-Path 头的编码方式）
func encodeTrimPath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func hmacSHA256B64(key []byte, data []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// buildMultipartUpload 构造 /upload 的 multipart/form-data 体
func buildMultipartUpload(field, filename, contentType string, data []byte) (body []byte, bodyType string, err error) {
	boundary := "----WebKitFormBoundary" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\nContent-Type: %s\r\n\r\n", boundary, field, filename, contentType)
	buf.Write(data)
	fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)
	return buf.Bytes(), "multipart/form-data; boundary=" + boundary, nil
}

// mimeByExt 上传体的 Content-Type（按扩展名粗略）
func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".webp":
		return "image/webp"
	case ".mp4", ".mov":
		return "video/quicktime"
	default:
		return "application/octet-stream"
	}
}

// firstIDFromJSON 从上传/notice 响应里尽力提取新照片 id（id/photoId/ids[0]/list[0]，兼容 data 包裹）
func firstIDFromJSON(raw []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	pick := func(m map[string]json.RawMessage) string {
		for _, k := range []string{"id", "photoId", "fileId", "itemId"} {
			if v, ok := m[k]; ok && len(v) > 0 && v[0] == '"' {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
		for _, k := range []string{"ids", "photoIds", "list", "itemIds"} {
			if v, ok := m[k]; ok {
				var arr []any
				if json.Unmarshal(v, &arr) == nil && len(arr) > 0 {
					s := fmt.Sprint(arr[0])
					if s != "" && s != "<nil>" {
						return s
					}
				}
			}
		}
		return ""
	}
	if s := pick(top); s != "" {
		return s
	}
	if v, ok := top["data"]; ok {
		var dm map[string]json.RawMessage
		if json.Unmarshal(v, &dm) == nil {
			if s := pick(dm); s != "" {
				return s
			}
		}
	}
	return ""
}

// firstPathFromJSON 同上，提取落盘路径提示
func firstPathFromJSON(raw []byte) string {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	lookup := func(m map[string]any) string {
		for _, k := range []string{"path", "dir", "upload_path", "uploadPath", "targetPath", "file"} {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	if s := lookup(top); s != "" {
		return s
	}
	if dm, ok := top["data"].(map[string]any); ok {
		return lookup(dm)
	}
	return ""
}

// requestUploadPathHint 拿默认落盘中转路径（不同版本只实现 v1/v2 其一，参数名不同版本不一）
func (d *FnOSPhoto) requestUploadPathHint(ctx context.Context, fileName string) string {
	cands := []url.Values{{}, {"name": {fileName}}, {"fileName": {fileName}}, {"filename": {fileName}}}
	for _, base := range []string{"/p/api/v2/photo/upload/path", "/p/api/v1/photo/upload/path"} {
		for _, q := range cands {
			var out map[string]any
			if err := d.request(ctx, "GET", base, q, nil, &out); err != nil {
				continue
			}
			b, _ := json.Marshal(out)
			if p := firstPathFromJSON(b); p != "" {
				return p
			}
		}
	}
	return ""
}

// uploadPhotoToLibrary 两步上传：/upload 落盘 + photo/upload/notice 入库，返回新照片 id
// dirHint 为落盘中转目录（.<备份位置>/.@#local 之类），空则用服务端默认
func (d *FnOSPhoto) uploadPhotoToLibrary(ctx context.Context, fileName string, data []byte, mtime time.Time, dirHint string) (photoID string, err error) {
	if dirHint == "" {
		dirHint = d.requestUploadPathHint(ctx, fileName)
	}
	if dirHint == "" {
		return "", errors.New("未能获取上传落盘目录（photo/upload/path 无返回）")
	}
	fullPath := trimJoinPath(dirHint, fileName)
	encPath := encodeTrimPath(fullPath)
	if os.Getenv("FNOS_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[dbg] dirHint=%q fullPath=%q enc=%q\n", dirHint, fullPath, encPath)
	}
	mtim := strconv.FormatInt(mtime.Unix(), 10)
	body, bodyType, err := buildMultipartUpload("trim-upload-file", fileName, mimeByExt(fileName), data)
	if err != nil {
		return "", err
	}

	// Trim-Sign 候选：登录 secret 的 HMAC 优先，token 作兼容候选，空签名兜底
	signs := make([]string, 0, 3)
	if len(d.trimKey) > 0 {
		signs = append(signs, hmacSHA256B64(d.trimKey, []byte(encPath)))
	}
	signs = append(signs, hmacSHA256B64([]byte(d.token), []byte(encPath)))
	signs = append(signs, "")

	var uploadRaw []byte
	uploaded := false
	for _, sign := range signs {
		req := d.client.R().SetContext(ctx).
			SetHeader("Content-Type", bodyType).
			SetHeader("Trim-Path", encPath).
			SetHeader("Trim-Sign", sign).
			SetHeader("Trim-Mtim", mtim).
			SetHeader("Cookie", "fnos-token="+d.token+"; language=zh-CN").
			SetBody(body)
		resp, err := req.Execute("POST", d.baseURL+"/upload")
		if err != nil {
			return "", err
		}
		if resp.StatusCode()/100 != 2 {
			continue
		}
		uploadRaw = resp.Body()
		var shell fnBaseResp
		if json.Unmarshal(uploadRaw, &shell) == nil && shell.code() != 0 {
			continue // 签名不对，换下一个候选
		}
		if os.Getenv("FNOS_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[dbg] /upload http=%d raw=%s\n", resp.StatusCode(), truncateBytes(resp.Body(), 300))
		}
		uploaded = true
		break
	}
	if !uploaded {
		return "", errors.New("文件上传落盘失败（/upload 未接受，Trim-Sign 候选均未通过）")
	}

	// notice 入库：多候选 body 依次尝试（不同版本字段差异，参考 FMphoto）
	fc := time.Unix(mtime.Unix(), 0).Format("2006-01-02 15:04:05")
	// 归档策略字段：notice 缺了它们服务端只回执不入库（FMphoto 同样必带）
	// folder_manage_mode: 1 默认归档 / rename_strategy: 0 不改名
	policy := func(m map[string]any) map[string]any {
		m["folder_manage_mode"] = 1
		m["rename_strategy"] = 0
		return m
	}
	// 实测（2026-11-19）：/upload 落盘后文件在暂存目录里带 .~#N 后缀（N 从 0 起，
	// 同名重传 N 递增），notice 的 path/file_name 必须用真实暂存名；
	// 用不带后缀的原名只会得到 error:"system busy"。
	// /upload 响应体为空拿不到暂存名，按 .~#0..~#9 依次探测。
	noticeCandidates := make([]any, 0, 12)
	for n := 0; n <= 9; n++ {
		stagedName := fmt.Sprintf("%s.~#%d", fileName, n)
		stagedPath := trimJoinPath(dirHint, stagedName)
		noticeCandidates = append(noticeCandidates, policy(map[string]any{"files": []any{map[string]any{
			"path": stagedPath, "file": stagedPath,
			"file_create_time": fc, "fileCreateTime": fc,
			"original_name": fileName, "originalName": fileName,
			"file_name": stagedName, "fileName": stagedName,
			"file_size": len(data), "fileSize": len(data),
		}}}))
	}
	// 兜底：不带后缀的原名 + 纯路径数组
	noticeCandidates = append(noticeCandidates,
		map[string]any{"files": []any{fullPath}},
		map[string]any{"files": []any{map[string]any{
			"file_create_time": fc, "fileCreateTime": fc,
			"original_name": fileName, "originalName": fileName,
			"file_name": fileName, "fileName": fileName,
			"file_size": len(data), "fileSize": len(data),
		}}})
	dup := false
	// notice 结果在 data.list[] 里：error=="system busy" 要退避重试（FMphoto 同款策略），
	// id!=0 才是真正入库成功；repeat=true 表示照片已在库。
	busyWaits := []time.Duration{800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 4800 * time.Millisecond}
	for _, cand := range noticeCandidates {
		for attempt := 0; ; attempt++ {
			var noticeData map[string]any
			err := d.request(ctx, "POST", "/p/api/v1/photo/upload/notice", nil, cand, &noticeData)
			if err != nil {
				if strings.Contains(err.Error(), "已存在") || strings.Contains(err.Error(), "exist") {
					dup = true // 重复上传：照片已在库，不算失败
				} else if os.Getenv("FNOS_DEBUG") != "" {
					fmt.Fprintf(os.Stderr, "[dbg] notice cand err: %v\n", err)
				}
				break // 换下一个候选 body
			}
			busy, gotID, repeated := parseNoticeResult(noticeData)
			if os.Getenv("FNOS_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[dbg] notice data=%v busy=%v id=%q repeat=%v\n", noticeData, busy, gotID, repeated)
			}
			if repeated {
				dup = true
				if gotID != "" {
					return gotID, nil // 重复内容：用服务端返回的已有照片 id
				}
				break
			}
			if gotID != "" {
				return gotID, nil
			}
			if !busy {
				break // 明确失败（非 busy），换下一个候选 body
			}
			if attempt >= len(busyWaits) {
				break // 重试次数用尽
			}
			select {
			case <-time.After(busyWaits[attempt]):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	// notice 未直接成功：从 /upload 响应里找 id 兜底
	if id := firstIDFromJSON(uploadRaw); id != "" {
		return id, nil
	}
	if dup {
		return "", nil
	}
	return "", errors.New("上传完成但入库通知失败（notice），可稍后重试或等待飞牛自动扫盘")
}

// addToAlbum 把已入库的照片加进用户相册（ids/album_id 必须数字，否则 HTTP 400）
func (d *FnOSPhoto) addToAlbum(ctx context.Context, photoID, albumID int64, albumName string) error {
	if photoID <= 0 || albumID <= 0 {
		return fmt.Errorf("相册归档失败：缺少照片/相册 id（photo=%d album=%d）", photoID, albumID)
	}
	return d.request(ctx, "POST", "/p/api/v1/gallery/addTo/album", nil,
		map[string]any{"ids": []int64{photoID}, "album_id": albumID, "album_name": albumName}, nil)
}

// createAlbum 新建用户相册，返回新相册 id（响应不带 id，创建后按名字反查）
func (d *FnOSPhoto) createAlbum(ctx context.Context, name string) (int64, error) {
	err := d.request(ctx, "POST", "/p/api/v1/album/create", nil,
		map[string]any{"ids": []int64{}, "category": 0, "displayType": 1, "album_name": name}, nil)
	if err != nil && !strings.Contains(err.Error(), "4014") { // 4014=同名已存在，复用即可
		return 0, err
	}
	// 按名字反查 id
	for offset := 0; offset < 5000; offset += 100 {
		var data fnListData[fnAlbum]
		q := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {"100"},
			"sort_by": {"date_time"}, "sort_direction": {"desc"}}
		if err := d.request(ctx, "GET", "/p/api/v1/album/list", q, nil, &data); err != nil {
			return 0, err
		}
		for _, a := range data.List {
			if a.AlbumName == name {
				return a.AlbumID, nil
			}
		}
		if len(data.List) < 100 {
			break
		}
	}
	return 0, fmt.Errorf("相册 %q 创建后未在列表中找到", name)
}

// recycleAdd 把照片移入回收站（多候选 body，不同版本字段不一）
// 注意：飞牛相册没有版本历史，回收站是唯一的删除保护，驱动不用永久删除接口
func (d *FnOSPhoto) recycleAdd(ctx context.Context, photoID int64) error {
	idStr := strconv.FormatInt(photoID, 10)
	cands := []any{
		map[string]any{"ids": []int64{photoID}},
		map[string]any{"ids": []string{idStr}},
		map[string]any{"id": idStr},
		map[string]any{"photoId": idStr},
	}
	var lastErr error
	for _, c := range cands {
		if err := d.request(ctx, "POST", "/p/api/v1/recycle-bin/add", nil, c, nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// parseNoticeResult 解析 notice 的 data：list[] 里 error=="system busy" 表示要重试，
// id!=0 表示入库成功（返回字符串 id）；repeat=true 表示照片已在库。
func parseNoticeResult(data map[string]any) (busy bool, id string, repeated bool) {
	list, ok := data["list"].([]any)
	if !ok {
		// 没有 list 结构：壳层 code==0 就当成功
		return false, "", false
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if e, _ := m["error"].(string); e == "system busy" {
			busy = true
			continue
		}
		if e, _ := m["error"].(string); e != "" {
			continue // 其他错误：换候选 body
		}
		if r, _ := m["repeat"].(bool); r {
			// 重复上传：内容已在库，服务端会带已有照片的 id
			repeated = true
			switch v := m["id"].(type) {
			case float64:
				if v > 0 {
					id = strconv.FormatInt(int64(v), 10)
				}
			case string:
				if v != "" && v != "0" {
					id = v
				}
			}
			continue
		}
		switch v := m["id"].(type) {
		case float64:
			if v > 0 {
				id = strconv.FormatInt(int64(v), 10)
			}
		case string:
			if v != "" && v != "0" {
				id = v
			}
		}
	}
	return busy, id, repeated
}

// aesDecryptCBC AES-CBC 解密去 PKCS7（解登录 secret 用）
func aesDecryptCBC(ct, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("AES 密文长度非法")
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(pt) {
		return nil, errors.New("PKCS7 填充非法")
	}
	return pt[:len(pt)-pad], nil
}

func parsePublicKey(pubPEM string) (*rsa.PublicKey, error) {
	body := strings.ReplaceAll(pubPEM, "-----BEGIN PUBLIC KEY-----", "")
	body = strings.ReplaceAll(body, "-----END PUBLIC KEY-----", "")
	body = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, body)
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		// 有些版本直接给 base64 裸公钥
		raw2, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(pubPEM))
		if err2 != nil {
			return nil, fmt.Errorf("RSA 公钥解析失败: %w", err)
		}
		raw = raw2
	}
	if block, _ := pem.Decode(raw); block != nil {
		raw = block.Bytes
	}
	pub, err := x509.ParsePKIXPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("RSA 公钥解析失败: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("不是 RSA 公钥")
	}
	return rsaPub, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytesRepeat(byte(pad), pad)...)
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// ---------------------------------------------------------------------------
// 「全部照片」：gallery/timeline 按天统计 + gallery/getList 分页拉取
// （参考飞牛 Web 图库与 NasPhotoTV 的 FnOsGallery：跨日翻页可能不完整，
//  按年拉取不够时退回官方 Web 使用的单日范围逐日补齐）
// ---------------------------------------------------------------------------

const fnGalleryPageSize = 500

// galleryTimeline 全库按天的照片数量统计
func (d *FnOSPhoto) galleryTimeline(ctx context.Context) ([]galleryDay, error) {
	var data fnListData[galleryDay]
	if err := d.request(ctx, "GET", "/p/api/v1/gallery/timeline", url.Values{}, nil, &data); err != nil {
		return nil, err
	}
	out := make([]galleryDay, 0, len(data.List))
	for _, day := range data.List {
		if day.Year > 0 && day.ItemCount > 0 {
			out = append(out, day)
		}
	}
	return out, nil
}

// galleryRange 拉取一段日期范围内的全部媒体，offset 翻页写入 collected（按 ID 去重）。
// 服务端无 total/hasNext，可能把 limit 压小：非空页且有新增就继续，直到空页/读满/整页重复。
func (d *FnOSPhoto) galleryRange(ctx context.Context, start, end time.Time, expectedTotal int64, collected map[int64]fnPhoto) error {
	offset := 0
	for {
		q := url.Values{
			"start_time": {start.Format("2006:01:02") + " 00:00:00"},
			"end_time":   {end.Format("2006:01:02") + " 23:59:59"},
			"limit":      {strconv.Itoa(fnGalleryPageSize)},
			"offset":     {strconv.Itoa(offset)},
			"mode":       {"index"},
		}
		var data fnListData[fnPhoto]
		if err := d.request(ctx, "GET", "/p/api/v1/gallery/getList", q, nil, &data); err != nil {
			return err
		}
		if len(data.List) == 0 {
			return nil
		}
		before := len(collected)
		for _, p := range data.List {
			if p.ID > 0 {
				collected[p.ID] = p
			}
		}
		offset += len(data.List) // 解析失败的条目也占服务端位置，offset 不回退
		if int64(len(collected)) >= expectedTotal {
			return nil
		}
		if len(collected) == before {
			return nil // 整页重复，视为到头
		}
	}
}

// listGallery 「全部照片」：时间线取全量天数 → 按年拉取；某年不足则退回逐日补齐
func (d *FnOSPhoto) listGallery(ctx context.Context) ([]fnPhoto, error) {
	days, err := d.galleryTimeline(ctx)
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return []fnPhoto{}, nil
	}
	yearDays := make(map[int][]galleryDay)
	for _, day := range days {
		yearDays[day.Year] = append(yearDays[day.Year], day)
	}
	years := make([]int, 0, len(yearDays))
	for y := range yearDays {
		years = append(years, y)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))

	collected := make(map[int64]fnPhoto, len(days)*4)
	for _, year := range years {
		daysInYear := yearDays[year]
		expected := 0
		for _, day := range daysInYear {
			expected += day.ItemCount
		}
		before := len(collected)
		yearStart := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local)
		yearEnd := time.Date(year, 12, 31, 0, 0, 0, 0, time.Local)
		_ = d.galleryRange(ctx, yearStart, yearEnd, int64(before+expected), collected)
		if len(collected)-before < expected {
			// 跨年范围翻页不完整：退回官方 Web 端的单日协议，逐日补齐
			for _, day := range daysInYear {
				dayStart := time.Date(day.Year, time.Month(day.Month), day.Day, 0, 0, 0, 0, time.Local)
				if err := d.galleryRange(ctx, dayStart, dayStart, int64(day.ItemCount), collected); err != nil {
					return nil, fmt.Errorf("拉取 %04d:%02d:%02d 照片失败: %w", day.Year, day.Month, day.Day, err)
				}
			}
		}
	}
	out := make([]fnPhoto, 0, len(collected))
	for _, p := range collected {
		out = append(out, p)
	}
	// 按拍摄时间倒序
	sort.Slice(out, func(i, j int) bool {
		return parseFnTime(firstNonEmpty(out[i].DateTime, out[i].PhotoDateTime)).After(
			parseFnTime(firstNonEmpty(out[j].DateTime, out[j].PhotoDateTime)))
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP 业务请求：AccessToken + authx 签名
// ---------------------------------------------------------------------------

// authxHeader sign = md5(SALT_path_nonce_timestamp_md5(参数串)_SECRET)
// path 必须是飞牛内部 /p/api/... 原路径；timestamp 毫秒；GET 参数按 key 排序
func (d *FnOSPhoto) authxHeader(path, payload string) string {
	nonce := strconv.Itoa(100000 + mrand.Intn(900000))
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	h := md5.New()
	_, _ = io.WriteString(h, fnSALT+"_"+path+"_"+nonce+"_"+timestamp+"_"+md5Hex(payload)+"_"+fnSECRET)
	return "nonce=" + nonce + "&timestamp=" + timestamp + "&sign=" + hex.EncodeToString(h.Sum(nil))
}

// signPayload GET 参数按 key 排序拼串。
// 有 RAW / ENCODED 两种历史签名规则（不同 fnOS 版本服务端不同），默认 RAW，
// 收到业务码 5000 时自动换规则重试。
func signPayload(query url.Values, encoded bool) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := query.Get(k)
		if encoded {
			k = url.QueryEscape(k)
			v = url.QueryEscape(v)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "&")
}

// request 发起一次带鉴权的业务请求，内置 401 重登与签名规则协商
func (d *FnOSPhoto) request(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if d.token == "" {
			if err := d.login(ctx); err != nil {
				return err
			}
		}
		fullURL := d.baseURL + path
		if len(query) > 0 {
			fullURL += "?" + query.Encode()
		}
		var bodyText string
		if method != "GET" && body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			bodyText = string(b)
		}
		req := d.client.R().SetContext(ctx).
			SetHeader("AccessToken", d.token).
			SetHeader("authx", d.authxHeader(path, d.signPayload(query, bodyText)))
		if bodyText != "" {
			req = req.SetBody(bodyText).SetHeader("Content-Type", "application/json")
		}
		resp, err := req.Execute(method, fullURL)
		if err != nil {
			return err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			// AccessToken 失效：清空后强制重登（下一轮循环处理）
			d.mu.Lock()
			d.token = ""
			d.stopPingLocked()
			d.mu.Unlock()
			lastErr = fmt.Errorf("AccessToken 失效（HTTP 401）")
			continue
		}
		var fr fnBaseResp
		if err := json.Unmarshal(resp.Body(), &fr); err != nil {
			return fmt.Errorf("飞牛返回非 JSON（HTTP %d）: %s", resp.StatusCode(), truncateBytes(resp.Body(), 200))
		}
		if fr.code() == fnErrBadSign {
			d.signEncoded = !d.signEncoded
			lastErr = fmt.Errorf("authx 签名规则不匹配（code 5000），已切换为 %s 模式重试", signModeName(d.signEncoded))
			continue
		}
		if fr.code() != 0 {
			return fmt.Errorf("飞牛接口错误 code=%d: %s", fr.code(), fr.message())
		}
		if out != nil && len(fr.Data) > 0 {
			if err := json.Unmarshal(fr.Data, out); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("请求 %s 失败: %w", path, lastErr)
}

// signPayload 计算参与 authx 签名的参数串：GET 用排序后的查询参数，POST 用实际发送的 JSON 字节串
func (d *FnOSPhoto) signPayload(query url.Values, bodyText string) string {
	if len(query) > 0 {
		return signPayload(query, d.signEncoded)
	}
	return bodyText
}

func signModeName(encoded bool) string {
	if encoded {
		return "ENCODED"
	}
	return "RAW"
}

// ---------------------------------------------------------------------------
// 列表拉取
// ---------------------------------------------------------------------------

// listAlbums 分页拉取相册列表（album/list 或 album_grant/list_to_me）
func (d *FnOSPhoto) listAlbums(ctx context.Context, path string, base url.Values) ([]fnAlbum, error) {
	out := make([]fnAlbum, 0, 32)
	seen := make(map[int64]bool)
	offset := 0
	for {
		q := cloneValues(base)
		q.Set("offset", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(fnPageSize))
		var data fnListData[fnAlbum]
		if err := d.request(ctx, "GET", path, q, nil, &data); err != nil {
			return nil, err
		}
		added := 0
		for _, a := range data.List {
			if a.AlbumID <= 0 || seen[a.AlbumID] {
				continue
			}
			seen[a.AlbumID] = true
			out = append(out, a)
			added++
		}
		if len(data.List) == 0 || added == 0 {
			break
		}
		if data.Total > 0 && int64(len(out)) >= data.Total {
			break
		}
		offset += len(data.List)
		if offset > 10000 { // 兜底
			break
		}
	}
	return out, nil
}

// listPhotos 分页拉取相册内照片（album/photos）
func (d *FnOSPhoto) listPhotos(ctx context.Context, albumID string) ([]fnPhoto, error) {
	out := make([]fnPhoto, 0, fnPageSize)
	seen := make(map[int64]bool)
	offset := 0
	for {
		q := url.Values{
			"album_id":       {albumID},
			"sort_by":        {"date_time"},
			"sort_direction": {"desc"},
			"offset":         {strconv.Itoa(offset)},
			"limit":          {strconv.Itoa(fnPageSize)},
		}
		var data fnListData[fnPhoto]
		if err := d.request(ctx, "GET", "/p/api/v1/album/photos", q, nil, &data); err != nil {
			return nil, err
		}
		added := 0
		for _, p := range data.List {
			if p.ID <= 0 || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			out = append(out, p)
			added++
		}
		// 部分版本不返回 total 或 total 恒为 0，只能以「空页/无新增」为终点
		if len(data.List) == 0 || added == 0 {
			break
		}
		if data.Total > 0 && int64(len(out)) >= data.Total {
			break
		}
		offset += len(data.List)
		if offset > 100000 { // 兜底上限 500 页
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func (d *FnOSPhoto) normalizeAddress() error {
	addr := strings.TrimSpace(d.Address)
	if addr == "" {
		return errors.New("address 不能为空")
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return fmt.Errorf("address 解析失败: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("address 里没有主机名")
	}
	// 端口规则：显式写了端口就用它；带协议但没端口（https://domain）说明走 443 反代，
	// 用协议默认端口；裸 IP 不带协议不带端口（内网直连）才补飞牛默认的 5667。
	explicitScheme := strings.Contains(d.Address, "://")
	port := u.Port()
	if port == "" && !explicitScheme {
		port = "5667"
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}
	basePath := strings.TrimSuffix(u.Path, "/")
	d.baseURL = u.Scheme + "://" + hostPort + basePath
	wsScheme := "wss"
	if u.Scheme == "http" {
		wsScheme = "ws"
	}
	d.wsURL = wsScheme + "://" + hostPort + basePath + "/websocket?type=main"
	return nil
}

var fnTimeLayouts = []string{
	"2006:01:02 15:04:05",
	"2006-01-02 15:04:05",
	time.RFC3339,
}

func parseFnTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range fnTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(byteCount int) string {
	b := make([]byte, byteCount)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

func randomASCII(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = fnKeyChars[mrand.Intn(len(fnKeyChars))]
	}
	return string(b)
}

func cloneValues(src url.Values) url.Values {
	dst := make(url.Values, len(src)+4)
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// 大整数 ID 转字符串（部分接口 id 超过 int32）
func int64String(n int64) string {
	return strconv.FormatInt(n, 10)
}
