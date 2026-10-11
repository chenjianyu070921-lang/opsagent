# Phase 2：指标和历史记录 设计文档

日期：2026-10-09
状态：已确认，待编写实施计划

## 1. 背景与目标

Phase 1 已完成只读诊断 MVP：过程式采集 Pod/Deployment/Event/Log 构成 Snapshot，规则引擎产出带 `ev-x` 证据引用的 Finding，可选 LLM 汇总根因，输出 Markdown 报告。

Phase 2 在不破坏"只读、证据驱动、离线可用"核心承诺的前提下，补齐五个功能：

1. 接入 Prometheus，把内存与 CPU 指标纳入诊断证据；
2. 使用 SQLite 保存诊断历史，可列表查看、完整复现；
3. 增强证据引用：时间戳、源系统链接、正文行内标记、LLM 引用校验；
4. 接入 OpenTelemetry，观测 opsagent 自身各阶段耗时；
5. 支持报告导出为 Markdown 文件和独立 HTML。

## 2. 非目标

本次明确不做：

- `internal/tool` 工具注册表、Plan/通用状态机（Phase 3 有动态工具选择需求时再建）；
- OTel self-metrics instrument（避免与 Prometheus 双份维护）；
- 查询应用链路（Tempo/Jaeger trace 作为证据）；
- JSON/CSV 导出、history 搜索过滤；
- 历史保留期/清理策略（Phase 4 前补）；
- 任何 Kubernetes 写操作。

## 3. 总体架构

### 3.1 架构思路

编排从 Cobra 命令移到 `internal/agent` 的固定阶段管线；`report.Document` 成为渲染器和历史存储共同的唯一数据模型；外部系统全部收敛在边界接口后面，失败统一降级。

```text
internal/cli            flag 解析 + 依赖装配 + 最终输出（薄）
  └─ internal/agent     Runner.Run()：固定阶段管线（唯一入口）
       ├─ internal/diagnose   Snapshot 采集、规则引擎（纯函数）
       ├─ internal/prometheus 指标查询（边界接口）
       ├─ internal/llm        根因分析（可选阶段）
       ├─ internal/storage    历史仓储（边界接口）
       └─ internal/report     Document 组装（渲染无关）
internal/observability  OTel 初始化（全局，noop 默认）
internal/policy         脱敏（沿用）
```

### 3.2 依赖方向（无环）

- `cli → agent → {diagnose, prometheus, llm, storage, report}`
- `diagnose → prometheus`（Snapshot 持有指标领域类型）
- `report → {diagnose, llm}`
- `storage`、`prometheus` 为叶子包，不依赖任何内部包

### 3.3 Runner 固定阶段

`internal/agent` 只允许一个 Runner、固定阶段，禁止长成框架（不引入 Plan/Step 层次）：

```text
collect（硬失败）
  → metrics（失败降级）
  → rules
  → llm（失败降级）→ 引用校验
  → build Document
  → persist（失败降级）
```

每个阶段对应一个 span。**渲染留在 cli**：Run 返回 Document，由 cli 渲染到 stdout 或文件；未来 TUI 可直接消费 Document。

关键类型：

```go
type Deps struct {
    K8s   k8sclient.Interface
    Prom  prometheus.Querier // 可为 nil
    Repo  storage.Repository // 可为 nil
}

type Input struct {
    Target        diagnose.Target
    Since         time.Duration
    Tail          int64
    UseLLM        bool
    ModelOverride string
    MetricsWindow time.Duration
    GrafanaURL    string
}

type Output struct {
    Document *report.Document
    RecordID int64 // 0 表示未落库
    Notices  []string
}

func (r *Runner) Run(ctx context.Context, in Input) (*Output, error)
```

## 4. Prometheus 接入

### 4.1 地址解析

```go
func ResolveURL(ctx context.Context, explicit string, k8s kubernetes.Interface) (baseURL, source string, err error)
```

优先级：`--prometheus-url` flag → `OPSAGENT_PROMETHEUS_URL` 环境变量 → 集群内自动探测。

