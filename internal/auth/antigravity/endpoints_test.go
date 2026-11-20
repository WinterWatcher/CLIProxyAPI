package antigravity

import "testing"

func TestUserInfoEndpointDefaultAndOverride(t *testing.T) {
	t.Setenv(EnvUserInfoEndpoint, "")
	if got := userInfoEndpoint(); got != UserInfoEndpoint {
		t.Fatalf("默认值应为官方地址, got %q", got)
	}

	t.Setenv(EnvUserInfoEndpoint, "  http://127.0.0.1:28100/oauth-userinfo  ")
	if got := userInfoEndpoint(); got != "http://127.0.0.1:28100/oauth-userinfo" {
		t.Fatalf("环境变量覆盖值应去除空白, got %q", got)
	}
}

func TestDailyAPIEndpointDefaultAndOverride(t *testing.T) {
	t.Setenv(EnvDailyAPIEndpoint, "")
	if got := dailyAPIEndpoint(); got != DailyAPIEndpoint {
		t.Fatalf("默认值应为官方地址, got %q", got)
	}

	t.Setenv(EnvDailyAPIEndpoint, "http://127.0.0.1:28100/")
	if got := dailyAPIEndpoint(); got != "http://127.0.0.1:28100" {
		t.Fatalf("应去掉结尾斜杠, got %q", got)
	}
}
