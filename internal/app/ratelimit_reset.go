package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// 匹配常见错误消息中的等待/重试时间描述，如:
	// "try again in 20s", "Please retry in 5 minutes", "resets in 2h30m"
	reTryAgainIn = regexp.MustCompile(`(?i)(?:try again in|retry (?:after|in)|resets? in)\s+([0-9]+(?:\.[0-9]+)?\s*(?:s(?:ec(?:ond)?s?)?|m(?:in(?:ute)?s?)?|h(?:our)?s?|d(?:ay)?s?))`)
	// 匹配时间点描述，如 "resets at 2026-10-24 18:00:00" 或 "resets at 1729792800"
	reResetsAt = regexp.MustCompile(`(?i)(?:resets?|retry) at\s+([0-9T:\-Z\s]+)`)
)

// extractRateLimitReset 从响应头与 Body 中解析上游返回的限流恢复时间，
// 返回标准格式的人类可读字符串，未获取到时返回空字符串。
func extractRateLimitReset(header http.Header, body []byte) string {
	// 1. 优先从标准与常见速率限制响应头提取
	if header != nil {
		headersToCheck := []string{
			"Retry-After",
			"X-Ratelimit-Reset",
			"Ratelimit-Reset",
			"X-Rate-Limit-Reset",
			"X-Ratelimit-Reset-Tokens",
			"X-Ratelimit-Reset-Requests",
		}
		for _, h := range headersToCheck {
			val := strings.TrimSpace(header.Get(h))
			if val == "" {
				continue
			}
			if formatted := formatResetTimeValue(val); formatted != "" {
				return formatted
			}
		}
	}

	// 2. 从响应体 Body 中解析
	if len(body) > 0 {
		if formatted := extractResetFromBody(body); formatted != "" {
			return formatted
		}
	}

	return ""
}

// formatResetTimeValue 解析时间值（支持 Unix 秒/毫秒、相对秒数、HTTP-Date、Go 时长字符串）
func formatResetTimeValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}

	// 纯数字判断（Unix 时间戳或秒数）
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		sec := int64(f)
		now := time.Now()
		// 秒级 Unix 时间戳（> 1e9，即 2001 年以后）
		if sec > 1000000000 && sec < 10000000000 {
			t := time.Unix(sec, 0)
			return formatFutureTime(t, now)
		}
		// 毫秒级 Unix 时间戳（> 1e12）
		if sec >= 1000000000000 {
			t := time.UnixMilli(sec)
			return formatFutureTime(t, now)
		}
		// 相对秒数（例如 Retry-After: 120）
		if f > 0 {
			d := time.Duration(f * float64(time.Second))
			target := now.Add(d)
			return fmt.Sprintf("in %s (at %s)", d.Round(time.Second).String(), target.UTC().Format("2006-01-02 15:04:05 UTC"))
		}
	}

	// HTTP-Date 格式 (e.g. "Wed, 21 Oct 2026 07:28:00 GMT")
	if t, err := http.ParseTime(v); err == nil {
		return formatFutureTime(t, time.Now())
	}

	// 时长格式 (e.g. "120s", "2m30s", "1h")
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		now := time.Now()
		target := now.Add(d)
		return fmt.Sprintf("in %s (at %s)", d.Round(time.Second).String(), target.UTC().Format("2006-01-02 15:04:05 UTC"))
	}

	return v
}

func formatFutureTime(target time.Time, now time.Time) string {
	diff := target.Sub(now).Round(time.Second)
	if diff > 0 {
		return fmt.Sprintf("%s (in %s)", target.UTC().Format("2006-01-02 15:04:05 UTC"), diff.String())
	}
	return target.UTC().Format("2006-01-02 15:04:05 UTC")
}