探测规则：

- 仅当环境变量 `KUBERNETES_SERVICE_HOST` 存在（集群内运行）时进行；本机/跳板机直接跳过，零等待；
- 在候选命名空间（`monitoring`、`observability`、`default`、`kube-system`）列 Service：先按名字子串（`prometheus-k8s`、`prometheus`）匹配，再按标签 `app.kubernetes.io/name=prometheus` 兜底；
- 候选数封顶 8 个，对 `http://<svc>.<ns>.svc:<port>` 并行探测 `/api/v1/status/buildinfo`，总预算 3s，首个成功者胜出；
- 返回值带 `source`（flag/env/discovery），记入 Document 和 DB 列，"指标哪来的"可审计；
- 全部未命中不报错，产 notice：「未找到 Prometheus（可用 --prometheus-url 或 OPSAGENT_PROMETHEUS_URL 指定），跳过指标分析」。

### 4.2 查询内容

固定 4 条 QueryRange，窗口默认 `--metrics-window=30m`，step = `window/60`（下限 15s），4 条并发、整体超时 10s，每条独立失败、独立降级：

| 查询 | PromQL 要点 | 用途 |
|---|---|---|
| 内存用量 | `container_memory_working_set_bytes{namespace, pod, container!=""}` | 内存趋势 |
| 内存上限 | `kube_pod_container_resource_limits{resource="memory"}` | 计算占比 |
| CPU 用量 | `rate(container_cpu_usage_seconds_total[5m])` | CPU 用量曲线 |
| CPU 限流 | `rate(container_cpu_cfs_throttled_periods_total[5m]) / rate(container_cpu_cfs_periods_total[5m])` | 限流比率 |

- limit 序列缺失（未装 kube-state-metrics）时只放弃占比计算，其余照常；
- 客户端：官方 `github.com/prometheus/client_golang/api` + `api/prometheus/v1`（v1.API 是 interface，可注入假实现）；
- 结果按 pod/container 归并：

```go
type Point struct {
    T time.Time
    V float64
}

type ContainerMetrics struct {
    Pod, Container      string
    MemoryWorkingSet    []Point
    MemoryLimit         *float64
    MemoryPeakRatio     float64
    CPUUsageCores       []Point
    CPULimit            *float64
    CPUThrottledRatio   []Point
}

type Querier interface {
    Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*ContainerMetrics, error)
}
```

结果挂到 `snap.Metrics[pod][container]`。

### 4.3 证据链接

```go
func GraphURL(base, promQL string, start, end time.Time) string
// /graph?g0.expr=<urlencoded>&g0.tab=0&g0.start=...&g0.end=...

func GrafanaExploreURL(grafanaBase, promQL string, start, end time.Time) string
// /explore?left=<url.QueryEscape 的 JSON>；grafanaBase 为空时返回空串
```

Grafana 地址来自 `--grafana-url` 或 `OPSAGENT_GRAFANA_URL`，未配置则省略该链接。

## 5. 证据模型与规则

### 5.1 证据模型升级

```go
type EvidenceLink struct {
    Type  string // graph / grafana / promql
    Label string
    URL   string
}

type Evidence struct {
    ID, Kind, Source, Summary, Detail string
    Timestamp *time.Time     // 状态=采集时刻；事件=事件 LastTimestamp；指标=观测时刻
    Links     []EvidenceLink // 指标证据携带
}
```

- `EvidenceSet.Add(kind, source, summary, detail)` 签名保持不变（现有 6 条规则零改动），内部时间戳取 Snapshot 采集时刻；
- 新增 `AddWith(ev Evidence) string` 接收完整元数据（指标规则使用）；
- ev-x 编号仍在注册时分配，单次诊断内有效；
- 去重键沿用 Phase 1 的 `kind|source|summary`；指标证据的 PromQL 已体现在 Summary 与 Links 中，不另加去重维度。

### 5.2 规则变化

