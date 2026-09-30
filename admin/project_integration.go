package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ProjectNewAPISummary uses the existing audit binding; neither its secret nor
// the API key leaves this server. The admin supplies a NewAPI origin explicitly.
func (h *Handler) ProjectNewAPISummary(c *gin.Context) {
	var in struct {
		APIKeyID int64  `json:"api_key_id"`
		BaseURL  string `json:"base_url"`
		DayStart int64  `json:"day_start"`
	}
	if c.ShouldBindJSON(&in) != nil || in.APIKeyID <= 0 {
		writeError(c, 400, "需要审计绑定 API Key ID 和 NewAPI 地址")
		return
	}
	u, err := url.Parse(strings.TrimSpace(in.BaseURL))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		writeError(c, 400, "NewAPI 地址必须为不含凭据或查询参数的 HTTP(S) URL")
		return
	}
	if in.DayStart <= 0 || in.DayStart > time.Now().Unix() || time.Now().Unix()-in.DayStart > 27*3600 {
		writeError(c, 400, "统计起点必须在最近 27 小时内")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	binding, err := h.db.GetPromptFilterNewAPIBinding(ctx, in.APIKeyID)
	if err != nil || binding == nil || !binding.Enabled || len(binding.Secret) < 32 {
		writeError(c, 409, "审计绑定不存在、未启用或未配置密钥")
		return
	}
	key, err := h.db.GetAPIKeyByID(ctx, in.APIKeyID)
	if err != nil || key == nil || !key.Enabled {
		writeError(c, 409, "审计绑定对应的 API Key 不可用")
		return
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/integration/codex2api/summary"
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(c, 400, "NewAPI 地址无效")
		return
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		writeInternalError(c, err)
		return
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(key.Key)))
	fields := []string{strconv.FormatInt(time.Now().Unix(), 10), hex.EncodeToString(nonce), binding.PlatformCode, hex.EncodeToString(digest[:]), strconv.FormatInt(in.DayStart, 10)}
	names := []string{"Timestamp", "Nonce", "Platform", "Key-Fingerprint", "Day-Start"}
	for i, name := range names {
		req.Header.Set("X-CPA-"+name, fields[i])
	}
	mac := hmac.New(sha256.New, []byte(binding.Secret))
	mac.Write([]byte("project-summary-v1\nGET\n/api/integration/codex2api/summary\n" + strings.Join(fields, "\n")))
	req.Header.Set("X-CPA-Signature", hex.EncodeToString(mac.Sum(nil)))
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		writeError(c, 502, "无法连接 NewAPI，请检查地址和网络")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message := "NewAPI 汇总接口返回错误，请检查对端版本与审计绑定"
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			message = "NewAPI 拒绝联动签名，请核对平台标识、绑定密钥、API Key 指纹和系统时间"
		}
		c.JSON(502, gin.H{"error": message, "upstream_status": resp.StatusCode})
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || !json.Valid(body) {
		writeError(c, 502, "NewAPI 汇总响应无效")
		return
	}
	var summary struct {
		Version  int               `json:"version"`
		Channels []json.RawMessage `json:"channels"`
	}
	if json.Unmarshal(body, &summary) != nil || summary.Version != 1 || summary.Channels == nil {
		writeError(c, 502, "NewAPI 汇总接口版本不受支持，请更新对端")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(200, "application/json", body)
}