// extractResetFromBody 解析 JSON 字段或文本正则
func extractResetFromBody(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		// 检查顶层或 error 嵌套下的候选字段
		candidates := []any{
			m["resets_at"],
			m["reset_at"],
			m["reset_time"],
			m["retry_after"],
			m["reset_after"],
		}
		if errObj, ok := m["error"].(map[string]any); ok {
			candidates = append(candidates,
				errObj["resets_at"],
				errObj["reset_at"],
				errObj["reset_time"],
				errObj["retry_after"],
				errObj["reset_after"],
			)
			// 从 error.message 匹配
			if msg, ok := errObj["message"].(string); ok {
				if parsed := extractFromText(msg); parsed != "" {
					return parsed
				}
			}
		}
		for _, c := range candidates {
			if c == nil {
				continue
			}
			var strVal string
			switch val := c.(type) {
			case float64:
				strVal = strconv.FormatFloat(val, 'f', -1, 64)
			case int64:
				strVal = strconv.FormatInt(val, 10)
			case int:
				strVal = strconv.Itoa(val)
			case string:
				strVal = val
			}
			if strVal != "" {
				if formatted := formatResetTimeValue(strVal); formatted != "" {
					return formatted
				}
			}
		}

		if msg, ok := m["message"].(string); ok {
			if parsed := extractFromText(msg); parsed != "" {
				return parsed
			}
		}
	}

	// 纯文本兜底正则匹配
	return extractFromText(string(body))
}

func extractFromText(text string) string {
	if matches := reTryAgainIn.FindStringSubmatch(text); len(matches) > 1 {
		durStr := strings.TrimSpace(matches[1])
		// 标准化单位，例如 5 minutes -> 5m, 20 seconds -> 20s
		durStr = normalizeDurationString(durStr)
		if formatted := formatResetTimeValue(durStr); formatted != "" {
			return formatted
		}
		return "in " + matches[1]
	}
	if matches := reResetsAt.FindStringSubmatch(text); len(matches) > 1 {
		timeStr := strings.TrimSpace(matches[1])
		if formatted := formatResetTimeValue(timeStr); formatted != "" {
			return formatted
		}
		return timeStr
	}
	return ""
}

func normalizeDurationString(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "seconds", "s")
	s = strings.ReplaceAll(s, "second", "s")
	s = strings.ReplaceAll(s, "sec", "s")
	s = strings.ReplaceAll(s, "minutes", "m")
	s = strings.ReplaceAll(s, "minute", "m")
	s = strings.ReplaceAll(s, "min", "m")
	s = strings.ReplaceAll(s, "hours", "h")
	s = strings.ReplaceAll(s, "hour", "h")
	s = strings.ReplaceAll(s, "days", "d")
	s = strings.ReplaceAll(s, "day", "d")
	// 简单的天转小时处理，如 1d -> 24h
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return fmt.Sprintf("%dh", days*24)
		}
	}
	return s
}

// injectRateLimitReset 将限流恢复时间插入到现有的响应体 JSON 中。
// 若 body 已经是合法 JSON 对象，则在现有结构（顶层及 error 内部）中插入 rate_limit_reset 字段；
// 若未能获取到有效恢复时间，则原样返回原 body。
func injectRateLimitReset(body []byte, resetInfo string) []byte {
	if strings.TrimSpace(resetInfo) == "" {
		return body
	}
	if len(body) == 0 {
		resp, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message":          "rate limit reached",
				"type":             "rate_limit_error",
				"rate_limit_reset": resetInfo,
			},
			"rate_limit_reset": resetInfo,
		})
		return resp
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	m["rate_limit_reset"] = resetInfo
	if errObj, ok := m["error"].(map[string]any); ok {
		errObj["rate_limit_reset"] = resetInfo
	}

	modified, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return modified
}

// injectRateLimitResetWithHeader 自动从 header 与 body 提取恢复时间，并插入到响应体中返回。
func injectRateLimitResetWithHeader(body []byte, header http.Header) []byte {
	reset := extractRateLimitReset(header, body)
	if reset == "" {
		return body
	}
	return injectRateLimitReset(body, reset)
}
