# OpsAgent

OpsAgent 是一个用 Go 编写的 Kubernetes/后端运维排障 Agent。

它面向指定 Kubernetes 资源，自动采集 Pod、Deployment、Event、Log、Prometheus 指标等现场信息，结合规则引擎和大模型，输出带证据链、处理建议和风险提示的诊断报告。

## 项目定位

- CLI-first：第一版以命令行工具为主；
- 只读优先：默认不修改集群资源；
- 证据驱动：每条结论都要关联事件、日志、状态或指标；
- 安全可控：不开放通用 shell，不让模型自由拼接危险命令；
- 后期扩展：支持 TUI、MCP、告警联动和受控修复。

## 快速验证

```bash
go run ./cmd/opsagent diagnose pod/<pod-name> -n <namespace>
```

## 用法

```bash
# 诊断指定 Pod（斜杠形式，也可写 diagnose pod <name>）
opsagent diagnose pod/payment-api-xxx -n production --since 30m

# 诊断 Deployment：自动关联其 Pod，采集状态、事件和可疑容器日志
opsagent diagnose deployment/payment-api -n production

# 诊断整个命名空间，或用表格快速浏览
opsagent diagnose -n production
opsagent diagnose -o table
```

输出为 Markdown 诊断报告：结论摘要、发现详情（解释 / 建议 / 风险提示）、证据链。

目前规则引擎可识别：CrashLoopBackOff、ImagePullBackOff、OOMKilled、Pending 调度失败、探针失败、Deployment 发布失败；LLM 分析和 Service 诊断将在后续版本加入。

## 文档

- [项目设计文档](docs/design/2026-10-07-kubernetes-ops-agent.md)
- [路线图](docs/roadmap.md)
