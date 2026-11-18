package app

import (
	"cmp"
	"context"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"slices"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

var panelTmpl = template.Must(template.New("panel").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="2">
<title>magic 状态</title>
<style>
body { font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; margin: 2em; color: #222; }
table { border-collapse: collapse; margin: .5em 0 1.5em; }
th, td { border: 1px solid #ccc; padding: .3em .7em; text-align: left; }
th { background: #f3f3f3; }
.muted { color: #888; }
.err { color: #b00; }
</style>
</head>
<body>
<h1>magic 状态</h1>
<p>本机：{{.Name}}　
{{if .Peer}}状态：已连接 <b>{{.Peer.Name}}</b>（{{.Peer.OS}}）{{else}}状态：<span class="err">未连接</span>{{end}}</p>

{{if .Peer}}<h2>本机虚拟显示器</h2>
<p>位置 ({{.Virtual.Rect.X}}, {{.Virtual.Rect.Y}})，大小 {{.Virtual.Rect.W}}×{{.Virtual.Rect.H}}，
{{if .Virtual.Real}}已创建（窗口可拖到对方屏幕）{{else}}<span class="err">未创建</span>（仅键鼠可跨屏，窗口不能拖过去，详见日志）{{end}}</p>
{{end}}
{{with .Link}}{{if .Connected}}<h2>连接质量</h2>
<table>
<tr><th>输入延迟(KCP)</th><td>{{printf "%.1f" .RTTms}} ms</td></tr>
<tr><th>输入延迟(应用层, 最近30秒最大)</th><td>{{printf "%.1f" .AppRTTms}} ms（最大 {{printf "%.1f" .AppRTTMaxms}} ms）</td></tr>
<tr><th>重传率(全部)</th><td>{{printf "%.1f" .RetransPct}}%</td></tr>
<tr><th>超时重传率(全部)</th><td>{{printf "%.1f" .LostPct}}%</td></tr>
<tr><th>下行(画面/文件)</th><td>{{printf "%.0f" .InKBps}} KB/s</td></tr>
<tr><th>上行(画面/文件)</th><td>{{printf "%.0f" .OutKBps}} KB/s</td></tr>
</table>
{{end}}{{end}}
<h2>本机拖到对方屏幕上的窗口</h2>
{{if not .Spanning}}<p class="muted">无</p>
{{else}}<table>
<tr><th>应用</th><th>标题</th><th>在对方屏幕的位置</th></tr>
{{range .Spanning}}<tr><td>{{.App}}</td><td>{{.Title}}</td><td>{{template "rect" .Rect}}</td></tr>
{{end}}</table>{{end}}

<h2>对方显示在本机的窗口</h2>
{{if not .Views}}<p class="muted">无</p>
{{else}}<table>
<tr><th>窗口</th><th>本机位置</th></tr>
{{range .Views}}<tr><td>{{.Title}}{{if not .Shown}} <span class="muted">（等待画面…）</span>{{end}}</td><td>{{template "rect" .Rect}}</td></tr>
{{end}}</table>{{end}}
</body>
</html>
{{define "rect"}}({{.X}}, {{.Y}}) {{.W}}×{{.H}}{{end}}
`))

// viewStatus describes one peer window shown here, for the status page.
type viewStatus struct {
	ID    uint64
	Title string
	Rect  plat.Rect
	Shown bool // the overlay exists (a first frame arrived)
}

// ServePanel runs the loopback status page until ctx ends.
func (sh *Sharing) ServePanel(ctx context.Context) error {
	ln, err := net.Listen("tcp", sh.cfg.Panel)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           sh.panelHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("panel: http://%s", sh.cfg.Panel)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (sh *Sharing) panelHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", sh.panelIndex)
	return mux
}

func (sh *Sharing) panelIndex(w http.ResponseWriter, r *http.Request) {
	if !sh.panelHostOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	data := struct {
		Name     string
		Peer     *proto.Hello
		Virtual  proto.Virtual
		Link     LinkStats
		Spanning []proto.SpanWindow
		Views    []viewStatus
	}{Name: sh.cfg.Name, Peer: sh.s.Peer(), Virtual: sh.s.Virtual(), Link: sh.s.LinkStats()}
	sh.mu.Lock()
	data.Spanning = slices.Clone(sh.layout)
	rvs := make([]*remoteView, 0, len(sh.views))
	for _, rv := range sh.views {
		rvs = append(rvs, rv)
	}
	sh.mu.Unlock()
	for _, rv := range rvs {
		rv.mu.Lock()
		data.Views = append(data.Views, viewStatus{ID: rv.id, Title: rv.title, Rect: rv.rect, Shown: rv.ov != nil})
		rv.mu.Unlock()
	}
	slices.SortFunc(data.Views, func(a, b viewStatus) int { return cmp.Compare(a.ID, b.ID) })
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := panelTmpl.Execute(w, data); err != nil {
		log.Printf("panel: %v", err)
	}
}

// panelHostOK rejects requests whose Host is not the panel address, which
// defeats DNS rebinding (a foreign name resolving to 127.0.0.1).
func (sh *Sharing) panelHostOK(r *http.Request) bool {
	if r.Host == sh.cfg.Panel {
		return true
	}
	// Accept the actual listen address too (e.g. a ":0" test config or a
	// "localhost" config reached as 127.0.0.1).
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		return r.Host == a.String()
	}
	return false
}
