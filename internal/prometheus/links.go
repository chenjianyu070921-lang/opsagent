package prometheus

import (
	"fmt"
	"net/url"
	"time"
)

// GraphURL 生成 Prometheus 自带图表页链接，打开即可核对该 PromQL 的原始曲线。
func GraphURL(base, promQL string, start, end time.Time) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = "/graph"
	q := u.Query()
	q.Set("g0.expr", promQL)
	q.Set("g0.tab", "0")
	q.Set("g0.start", start.Format(time.RFC3339))
	q.Set("g0.end", end.Format(time.RFC3339))
	u.RawQuery = q.Encode()
	return u.String()
}

// GrafanaExploreURL 生成 Grafana Explore 链接；grafanaBase 为空时返回空串。
func GrafanaExploreURL(grafanaBase, promQL string, start, end time.Time) string {
	if grafanaBase == "" {
		return ""
	}
	u, err := url.Parse(grafanaBase)
	if err != nil {
		return ""
	}
	u.Path = "/explore"
	left := fmt.Sprintf(`{"datasource":"Prometheus","queries":[{"expr":%q,"refId":"A"}],"range":{"from":%q,"to":%q}}`,
		promQL, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	u.RawQuery = "left=" + url.QueryEscape(left)
	return u.String()
}
