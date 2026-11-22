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

// ResolveDailyAPIEndpoint 返回环境变量覆盖的 Antigravity Daily API 根地址（不带结尾斜杠）；
// 未设置 ANTIGRAVITY_DAILY_API_ENDPOINT 时返回空字符串。
func ResolveDailyAPIEndpoint() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(EnvDailyAPIEndpoint)), "/")
}

// IsOfficialCloudCodeEndpoint 判断目标地址是否指向 Google 官方 Cloud Code 直连域名。
// 当部署环境启用了本机安全中继（ANTIGRAVITY_DAILY_API_ENDPOINT）时，
// 即使凭证中显式写入了官方直连地址，也应强制收口到安全中继，杜绝绕过脱敏防线。
func IsOfficialCloudCodeEndpoint(rawURL string) bool {
	trimmed := strings.ToLower(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	return trimmed == strings.ToLower(DailyAPIEndpoint) ||
		trimmed == strings.ToLower(APIEndpoint) ||
		trimmed == "https://daily-cloudcode-pa.sandbox.googleapis.com"
}

// dailyAPIEndpoint 返回 Daily API 根地址（不带结尾斜杠），优先使用环境变量覆盖值。
func dailyAPIEndpoint() string {
	if v := ResolveDailyAPIEndpoint(); v != "" {
		// NOTE: 调用方用 "%s/%s:method" 拼接，这里去掉结尾斜杠避免出现 "//"。
		return v
	}
	return DailyAPIEndpoint
}
