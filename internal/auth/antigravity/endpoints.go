package antigravity

import (
	"os"
	"strings"
)

// 以下环境变量用于在出口网络无法直连 *.googleapis.com 时（例如服务器出口 IP 会被直接 RST），
// 把 OAuth 登录阶段的 Google 请求改走本机转发服务（会借用可连通的前置域名转发）。
// 未设置时保持官方默认地址，对其他部署零影响。
const (
	// EnvUserInfoEndpoint 覆盖获取登录邮箱的 userinfo 地址。
	EnvUserInfoEndpoint = "ANTIGRAVITY_USERINFO_ENDPOINT"
	// EnvDailyAPIEndpoint 覆盖 loadCodeAssist / onboardUser 使用的 Daily API 根地址。
	EnvDailyAPIEndpoint = "ANTIGRAVITY_DAILY_API_ENDPOINT"
)

// userInfoEndpoint 返回 userinfo 请求地址，优先使用环境变量覆盖值。
func userInfoEndpoint() string {
	if v := strings.TrimSpace(os.Getenv(EnvUserInfoEndpoint)); v != "" {
		return v
	}
	return UserInfoEndpoint
}

// dailyAPIEndpoint 返回 Daily API 根地址（不带结尾斜杠），优先使用环境变量覆盖值。
func dailyAPIEndpoint() string {
	if v := strings.TrimSpace(os.Getenv(EnvDailyAPIEndpoint)); v != "" {
		// NOTE: 调用方用 "%s/%s:method" 拼接，这里去掉结尾斜杠避免出现 "//"。
		return strings.TrimRight(v, "/")
	}
	return DailyAPIEndpoint
}
