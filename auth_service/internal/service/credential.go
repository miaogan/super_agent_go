package service

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/aegis/auth_service/internal/oauth2"
	"github.com/aegis/pkg/passwd"
)

// passwordVerify 校验口令。
//
// 使用 pkg/passwd 的共享实现 —— 与 user_service 落库时用的是同一份代码，
// 不存在两边算法漂移导致"存得进、登不上"的可能。
func passwordVerify(stored, plain string) bool {
	return passwd.Verify(stored, plain)
}

// decodeIDToken 解析 id_token 的 payload。
//
// ⚠️ 安全说明：此处**不校验签名**。
// 成立前提是 id_token 刚从 IdP 的 HTTPS token 端点换回，通道可信。
// 若日后 id_token 来自其他来源（如前端直接回传），必须改为
// 拉取 JWKS 并验签，否则可能被伪造。
//
// 保留此路径是因为部分 IdP（如企业微信）不提供 userinfo 接口，
// 只能从 id_token 拿资料。
func decodeIDToken(idToken string) *oauth2.UserInfo {
	if idToken == "" {
		return nil
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 部分实现使用带 padding 的 base64
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}

	info := &oauth2.UserInfo{Raw: raw}
	info.Subject = jsonStr(raw, "sub")
	info.Username = jsonStr(raw, "preferred_username")
	info.Nickname = jsonStr(raw, "name")
	info.Email = jsonStr(raw, "email")
	info.Phone = jsonStr(raw, "phone_number")
	info.Avatar = jsonStr(raw, "picture")

	switch v := raw["roles"].(type) {
	case []any:
		for _, r := range v {
			if s, ok := r.(string); ok && s != "" {
				info.Roles = append(info.Roles, s)
			}
		}
	case string:
		info.Roles = strings.Fields(v)
	}
	return info
}

func jsonStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