- 新增 2 条规则，追加到 rules 末尾：
  - `memoryTrendRule`：窗口内 working_set/limit 峰值占比 ≥ 85% 时出 Finding（严重度：中）；若同一容器已有 OOMKilled 发现，则不重复成条；
  - `cpuThrottleRule`：限流比率窗口均值 ≥ 25% 时出 Finding（严重度：低），提示 CPU limit 过紧或计算尖峰；
- 现有 `oomKilledRule` 增强：指标可得时追加一条 `kind=metric` 证据（趋势 + graph 链接，经 `AddWith`），帮助区分"内存缓慢增长逼近 limit"与"瞬时真实增长"；
- 指标缺失时所有规则行为与 Phase 1 完全一致，不产生空发现。

## 6. SQLite 诊断历史

### 6.1 数据库

- 默认路径：`os.UserConfigDir()/opsagent/history.db`（Windows 为 `%AppData%\opsagent\history.db`），可用 `--history-db` 或 `OPSAGENT_HISTORY_DB` 覆盖；打开时自动创建目录；
- 驱动 `github.com/glebarez/go-sqlite`（底层 modernc.org/sqlite 纯 Go，driver 名 `sqlite`），零 CGO，GoReleaser 交叉编译无额外工具链；
- DSN 带 `_busy_timeout=5000&_fk=1`；journal 模式保持默认 DELETE，避免 WAL 的 `-wal/-shm` 附属文件；
- 迁移：`PRAGMA user_version` + 内嵌有序 DDL，事务内逐版本执行，不引入 golang-migrate/goose。

v1 表结构：

```sql
CREATE TABLE diagnosis_records (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at       TEXT    NOT NULL,              -- RFC3339 UTC
  namespace        TEXT    NOT NULL,
  target_kind      TEXT    NOT NULL,              -- pod / deployment / all
  target_name      TEXT    NOT NULL DEFAULT '',
  target_display   TEXT    NOT NULL,              -- pod/payment-api-xxx，list 直接展示
  findings_count   INTEGER NOT NULL DEFAULT 0,
  highest_severity TEXT    NOT NULL DEFAULT '',
  duration_ms      INTEGER NOT NULL DEFAULT 0,
  prometheus_url   TEXT    NOT NULL DEFAULT '',
  llm_model        TEXT    NOT NULL DEFAULT '',
  schema_version   INTEGER NOT NULL,              -- payload 结构版本，当前 1
  payload          TEXT    NOT NULL               -- report.Document 的 JSON
);

CREATE INDEX idx_diagnosis_created_at ON diagnosis_records (created_at DESC);
CREATE INDEX idx_diagnosis_ns_time    ON diagnosis_records (namespace, created_at DESC);
```

设计要点：

- list 只读标量列，不解析 payload；完整报告仅 Get 时反序列化 Document → 走同一套 Renderer；
- 历史中冻结的 ev-x 编号与链接原样复现，不承诺跨记录稳定；
- 未来报告结构不兼容时，由组装/渲染层按 `schema_version` 做读时兼容或提示，不改写历史行。

仓储接口：

```go
type Record struct {
    ID              int64
    CreatedAt       time.Time
    Namespace       string
    TargetKind      string
    TargetName      string
    TargetDisplay   string
    FindingsCount   int
    HighestSeverity string
    DurationMS      int64
    PrometheusURL   string
    LLMModel        string
    SchemaVersion   int
    Payload         []byte
}

type RecordSummary struct { /* 同上，无 Payload */ }

type Repository interface {
    Save(ctx context.Context, r Record) (int64, error)
    List(ctx context.Context, namespace string, limit int) ([]RecordSummary, error)
    Get(ctx context.Context, id int64) (*Record, error)
    Latest(ctx context.Context) (*Record, error)
}
```

### 6.2 落库时机与失败语义

- Runner 的 `persist` 阶段在 Document 组装后执行：保存标量摘要 + payload JSON（`schema_version=1`），拿回 RecordID；
- diagnose 主流程中 DB 为 best-effort：打开/迁移/保存失败只产 notice（Document 顶部提示区 + stderr），退出码不变；
- history 命令和 `--last` 相反：DB 是其工作对象，打不开或记录不存在作为命令错误返回（非零退出）；空库 `--last` 提示「暂无诊断历史」。

