package fnosphoto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
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
	envelope, err := buildEnvelope(hello.Pub, payload)
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
func buildEnvelope(pubPEM string, payload map[string]any) (map[string]any, error) {
	aesKey := randomASCII(32)
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher([]byte(aesKey))
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	aesCT := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(aesCT, padded)

	rsaPub, err := parsePublicKey(pubPEM)
	if err != nil {
		return nil, err
	}
	rsaCT, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, []byte(aesKey))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"req": "encrypted",
		"iv":  base64.StdEncoding.EncodeToString(iv),
		"rsa": base64.StdEncoding.EncodeToString(rsaCT),
		"aes": base64.StdEncoding.EncodeToString(aesCT),
	}, nil
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
