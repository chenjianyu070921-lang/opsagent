# Kubernetes/后端运维排障 Agent 设计文档

日期：2026-10-07

## 1. 背景

线上 Kubernetes 服务异常时，开发和运维需要手动查询 Pod、Deployment、Event、日志、Service、Endpoints、Ingress、Node 和 Prometheus 指标。这个过程重复、耗时，并且新人容易遗漏关键信息。

OpsAgent 要把高频排障流程自动化，围绕指定资源采集证据，结合确定性规则和大模型，生成可信、可审计、可复盘的诊断报告。

## 2. 项目目标

第一版目标是实现一个本地可运行的只读诊断 CLI：

1. 支持诊断指定 Pod、Deployment，后续扩展 Service；
2. 自动采集 Kubernetes 状态、事件和容器日志；
3. 支持 CrashLoopBackOff、ImagePullBackOff、OOMKilled、Pending、探针失败等高频场景；
4. 先由规则引擎判断确定性问题，再由 LLM 汇总证据并生成解释；
5. 输出 Markdown 报告；
6. 保存诊断历史，第二阶段接入 Prometheus 和 SQLite。

## 3. 非目标

第一版不做以下内容：

- Web 平台；
- 多租户和用户体系；
- 通用 bash；
- 任意 kubectl 命令执行；
- Secret 明文读取；
- 自动重启、删除、扩容等写操作；
- 复杂 Operator 平台。

## 4. 使用方式

```bash
opsagent diagnose pod/payment-api-xxx -n production --since 30m
opsagent diagnose deployment/payment-api -n production
opsagent diagnose service/payment-api -n production
opsagent report --last
```

第一版可以在本机或跳板机运行，复用本机 kubeconfig；后续可以部署为 Kubernetes Job，与告警系统联动。

## 5. 技术选型

| 分层 | 技术 |
|---|---|
| 语言 | Go 1.25 |
| CLI | Cobra |
| Agent 编排 | 自研受控状态机 |
| 模型接入 | go-openai / OpenAI-compatible API |
| Kubernetes | client-go |
| 指标 | Prometheus HTTP API |
| 历史存储 | SQLite，第二阶段 |
| TUI | Bubbletea，第二阶段 |
| MCP | mcp-go，第二阶段 |
| 可观测性 | log/slog + OpenTelemetry |
| 发布 | GoReleaser |

## 6. 核心流程

```text
用户输入
  ↓
解析诊断对象
  ↓
读取基础资源
  ↓
识别故障类型
  ↓
按故障类型采集事件、日志、状态、指标
  ↓
规则引擎生成确定性发现
  ↓
LLM 汇总证据并生成根因候选
  ↓
输出 Markdown 报告
```

## 7. 模块划分

```text
cmd/opsagent       程序入口
internal/agent     状态机、计划、分析编排
internal/tool      工具注册表和统一工具接口
internal/kubernetes Kubernetes 资源采集
internal/prometheus Prometheus 指标查询
internal/policy    只读策略、脱敏、审批
internal/storage   诊断历史和证据存储
internal/report    Markdown 报告渲染
internal/observability 日志、追踪和指标
```

## 8. 第一版诊断场景

- CrashLoopBackOff：退出码、上次终止原因、重启次数、日志、探针和资源配置；
- ImagePullBackOff：镜像、tag、imagePullSecrets、仓库访问事件；
- OOMKilled：exitCode=137、内存 limit、内存趋势；
- Pending：调度事件、资源不足、污点、PVC、配额；
- Readiness/Liveness 失败：探针配置、事件、日志、端口；
- Service 无 Endpoints：selector、Pod label、readiness、端口。

## 9. 安全设计

第一版必须遵循：

1. 默认只读；
2. 工具白名单；
3. 禁止通用 shell；
4. 禁止模型自由拼接 kubectl；
5. 不读取 Secret 明文；
6. 日志、事件和资源信息发送给模型前做脱敏；
7. 所有工具调用保留审计记录；
8. 未来写操作必须人工确认。

## 10. 参考项目

- K8sGPT：Analyzer 插件化和规则排障；
- Google kubectl-ai：Agent Loop、工具注册、权限确认和 MCP；
- HolmesGPT：SRE Agent、工具输出预算和 Operator 模式；
- Robusta：告警增强和 Playbook；
- BotKube：ChatOps；
- Fuzzy Labs SRE Agent：评估体系。

## 11. 验收标准

第一版完成时应满足：

1. 可以通过 CLI 诊断指定 Pod/Deployment；
2. 至少正确识别 5 类高频故障；
3. 报告包含结论、证据、建议和风险提示；
4. 默认不执行任何 Kubernetes 写操作；
5. 核心规则具备单元测试；
6. `go test ./...` 通过。
