package pixcake

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type PixCake struct {
	model.Storage
	Addition

	token string
	http  *http.Client
	mu    sync.Mutex
	sts   *stsToken
	stsAt time.Time
}

var config = driver.Config{
	Name:        "PixCake Cloud",
	LocalSort:   true,
	OnlyProxy:   false,
	CheckStatus: true,
	// 前端把 "|" 前面当作提示级别，正文查语言包 drivers.config.PixCake Cloud.alert。
	Alert: "warning|Reverse-engineered client API, not an official one. Paste a new token when it expires.",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &PixCake{}
	})
}

func (d *PixCake) Config() driver.Config { return config }

func (d *PixCake) GetAddition() driver.Additional { return &d.Addition }

func (d *PixCake) Init(ctx context.Context) error {
	tok, err := d.resolveToken()
	if err != nil {
		return err
	}
	d.token = tok
	d.http = &http.Client{Timeout: 60 * time.Second}
	if d.APIBase == "" {
		d.APIBase = "https://link-api.pixcakeai.com"
	}
	d.APIBase = strings.TrimRight(d.APIBase, "/")
	if d.SupportAppID == 0 {
		d.SupportAppID = 1
	}
	// 用一个最轻的请求验证 token 是否可用，避免挂载后才在列表页报错。
	var info cloudInfo
	if err := d.request(ctx, http.MethodGet, "/v1/api/cloud/info", nil, nil, &info); err != nil {
		return fmt.Errorf("像素蛋糕鉴权失败：%w", err)
	}
	return nil
}

func (d *PixCake) Drop(ctx context.Context) error {
	d.token = ""
	d.sts = nil
	return nil
}

// resolveToken 支持两种输入：直接的 JWT，或者 tokenV2 + 硬件 UUID。
func (d *PixCake) resolveToken() (string, error) {
	if t := strings.TrimSpace(d.Token); t != "" {
		return strings.TrimPrefix(strings.TrimSpace(t), "Bearer "), nil
	}
	if d.TokenV2 == "" || d.HardwareUUID == "" {
		return "", fmt.Errorf("需要填写 token 或 token_v2 + hardware_uuid")
	}
	pt, err := decryptTokenV2(d.TokenV2, d.HardwareUUID)
	if err != nil {
		return "", err
	}
	return pt, nil
}

// decryptTokenV2 复刻客户端的 LocalTokenCodec::decrypt。
//
// 算法：AES-256-CBC，key = 硬件 UUID 去掉横杠并转大写后的 ASCII，
// 不足 32 字节用字符 '0' 补齐；IV 取 key 的前 16 字节。
func decryptTokenV2(hexCipher, hwUUID string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexCipher))
	if err != nil {
		return "", fmt.Errorf("tokenV2 不是合法十六进制：%w", err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", fmt.Errorf("tokenV2 长度 %d 不是 16 的整数倍", len(raw))
	}
	keyStr := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(hwUUID), "-", ""))
	if keyStr == "" {
		return "", fmt.Errorf("hardware_uuid 为空")
	}
	key := []byte(keyStr)
	if len(key) > 32 {
		key = key[:32]
	}
	for len(key) < 32 {
		key = append(key, '0')
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(out, raw)
	// 去 PKCS7 填充；客户端只做宽松校验，这里同样宽松处理。
	if n := int(out[len(out)-1]); n > 0 && n <= aes.BlockSize && n <= len(out) {
		ok := true
		for _, b := range out[len(out)-n:] {
			if int(b) != n {
				ok = false
				break
			}
		}
		if ok {
			out = out[:len(out)-n]
		}
	}
	tok := strings.TrimSpace(string(out))
	if !strings.HasPrefix(tok, "ey") {
		return "", fmt.Errorf("解密结果不像 JWT，检查 hardware_uuid 是否正确")
	}
	return tok, nil
}

// request 发起一次 API 调用。像素蛋糕的鉴权头是裸 token，不带 Bearer 前缀。
func (d *PixCake) request(ctx context.Context, method, p string, query url.Values, body any, out any) error {
	u := d.APIBase + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", d.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if d.http == nil {
		d.http = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("token 已失效或无权访问（401），请重新获取")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("接口 HTTP %d", resp.StatusCode)
	}
	var wrap apiResp
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("响应不是有效 JSON（HTTP %d）", resp.StatusCode)
	}
	if wrap.Code == nil {
		return fmt.Errorf("接口响应缺少 code 字段")
	}
	if *wrap.Code != 0 {
		msg := wrap.Error
		if msg == "" {
			msg = wrap.Message
		}
		return fmt.Errorf("接口错误 code=%d：%s", *wrap.Code, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(wrap.Data, out)
}

func (d *PixCake) listProjects(ctx context.Context) ([]project, error) {
	var all []project
	for page := 1; page <= 50; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", "50")
		q.Set("support_app_id", strconv.Itoa(d.SupportAppID))
		if d.ShowTeam {
			q.Set("is_team", "0")
		}
		var batch []project
		if err := d.request(ctx, http.MethodGet, "/v1/api/project/list", q, nil, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < 50 {
			break
		}
	}
	out := all[:0]
	for _, p := range all {
		if p.IsDeleted {
			continue
		}
		if d.RootFolderID != "" && strconv.FormatInt(p.ID, 10) != d.RootFolderID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (d *PixCake) listImages(ctx context.Context, projectID int64) ([]image, error) {
	var all []image
	for page := 1; page <= 200; page++ {
		q := url.Values{}
		q.Set("project_id", strconv.FormatInt(projectID, 10))
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", "50")
		q.Set("exclude_delete", "1")
		var batch []image
		if err := d.request(ctx, http.MethodGet, "/v1/api/project_image/list", q, nil, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < 50 {
			break
		}
	}
	return all, nil
}

// projectByID 查单个项目，用于从 ID 还原目录信息。
func (d *PixCake) projectByID(ctx context.Context, id int64) (*project, error) {
	ps, err := d.listProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i], nil
		}
	}
	return nil, fmt.Errorf("项目 %d 不存在或无权访问", id)
}

// imageByID 在项目内按 file_id 找图。
func (d *PixCake) imageByID(ctx context.Context, projectID int64, fileID string) (*image, error) {
	imgs, err := d.listImages(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range imgs {
		if imgs[i].FileID == fileID {
			return &imgs[i], nil
		}
	}
	return nil, fmt.Errorf("图片 %s 不存在", fileID)
}

// uploadToken 缓存 STS，最多十分钟，并在服务端过期时间前一分钟刷新。
func (d *PixCake) uploadToken(ctx context.Context) (*stsToken, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sts != nil && time.Since(d.stsAt) < 10*time.Minute {
		if expiry, err := time.Parse(time.RFC3339, d.sts.Expiration); err == nil && time.Now().Add(time.Minute).Before(expiry) {
			return d.sts, nil
		}
	}
	var sts stsToken
	if err := d.request(ctx, http.MethodGet, "/v2/api/user/sts_token", nil, nil, &sts); err != nil {
		return nil, err
	}
	if sts.AccessKeyID == "" || sts.AccessKeySecret == "" || sts.SecurityToken == "" || sts.Bucket == "" || sts.Prefix == "" {
		return nil, fmt.Errorf("STS 凭证或暂存目录不完整")
	}
	expiry, err := time.Parse(time.RFC3339, sts.Expiration)
	if err != nil || !expiry.After(time.Now().Add(time.Minute)) {
		return nil, fmt.Errorf("STS 有效期无效或即将过期")
	}
	d.sts, d.stsAt = &sts, time.Now()
	return d.sts, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
