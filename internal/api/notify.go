package api

import (
	"context"
	"net/http"
	"time"

	"github.com/asoe/beian2api/internal/notify"
)

// handleNotifyStatus 返回推送通道列表与各自最近一次投递状态。
//
// 不回显 URL 与密钥：这个接口和首页同一个开放级别，机器人地址本身就是凭据
// （拿到就能往你的群里发消息）。只用通道名 + 类型 + 成败来告诉你「通不通」。
func (s *Server) handleNotifyStatus(w http.ResponseWriter, r *http.Request) {
	d := s.notifier
	if d == nil || !d.Enabled() {
		writeOK(w, map[string]any{"enabled": false, "total": 0, "channels": []any{}})
		return
	}
	states := d.States()
	type item struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		Events    []string `json:"events"`
		LastAt    int64    `json:"last_at"`
		LastOK    bool     `json:"last_ok"`
		LastError string   `json:"last_error,omitempty"`
		OK        int      `json:"ok"`
		Failed    int      `json:"failed"`
		Attempts  int      `json:"attempts"`
	}
	out := make([]item, 0, len(d.Channels()))
	for _, ch := range d.Channels() {
		it := item{Name: ch.Name, Kind: ch.Kind, Events: ch.Events}
		if st, ok := states[ch.Name]; ok {
			it.LastAt, it.LastOK, it.LastError = st.LastAt, st.LastOK, st.LastError
			it.OK, it.Failed, it.Attempts = st.OK, st.Failed, st.Attempts
		}
		out = append(out, it)
	}
	writeOK(w, map[string]any{"enabled": true, "total": len(out), "channels": out})
}

// handleNotifyTest 立刻往全部通道发一条测试消息，返回每个通道的真实结果。
//
// 这是配置 Webhook 时唯一能确认「真的收到了」的办法：钉钉/企业微信在
// token 失效、关键词不匹配时也返回 HTTP 200，光看配置写没写对没有意义。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}
	d := s.notifier
	if d == nil || !d.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "notify_disabled", "未配置任何推送通道")
		return
	}
	// 测试消息要同步返回结果，但别让一个卡死的通道拖住请求：
	// 单个通道本身有 10 秒超时 + 有限重试，这里再给整体一个上限。
	// 整体 30 秒上限，但响应的写超时默认也只有 30 秒（且从读完请求头就开始计），
	// 于是「全部通道都慢」时测试结果必然送不回去。留出余量。
	defer extendWriteDeadline(w, 45*time.Second)()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results := d.Send(ctx, notify.Event{Kind: notify.EventTest, At: time.Now().Unix(), BaseURL: s.baseURL})
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	writeOK(w, map[string]any{"total": len(results), "ok": ok, "results": results})
}
