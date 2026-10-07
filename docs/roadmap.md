# 路线图

## Phase 1：Kubernetes 只读诊断 MVP

- 初始化 Cobra CLI；
- 接入 client-go；
- 支持 Pod / Deployment / Event / Log；
- 实现高频故障规则；
- 接入 LLM 生成分析；
- 输出 Markdown 报告。

## Phase 2：指标和历史记录

- 接入 Prometheus；
- 使用 SQLite 保存诊断历史；
- 增加证据引用；
- 接入 OpenTelemetry；
- 支持报告导出。

## Phase 3：TUI 和 MCP

- 使用 Bubbletea 实现终端交互；
- 使用 mcp-go 暴露 MCP Server；
- 接入 Runbook；
- 扩展 Service、Ingress、Node 等诊断。

## Phase 4：告警联动和受控修复

- 接入 AlertManager；
- 自动发送报告到 Slack / 飞书；
- 高危操作人工确认；
- 支持 rollout restart、扩缩容建议；
- 评估 Server / Operator 模式。
