package policy_test

import (
	"strings"
	"testing"

	"opsagent/internal/policy"
)

func TestSanitizeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bad  string // 不应再出现在输出中的敏感片段
	}{
		{"火山方舟 Key", "calling with ark-71734550-1dc6-459c-a8eb-ac7851858116 key", "71734550"},
		{"OpenAI 风格 Key", "auth sk-abcdef123456 now", "abcdef123456"},
		{"Bearer Token", "Authorization: Bearer abc.def-ghi~jkl/123", "abc.def-ghi~jkl/123"},
		{"password 赋值", `password=sup3rsecret!`, "sup3rsecret"},
		{"token 冒号赋值", "Token: abc123secret", "abc123secret"},
		{"AWS Key", "access AKIAIOSFODNN7EXAMPLE done", "EXAMPLE"},
		{"PEM 私钥", "key -----BEGIN RSA PRIVATE KEY-----\nMIIBsecretstuff\n-----END RSA PRIVATE KEY----- end", "MIIBsecretstuff"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := policy.SanitizeText(tc.in)
			if strings.Contains(out, tc.bad) {
				t.Errorf("脱敏不彻底，输出仍含 %q：\n%s", tc.bad, out)
			}
			if !strings.Contains(out, "******") {
				t.Errorf("输出中没有脱敏占位符：\n%s", out)
			}
		})
	}
}

func TestSanitizeKeepsNormalText(t *testing.T) {
	in := "Pod crash-demo phase=Failed, container exited with code 1"
	if got := policy.SanitizeText(in); got != in {
		t.Errorf("普通日志不应被修改：\n%s", got)
	}
}