### 6.3 history 命令

```bash
opsagent history list                     # 表格：ID/时间/命名空间/对象/发现数/最高严重度
opsagent history list -n production       # 按命名空间过滤
opsagent history show <id>                # 完整复现报告（默认 markdown）
opsagent history show <id> -f html        # 指定格式
opsagent history export <id> -o report.html   # 导出文件
opsagent diagnose --last                  # 最近一次记录快捷查看
```

`show/export` 全程不触碰集群：Get → Unmarshal Document → 同一渲染器 → stdout 或文件。

## 7. OpenTelemetry

只观测 opsagent 自身（trace，不做 metrics）：

- `observability.Init(ctx) (shutdown func(context.Context) error, err error)`：仅当 `OTEL_EXPORTER_OTLP_ENDPOINT` 或 `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` 非空才初始化；否则返回 noop shutdown，不启动 goroutine、不发起连接；
- exporter 用 `otlptracehttp` + BatchSpanProcessor，SDK 自动识别标准变量（`OTEL_EXPORTER_OTLP_HEADERS`、`OTEL_SERVICE_NAME` 默认 `opsagent`、`OTEL_RESOURCE_ATTRIBUTES`），不自造配置面；
- span 结构：根 span `diagnose`，子阶段 `collect / prometheus / rules / llm / build / persist`；`--last`、history 命令也有轻量根 span；降级阶段在 span 上记 event + `ok=false`，不冒泡 error；
- exporter 初始化失败 → stderr 一次性警告并回退 noop；批量上报失败静默（标准 SDK 行为）；main 退出前以 5s 预算 shutdown flush，超时不阻塞退出。

## 8. 报告导出

### 8.1 渲染器

```go
type Renderer interface {
    Render(w io.Writer, doc *Document) error
}

func For(format string) (Renderer, error)   // markdown / html
func FormatFromPath(path string) string     // .md→markdown, .html/.htm→html
```

- Markdown 渲染器由 Phase 1 逻辑迁移为消费 Document；md/html 共用同一份 Document，取证逻辑只有一套；
- 报告正文（Finding 的解释/建议）使用行内 `[ev-x]` 标记，由 Document 组装时按 `EvidenceIDs` 回填；
- Markdown 证据链条目附带时间戳与 graph/Grafana 链接。

### 8.2 HTML 渲染器

`html/template` + `//go:embed report.html` 单模板，内嵌 `<style>`，无外部资源：

- 证据用原生 `<details><summary>` 折叠，条目带 `id="ev-1"` 锚点；
- Finding 正文行内 `[ev-x]` 渲染为 `<a href="#ev-x">`；graph/Grafana 链接渲染为 `<a target="_blank">`；
- 结构化字段经模板自动转义；LLM 自由正文用 `goldmark` 转 HTML（默认不透传 raw HTML）；
- goldmark 是可砍依赖：必要时降级为 `html.EscapeString` 后 `<pre>` 展示。

### 8.3 触发方式

- `diagnose --export report.html`：格式取 `--format` 或扩展名；导出时 stdout 只打印文件路径与记录 ID，不重复全文；
- `history export <id> -o report.md`。

## 9. CLI 变化

`diagnose` 新增 flags：

| Flag | 默认值 | 说明 |
|---|---|---|
| `--prometheus-url` | 空 | Prometheus 地址（优先级最高） |
| `--grafana-url` | 空 | Grafana 地址，用于 Explore 链接 |
| `--metrics-window` | 30m | 指标查询窗口 |
| `--export <path>` | 空 | 导出报告到文件 |
| `--format` | 空 | 强制指定导出格式 |
| `--last` | false | 查看最近一次诊断 |
| `--history-db` | 空 | SQLite 文件路径 |

RunE 瘦身为：解析 flags → 装配依赖（K8s client、ResolveURL+Querier、Repo、observability）→ `agent.Runner.Run` → 渲染输出。

