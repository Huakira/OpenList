package icloud_photos

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The bridge (Python, pyicloud) is embedded in the binary and extracted at
// runtime, so the storage only needs an Apple ID: no second service to deploy,
// no address, no token.
//
//go:embed all:bridgefs
var bridgeSource embed.FS

// bridgeProc is one running bridge process, one per data directory. It is kept
// alive across Init calls on purpose: a sign-in that waits for a 2FA code holds
// half-finished state inside the process (see icloud_bridge/login.py).
type bridgeProc struct {
	baseURL string
	token   string
	appleID string
	cmd     *exec.Cmd
	log     *os.File
	exit    chan struct{}
	exited  error
}

var (
	bridgeMu sync.Mutex
	bridges  = map[string]*bridgeProc{} // data dir -> process
)

// startBridgeHook is replaced by tests to point the driver at an httptest
// server instead of spawning Python.
var startBridgeHook func(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error)

func ensureBridge(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error) {
	bridgeMu.Lock()
	defer bridgeMu.Unlock()
	if p := bridges[d.DataDir]; p != nil {
		if p.alive() && p.appleID == d.AppleID {
			return p, nil
		}
		p.stop()
		delete(bridges, d.DataDir)
	}
	start := startBridgeHook
	if start == nil {
		start = startBridge
	}
	p, err := start(ctx, d)
	if err != nil {
		return nil, err
	}
	bridges[d.DataDir] = p
	return p, nil
}

func (p *bridgeProc) alive() bool {
	if p.cmd == nil { // tests
		return true
	}
	select {
	case <-p.exit:
		return false
	default:
		return true
	}
}

func (p *bridgeProc) stop() {
	if p.cmd == nil || !p.alive() {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.exit:
	case <-time.After(5 * time.Second):
	}
	if p.log != nil {
		_ = p.log.Close()
	}
}

func (p *bridgeProc) waitReady(ctx context.Context, logPath string) error {
	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: 3 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		if !p.alive() {
			return fmt.Errorf("内置 bridge 启动失败：%s\n%s", p.exited, tail(logPath, 20))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/health", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+p.token)
			res, err := client.Do(req)
			if err == nil {
				res.Body.Close()
				return nil
			}
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("内置 bridge 60 秒内没有就绪：%v\n%s", last, tail(logPath, 20))
}

func startBridge(ctx context.Context, d *ICloudPhotos) (*bridgeProc, error) {
	if err := os.MkdirAll(d.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	srcDir := filepath.Join(d.DataDir, "bridge")
	if err := extractBridge(srcDir); err != nil {
		return nil, fmt.Errorf("释放内置 bridge 失败: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(d.DataDir, "bridge.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 bridge 日志失败: %w", err)
	}
	cmd := exec.Command(pythonBin(), "-m", "icloud_bridge", "serve",
		"--host", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Dir = d.DataDir
	cmd.Env = bridgeEnv(d, srcDir, token)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("启动内置 bridge 失败（%s）: %w", pythonBin(), err)
	}
	p := &bridgeProc{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		token:   token,
		appleID: d.AppleID,
		cmd:     cmd,
		log:     logFile,
		exit:    make(chan struct{}),
	}
	go func() {
		p.exited = cmd.Wait()
		close(p.exit)
	}()
	if err := p.waitReady(ctx, logPath); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

func bridgeEnv(d *ICloudPhotos, srcDir, token string) []string {
	env := append(os.Environ(),
		"PYTHONPATH="+srcDir,
		"PYTHONUNBUFFERED=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"BRIDGE_TOKEN="+token,
		"BRIDGE_DATA_DIR="+d.DataDir,
		"ICLOUD_APPLE_ID="+d.AppleID,
		"BRIDGE_SHOW_SMART_ALBUMS="+boolStr(d.ShowSmartAlbums),
		"ICLOUD_CHINA="+boolStr(d.China),
	)
	if p := strings.TrimSpace(d.EgressProxy); p != "" {
		env = append(env, "HTTPS_PROXY="+p, "HTTP_PROXY="+p, "NO_PROXY=localhost,127.0.0.1")
	}
	return env
}

func pythonBin() string {
	if v := os.Getenv("OPENLIST_ICLOUD_PYTHON"); v != "" {
		return v
	}
	return "python3"
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("找不到空闲端口: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// extractBridge writes the embedded Python package to dir and skips the work
// when the same source is already there.
func extractBridge(dir string) error {
	files := map[string][]byte{}
	err := fs.WalkDir(bridgeSource, "bridgefs", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "bridgefs"), "/")
		if rel == "" || rel == "README.md" || strings.Contains(rel, "__pycache__") || strings.HasSuffix(rel, ".pyc") {
			return nil
		}
		b, err := bridgeSource.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = b
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("二进制里没有内置 bridge（bridgefs 为空）")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write(files[n])
	}
	sum := hex.EncodeToString(h.Sum(nil))[:16]
	marker := filepath.Join(dir, ".version")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == sum {
		return nil
	}
	for _, n := range names {
		dst := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, files[n], 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, []byte(sum+"\n"), 0o644)
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
