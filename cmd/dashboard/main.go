// main.go workbuddy2api 独立可视化面板（网关核心保持精简，面板独立进程）。
//
// 设计（与 README「不内嵌 Web 管理面板」一致）：
//   - 网关只新增**数据端点** GET /v1/logs（最近请求流水，内存环形缓冲）；
//   - 本进程负责把网关已有数据端点（/v1/stats、/status、/v1/logs）聚合渲染成一页，
//     静态 HTML 用 go:embed 内嵌，单文件、零第三方依赖、可单独部署。
//
// 服务端代理而非浏览器直连网关：密钥只留在面板进程内（不下发到浏览器）。
//
// 用法:
//
//	go run ./cmd/dashboard                 # 默认 :7864，网关地址取 config.json
//	go run ./cmd/dashboard -listen :9000
//	go run ./cmd/dashboard -gateway http://127.0.0.1:7863
//
// 配置解析与网关侧其它工具一致：读 config.json 的 listen（取端口）与 api_key；
// 可用 WB2A_URL 指定网关地址、WB2A_API_KEY 指定 key、WB2A_CONFIG 指定配置文件路径。
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

func main() {
	var (
		listen   = flag.String("listen", ":7864", "面板监听地址")
		gateway  = flag.String("gateway", "", "网关基址（覆盖 config.json 的 listen），如 http://127.0.0.1:7863")
		timeout  = flag.Duration("timeout", 15*time.Second, "转发到网关的 HTTP 超时")
		showHelp = flag.Bool("h", false, "显示帮助")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [选项]\n\n选项:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showHelp {
		flag.Usage()
		return
	}

	baseURL, apiKey, err := resolveGateway(*gateway)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析网关地址失败: %v\n", err)
		os.Exit(1)
	}

	d := &dashboard{
		gateway: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: *timeout},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", d.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/v1/stats") })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/status") })
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/v1/logs") })

	srv := &http.Server{Addr: *listen, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("workbuddy2api dashboard: http://%s/  →  网关 %s\n", displayAddr(*listen), d.gateway)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "面板启动失败: %v\n", err)
		os.Exit(1)
	}
}

// displayAddr 把监听地址显示成人可点的形式（":7864" → "127.0.0.1:7864"）。
func displayAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", ":":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// dashboard 服务端代理：把前三个网关端点的 JSON 原样转发给浏览器。
type dashboard struct {
	gateway string
	apiKey  string
	client  *http.Client
}

func (d *dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 只服务根路径；其余未匹配路径交给 404（避免把任意路径都当首页）。
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// proxy 转发 GET 到网关，注入鉴权头，原样回传状态码与 body。
//
// 不回传网关的 Content-Type（统一声明 JSON），并加 no-store：面板本身轮询，
// 任何缓存层缓存住都会让页面停在旧数据。上游错误（401/404 等）也原样透出，
// 页面据此显示可操作的提示（如网关版本太旧没有 /v1/logs）。
func (d *dashboard) proxy(w http.ResponseWriter, r *http.Request, path string) {
	target := d.gateway + path
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "构造请求失败: "+err.Error())
		return
	}
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.apiKey)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "连接网关失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 32<<20))
}

func writeProxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// resolveGateway 决定网关地址与 api_key（与 cmd/stats 同口径，刻意各留一份：
// 两者都是独立进程、共享同一份 config.json 约定）。
//
// 优先级（地址）：-gateway flag > WB2A_URL > config.json 的 listen。
// key 来源：WB2A_API_KEY > config.json 的 api_key。
func resolveGateway(gatewayOverride string) (baseURL, apiKey string, err error) {
	cfgPath := os.Getenv("WB2A_CONFIG")
	if cfgPath == "" {
		cfgPath = "config.json"
	}
	raw, rerr := os.ReadFile(cfgPath)
	if rerr == nil {
		var cfg struct {
			Listen string `json:"listen"`
			APIKey string `json:"api_key"`
		}
		if uerr := json.Unmarshal(raw, &cfg); uerr != nil {
			return "", "", fmt.Errorf("解析 %s 失败: %w", cfgPath, uerr)
		}
		apiKey = cfg.APIKey
		if k := os.Getenv("WB2A_API_KEY"); k != "" {
			apiKey = k
		}
		if s := strings.TrimSpace(gatewayOverride); s != "" {
			return strings.TrimRight(s, "/"), apiKey, nil
		}
		if u := os.Getenv("WB2A_URL"); u != "" {
			return strings.TrimRight(u, "/"), apiKey, nil
		}
		return normalizeListen(cfg.Listen), apiKey, nil
	}

	apiKey = os.Getenv("WB2A_API_KEY")
	if s := strings.TrimSpace(gatewayOverride); s != "" {
		return strings.TrimRight(s, "/"), apiKey, nil
	}
	if u := os.Getenv("WB2A_URL"); u != "" {
		return strings.TrimRight(u, "/"), apiKey, nil
	}
	return "", "", fmt.Errorf("读取 %s 失败（可用 -gateway 或 WB2A_URL 直接指定网关地址）: %w", cfgPath, rerr)
}

// normalizeListen 把配置里的 listen 归一成本机可访问的 http 基址。
// 与 cmd/stats 的同名函数逐字一致（监听通配地址时收敛到回环）。
func normalizeListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "http://127.0.0.1:7863"
	}
	host, port := "", ""
	if h, p, err := net.SplitHostPort(listen); err == nil {
		host, port = h, p
	} else {
		if _, convErr := strconv.Atoi(listen); convErr == nil {
			port = listen
		} else {
			host = strings.Trim(strings.TrimSuffix(listen, ":"), "[]")
		}
	}
	if port == "" {
		port = "7863"
	}
	switch host {
	case "", "0.0.0.0", "::", ":":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
