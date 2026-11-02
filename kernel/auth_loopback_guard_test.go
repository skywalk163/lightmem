package kernel

import "testing"

// TestIsLoopbackAddr 覆盖启动期安全护栏的判定边界。
//
// 这张表的每一行都对应一次真实误用：":8080" 是最容易被顺手写下的形式，
// 而它等价于 0.0.0.0；把它判成回环就等于护栏失效。
func TestIsLoopbackAddr(t *testing.T) {
	回环 := []string{
		"127.0.0.1:8080",
		"127.0.0.1:18443",
		"127.0.0.1", // 无端口
		"[::1]:8080",
		"localhost:8080",
		"LOCALHOST:18444",
		" 127.0.0.1:8080 ", // 前后空格
	}
	非回环 := []string{
		"",                // 空串
		":8080",           // 全省略接口 = 0.0.0.0
		":18444",
		"0.0.0.0:8080",
		"[::]:8080",       // IPv6 未指定地址
		"172.17.0.1:8080", // 内网
		"192.168.1.5:18443",
		"10.0.10.10:8080",
		"example.com:8080", // 主机名解析与否都不该放行
	}

	for _, a := range 回环 {
		if !IsLoopbackAddr(a) {
			t.Errorf("IsLoopbackAddr(%q) = false，期望 true", a)
		}
	}
	for _, a := range 非回环 {
		if IsLoopbackAddr(a) {
			t.Errorf("IsLoopbackAddr(%q) = true，期望 false（会导致护栏失效）", a)
		}
	}
}

// TestAgentTokenCountZeroWhenUnconfigured 确认「未配置态」被如实报告为 0。
// 护栏依赖这个值：返回非 0 会让非回环启动被放行。
func TestAgentTokenCountZeroWhenUnconfigured(t *testing.T) {
	InitAgentTokens(map[string]AgentIdentity{})
	if n := AgentTokenCount(); n != 0 {
		t.Fatalf("未配置时应为 0，实际 %d", n)
	}
	InitAgentTokens(map[string]AgentIdentity{
		"t-a-0123456789abcdef0123456789abcdef": {AgentID: "a", UserID: "u", TenantID: "t"},
	})
	if n := AgentTokenCount(); n != 1 {
		t.Fatalf("配置 1 条时应为 1，实际 %d", n)
	}
	// 复位，避免影响同包其他用例
	InitAgentTokens(map[string]AgentIdentity{})
}
