// Package policy 实现只读诊断的安全策略：发送给模型前的文本脱敏等。
package policy

import "regexp"

// secretPattern 描述一类需要在发给 LLM 前抹掉的敏感内容。
type secretPattern struct {
	re   *regexp.Regexp
	repl string
}

// replacement 是统一的脱敏占位符。
const replacement = "******"

// patterns 覆盖常见的凭证形态：云厂商/API 密钥、Authorization 头、
// password/secret/token 赋值、AWS Key、PEM 私钥。
var patterns = []secretPattern{
	// 火山方舟 / OpenAI 风格 API Key
	{regexp.MustCompile(`ark-[0-9a-fA-F-]{8,}`), replacement},
	{regexp.MustCompile(`sk-[A-Za-z0-9]{6,}`), replacement},
	// Authorization: Bearer xxx（大小写不敏感）
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[A-Za-z0-9\-\._~+/=]+`), "${1}" + replacement},
	// password=xxx / token: xxx / secret="xxx" 等键值赋值
	{regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key)(\s*[:=]\s*)[^\s"',;]+`), "${1}${2}" + replacement},
	// AWS Access Key ID
	{regexp.MustCompile(`AKIA[0-9A-Z]{12,}`), replacement},
	// PEM 私钥块
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), replacement},
}

// SanitizeText 抹掉文本中可能的密钥和凭证，返回脱敏后的文本。
// 规则只处理高置信度的凭证形态，避免把普通日志内容大面积误改。
func SanitizeText(text string) string {
	for _, p := range patterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	return text
}
