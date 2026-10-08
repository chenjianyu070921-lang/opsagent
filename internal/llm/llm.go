// Package llm 封装大模型调用：把规则结论与证据组织成提示词，获取模型的根因分析。
// 模型只能看到本包给出的材料，不能直接访问集群或自由拼接命令。
package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sashabaranov/go-openai"

	"opsagent/internal/diagnose"
)

// Config 是模型连接配置，来自环境变量，可用 flag 覆盖模型名。
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

// 环境变量名与 README/配置说明保持一致。
const (
	EnvBaseURL = "OPSAGENT_LLM_BASE_URL"
	EnvAPIKey  = "OPSAGENT_LLM_API_KEY"
	EnvModel   = "OPSAGENT_LLM_MODEL"
)

// LoadConfig 从环境变量读取模型配置；缺项时返回错误，提示具体要配哪个变量。
func LoadConfig() (Config, error) {
	cfg := Config{
		BaseURL: os.Getenv(EnvBaseURL),
		APIKey:  os.Getenv(EnvAPIKey),
		Model:   os.Getenv(EnvModel),
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, EnvBaseURL)
	}
	if cfg.APIKey == "" {
		missing = append(missing, EnvAPIKey)
	}
	if cfg.Model == "" {
		missing = append(missing, EnvModel)
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("LLM 未配置：请设置环境变量 %s（设置后需重启终端/VS Code）", strings.Join(missing, "、"))
	}
	return cfg, nil
}

// Analysis 是模型返回的分析结果。
type Analysis struct {
	Model string
	// Content 是模型正文（Markdown），直接嵌入报告。
	Content string
}

// Analyze 把规则发现和证据发给模型，返回根因分析。
// maxTokens 需要给足：推理模型的思考过程也计入该预算，太小会导致正文为空。
func Analyze(ctx context.Context, cfg Config, snap *diagnose.Snapshot, findings []diagnose.Finding, evidence []diagnose.Evidence) (*Analysis, error) {
	system, user := BuildPrompt(snap, findings, evidence)

	clientCfg := openai.DefaultConfig(cfg.APIKey)
	clientCfg.BaseURL = cfg.BaseURL
	client := openai.NewClientWithConfig(clientCfg)

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: system},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
		Temperature: 0.2,
		MaxTokens:   4096,
	})
	if err != nil {
		return nil, fmt.Errorf("调用 LLM 失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("LLM 返回为空")
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		// 推理模型在预算不足时可能只返回了思考内容而没有正文。
		return nil, errors.New("LLM 正文为空（可能是 token 预算被思考过程耗尽，需要调大 max_tokens）")
	}
	return &Analysis{Model: resp.Model, Content: content}, nil
}
