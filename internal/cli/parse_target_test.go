package cli

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantKind   string
		wantTarget string
	}{
		{"无参数", nil, "all", ""},
		{"斜杠形式 pod", []string{"pod/payment-api-xxx"}, "pod", "payment-api-xxx"},
		{"斜杠形式 deployment", []string{"deployment/payment-api"}, "deployment", "payment-api"},
		{"两参数形式", []string{"pod", "payment-api-xxx"}, "pod", "payment-api-xxx"},
		{"裸资源类型别名", []string{"deploy"}, "deployment", ""},
		{"资源别名", []string{"po/name"}, "pod", "name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTarget(tc.args)
			if got.Kind != tc.wantKind || got.Name != tc.wantTarget {
				t.Errorf("parseTarget(%v) = (%q, %q)，期望 (%q, %q)",
					tc.args, got.Kind, got.Name, tc.wantKind, tc.wantTarget)
			}
		})
	}
}