特殊分支：

- `--last` 与诊断目标参数互斥，同时给出报用法错误；不采集集群，`Repo.Latest` → Document → 渲染；
- `--export` 的格式在采集前确定；未知扩展名且未给 `--format` 时提前报用法错误。

新增 `internal/cli/history.go`（list/show/export）；`main.go` 改为 signal context + `observability.Init` + 退出前 shutdown；根命令注册 history 命令。

## 10. 错误处理总表

| 故障 | diagnose 主流程 | history / --last |
|---|---|---|
| K8s 采集失败 | 硬失败（无现场不成诊断）；单容器日志失败继续跳过 | 不涉及（不碰集群） |
| Prometheus 未找到/查询失败/超时 | notice + stderr，指标为空，报告照常 | 不涉及 |
| LLM 调用失败 | stderr 警告，纯规则报告（沿用 Phase 1） | 不涉及 |
| LLM 引用不存在的 ev-x | 不删 LLM 章节：缺失编号在 Document 顶部 notice 区列出；正文 `[ev-x]` 中性化为 `[ev-x（无效引用）]` | 历史原样保留当时校验结果 |
| DB 打开/迁移/保存失败 | notice + stderr，退出码不变 | 命令错误，非零退出 |
| 导出路径不可写/格式不支持 | 命令错误，列出支持的格式 | 同左 |
| OTel endpoint 缺失/初始化失败 | noop / stderr 一次性警告后 noop | 同左 |

引用校验实现：`report.BuildDocument` 内用正则 `ev-\d+` 扫描 LLM 正文，与证据集合比对；无效引用列入 `Document.LLM.InvalidCitations` 并产生 notice。

## 11. 测试策略

全部离线、无外部服务，门禁沿用 `go test ./...`：

- **prometheus**：httptest 返回罐装 `/api/v1/query_range`、`/api/v1/status/buildinfo` JSON；断言 4 条 PromQL 含 namespace/pod 选择器、matrix 归并正确、5xx/超时独立降级；链接用 `url.Parse` 反查编码完整；探测用 client-go 自带 fake clientset 构造 Service，验证枚举、3s 预算截断、集群外跳过；
- **storage**：`t.TempDir()` 内真实驱动往返（Save→List→Get），断言 `user_version`、索引、未知 ID 与损坏 payload 的可识别错误；list 不触达 payload；
- **diagnose**：现有 rules_test 夹具增加 metrics 字段，覆盖 85%/25% 阈值边界、OOM 附带 metric 证据、无指标时行为不变、`AddWith` 去重；
- **report**：Markdown 黄金字符串断言行内 `[ev-x]`/链接/notice；HTML 断言 `<details>`、`id="ev-x"`、锚点 href、特殊字符被转义；引用校验覆盖 ev-7 不存在、ev-0 边界、正文无引用；
- **agent**：Runner 注入 fake K8s + 可切换报错的 fake Querier + 临时 SQLite + noop tracer，端到端断言：Prom 报错仍出 findings 且 notice 非空、无效引用被中性化、Save 失败不影响 Document；
- **cli**：Cobra `SetArgs` 测 `--last` 空库、`--last` 与目标互斥、`--export` 按扩展名选格式、格式冲突报错。

## 12. 验收标准

1. 配置 Prometheus 后，OOM 诊断附带内存趋势与 graph 链接；内存逼近 limit（≥85%）与 CPU 限流（≥25%）可独立产出发现；
2. Prometheus/OTel/DB 任一不可用时，规则报告照常产出，报告内有明确 notice；
3. `history list` 可查看历史，`history show <id>` 与 `--last` 可完整复现报告（含证据编号与链接）；
4. `diagnose --export` 与 `history export` 可导出 .md 与 .html；HTML 证据可折叠、引用可跳转、链接可点；
5. LLM 编造的 ev-x 编号被显式标记为无效引用；
6. 全程不执行任何 Kubernetes 写操作；
7. `go test ./...` 通过。
