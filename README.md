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
go run ./cmd/opsagent
```

## 计划中的命令

```bash
opsagent diagnose pod/payment-api-xxx -n production --since 30m
opsagent diagnose deployment/payment-api -n production
opsagent diagnose service/payment-api -n production
opsagent report --last
```

## 文档

- [项目设计文档](docs/design/2026-10-07-kubernetes-ops-agent.md)
- [路线图](docs/roadmap.md)
