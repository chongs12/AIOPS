package ticket

import "strings"

// NormalizeType 归一化工单类型字符串（LLM 输出容错）。
func NormalizeType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "5xx_surge", "5xx", "http_5xx":
		return Type5xxSurge
	case "pod_oom", "oom":
		return TypePodOOM
	case "slow_query", "slowquery", "db_slow":
		return TypeSlowQuery
	default:
		return TypeOther
	}
}
