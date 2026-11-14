package api

import (
	"net/http"

	"github.com/asoe/beian2api/internal/icpcode"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/service"
)

// handleVerify 校验备案号本身是否成立。
//
// 与查询接口互补：查询告诉你"这个域名有没有备案"，校验告诉你
// "这个备案号看着像不像真的"。两者结合就能识别伪造备案号的小作坊网站。
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		code = r.URL.Query().Get("icp")
	}
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing_code", "缺少 code 参数，例如 ?code=京ICP备12345678号-1")
		return
	}
	info := icpcode.Parse(code)

	// 若同时给了域名，顺手核对两者是否匹配。
	var crossCheck map[string]any
	if domain := r.URL.Query().Get("domain"); domain != "" {
		res, err := s.svc.Query(r.Context(), domain, service.Options{})
		if err != nil {
			crossCheck = map[string]any{"checked": false, "reason": err.Error()}
		} else if !res.Found || len(res.Records) == 0 {
			crossCheck = map[string]any{"checked": true, "match": false, "reason": "该域名没有备案记录"}
		} else {
			p, _ := model.Primary(res.Records)
			actual := p.ICPCode
			crossCheck = map[string]any{
				"checked":  true,
				"match":    icpcode.Same(info.Normalized, actual),
				"actual":   actual,
				"verified": info.Valid,
			}
		}
	}

	writeOK(w, map[string]any{
		"input":       code,
		"valid":       info.Valid,
		"normalized":  info.Normalized,
		"province":    info.Province,
		"region":      info.Region,
		"kind":        info.Kind,
		"serial":      info.Serial,
		"site_seq":    info.SiteSeq,
		"issues":      info.Issues,
		"fixed":       info.Fixed,
		"cross_check": crossCheck,
	})
}
