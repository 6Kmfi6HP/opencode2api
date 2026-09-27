package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
)

// StreamProtocolHooks lets each protocol customize error/productive detection
// without changing the retry driver.
type StreamProtocolHooks struct {
	// IsErrorEvent reports whether a complete SSE frame payload represents an
	// upstream error (as opposed to a productive event).
	IsErrorEvent func(payload []byte) bool
	// IsProductiveEvent reports whether a complete SSE frame payload represents
	// a productive (non-error) event.
	IsProductiveEvent func(payload []byte) bool
}

// responsesIsErrorEvent reports whether a Responses-protocol SSE frame is an
// error: type=error / type=response.failed, or any frame carrying a
// top-level "error" key.
func responsesIsErrorEvent(payload []byte) bool {
	var evt map[string]any
	if json.Unmarshal(payload, &evt) != nil {
		return false
	}
	typ, _ := evt["type"].(string)
	if typ == "error" || typ == "response.failed" {
		return true
	}
	_, hasErr := evt["error"]
	return hasErr
}

// responsesIsProductiveEvent reports whether a Responses-protocol SSE frame
// carries meaningful content ([DONE]/whitespace do not count).
func responsesIsProductiveEvent(payload []byte) bool {
	t := strings.TrimSpace(string(payload))
	if t == "" || t == "[DONE]" {
		return false
	}
	return !responsesIsErrorEvent(payload)
}

// chatIsErrorEvent reports whether a chat-completion SSE frame is an error
// (top-level "error" key on the JSON payload).
func chatIsErrorEvent(payload []byte) bool {
	var evt map[string]any
	if json.Unmarshal(payload, &evt) != nil {
		return false
	}
	_, hasErr := evt["error"]
	return hasErr
}

// chatIsProductiveEvent reports whether a chat-completion SSE frame carries
// meaningful content ([DONE]/whitespace do not count).
func chatIsProductiveEvent(payload []byte) bool {
	t := strings.TrimSpace(string(payload))
	if t == "" || t == "[DONE]" {
		return false
	}
	return !chatIsErrorEvent(payload)
}

// anthropicIsErrorEvent reports whether an Anthropic-SSE frame is an error
// (type=error).
func anthropicIsErrorEvent(payload []byte) bool {
	var evt map[string]any
	if json.Unmarshal(payload, &evt) != nil {
		return false
	}
	typ, _ := evt["type"].(string)
	return typ == "error"
}

// anthropicIsProductiveEvent reports whether an Anthropic-SSE frame carries
// meaningful content ([DONE]/whitespace do not count).
func anthropicIsProductiveEvent(payload []byte) bool {
	t := strings.TrimSpace(string(payload))
	if t == "" || t == "[DONE]" {
		return false
	}
	return !anthropicIsErrorEvent(payload)
}

var (
	// ResponsesProtocolHooks implements the Responses-protocol peek logic:
	// a frame is an error when its JSON payload carries type=error /
	// type=response.failed or a top-level "error" key; anything else with
	// non-empty content counts as productive.
	ResponsesProtocolHooks = StreamProtocolHooks{
		IsErrorEvent:      responsesIsErrorEvent,
		IsProductiveEvent: responsesIsProductiveEvent,
	}

	// ChatProtocolHooks implements chat-completion chunk detection:
	// error when the frame carries a top-level "error" key; [DONE] and
	// whitespace-only frames are non-productive; anything else is productive.
	ChatProtocolHooks = StreamProtocolHooks{
		IsErrorEvent:      chatIsErrorEvent,
		IsProductiveEvent: chatIsProductiveEvent,
	}

	// AnthropicProtocolHooks implements Anthropic SSE detection:
	// error when the frame carries type=error; productive otherwise.
	AnthropicProtocolHooks = StreamProtocolHooks{
		IsErrorEvent:      anthropicIsErrorEvent,
		IsProductiveEvent: anthropicIsProductiveEvent,
	}
)

// errStreamIncompleteNoCommit：peek 窗口内上游空流 EOF / 首字节超时 / 读
// 错误 / 上游错误事件——尚未向客户端写过任何字节，调用方可安全重试。
var errStreamIncompleteNoCommit = errors.New("stream incomplete before first client byte")

// FlushPeekedBytes 把 peek 阶段攒的 SSE 行原样写回 w（行字节、换行符都
// 不动）。返回首个写错误。byte-level 透传路径用它把 peek 消费掉的字节
// 回放给客户端。
func FlushPeekedBytes(w io.Writer, peeked []streamReadResult) error {
	for _, res := range peeked {
		if res.line == "" {
			continue
		}
		if _, err := io.WriteString(w, res.line); err != nil {
			return err
		}
	}
	return nil
}

// PeekOutcome 是 PeekFirstFrame 的结果。Consumed 不为空时表示「已有完整
// SSE 帧被消费」，调用方应把它原样喂回 handler 主循环。
type PeekOutcome struct {
	Consumed []streamReadResult
	Err      error
	// Reader 是 peek 内部使用的流式 reader（已经包了一层自己的 bufio 缓
	// 冲）。调用方应把它原样续用做主循环 reader ——否则其内部 bufio 里
	// 已经预读的行会被两个独立 bufio 撕成两半。
	Reader *streamReader
}

// PeekFirstFrame 在向上游拿到 200、但还未向客户端 WriteHeader 之前「窥
// 视」首个**完整 SSE 帧**(空行收尾；流末尾则接受 EOF 收尾)。
//
// 返回约定：
//   - 已见完整产出帧且非错误：Reader 续用,Consumed 应回放给主循环——
//     commit。
//   - 已见完整错误帧：Err=errStreamIncompleteNoCommit,reader 已关闭——
//     由调用方走未 commit 重试。
//   - 窗口内 EOF / 读错 / 超时且无产出：同上,Err=errStreamIncompleteNoCommit。
//
// 关键不变量:返回时 Consumed 一定落在帧边界(空行或 EOF 行)之后,
// 绝不截断在帧中间——否则 handler 侧的 bufio 续读会把当前帧的剩余行
// 与后续帧拼错。
func PeekFirstFrame(ctx context.Context, rc io.Reader, timeout time.Duration, hooks StreamProtocolHooks) PeekOutcome {
	reader := newStreamReader(ctx, rc, 0)
	// 不要 defer Close:成功路径里调用方会续用这个 reader(它的 bufio
	// 里可能已经预读了后续行);只有失败路径在这里显式关闭。

	var timeoutCh <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}

	var consumed []streamReadResult
	var frameBuf []string
	// hasFrame：已经见过至少一个完整 data 帧（含 ERROR 帧——错误帧也是「有产出」）。
	hasFrame := false
	// errorEvent：已见帧里存在错误事件。
	errorEvent := false

	// flushFrame 在当前帧边界（空行或 EOF）结算 frameBuf：判定 hasFrame /
	// errorEvent，并原样把整帧追加进 consumed。
	flushFrame := func() {
		for _, dl := range frameBuf {
			t := strings.TrimSpace(dl)
			if t == "" || t == "[DONE]" {
				continue
			}
			hasFrame = true
			if !errorEvent && hooks.IsErrorEvent != nil && hooks.IsErrorEvent([]byte(t)) {
				errorEvent = true
			}
		}
		frameBuf = nil
	}

	for {
		select {
		case <-ctx.Done():
			return PeekOutcome{Err: ctx.Err()}
		case <-timeoutCh:
			reader.Close()
			return PeekOutcome{Err: errStreamIncompleteNoCommit}
		case res := <-reader.Read():
			consumed = append(consumed, res)
			trimmed := strings.TrimSpace(res.line)
			switch {
			case trimmed == "":
				flushFrame()
			case strings.HasPrefix(trimmed, ":"):
				// SSE 心跳注释：不算产出。
			case strings.HasPrefix(trimmed, "data:"):
				frameBuf = append(frameBuf, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			case strings.HasPrefix(trimmed, "event:"):
				// 仅记录属于哪个事件；data 到帧尾才结算。
			case strings.HasPrefix(trimmed, "{"):
				// 非标准裸 JSON 行：直接当一帧。
				frameBuf = append(frameBuf, trimmed)
				flushFrame()
			}
			if res.err != nil {
				// EOF：流自然终止。当前若有残帧,先按帧结算;随后无论
				// 是否有产出都关闭——截在 EOF 处 consumed 已对齐帧边界。
				flushFrame()
				if !hasFrame || errorEvent {
					// 失败路径:把 reader 也关掉,免得泄漏 goroutine。
					reader.Close()
					return PeekOutcome{Consumed: consumed, Err: errStreamIncompleteNoCommit}
				}
				return PeekOutcome{Consumed: consumed, Reader: reader}
			}
			if hasFrame {
				if errorEvent {
					reader.Close()
					return PeekOutcome{Consumed: consumed, Err: errStreamIncompleteNoCommit}
				}
				return PeekOutcome{Consumed: consumed, Reader: reader}
			}
		}
	}
}

// DriveStreamWithRetry 负责把「peek 失败的可重试错误」翻译为最多
// StreamEmptyRetryMax 次重试。每次 attempt（含首轮）都通过 callOnce 拿到
// 一个已建立的流；调用方可以在 callOnce 里把「首轮复用上层已打开的
// firstRC、后续重试再发新请求」的策略封装进去——这样 driver 不需要感知
// first-call 与 retry 的差异。
//
// runOnce 是协议特有的「peek + 主循环」：返回 (true, nil) 表示已 commit；
// 返回 (false, err) 且 err 非 ctx.Canceled/DeadlineExceeded 时，若还
// 有重试额度则换 key 重试。peeked/rd 当前总是 nil——保留这两个形参是
// 为了让后续 protocol PR 能把「peek 与 handler 解耦」时不用改本签名。
//
// 返回约定与 runOnce 一致：(true, nil) 已 commit / (false, err) 全部
// attempt 都未 commit。ctx.Done 与所有错误原样透传。
func DriveStreamWithRetry(
	ctx context.Context,
	w http.ResponseWriter,
	hooks StreamProtocolHooks,
	callOnce func(ctx context.Context) (io.ReadCloser, int, error),
	runOnce func(ctx context.Context, w http.ResponseWriter, rc io.Reader, peeked []streamReadResult, rd *streamReader) (bool, error),
) (bool, error) {
	_ = hooks // hooks 当前由 runOnce 内部的 peek 使用;本参数预留以便后续 protocol PR 不改本签名。
	maxRetry := config.StreamEmptyRetryMax()
	rc, status, err := callOnce(ctx)
	if err != nil {
		if rc != nil {
			rc.Close()
		}
		return false, err
	}
	if status < 200 || status >= 300 {
		if rc != nil {
			rc.Close()
		}
		return false, fmt.Errorf("upstream status %d on retry", status)
	}

	for attempt := 0; attempt <= maxRetry; attempt++ {
		committed, runErr := runOnce(ctx, w, rc, nil, nil)
		if committed {
			return true, nil
		}
		if rc != nil {
			rc.Close()
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return false, runErr
		}
		if attempt < maxRetry {
			logging.FromContext(ctx).Warn("stream empty before commit, retrying with next key",
				"attempt", attempt+1, "max_retry", maxRetry, "cause", runErr)
			nextRC, status, callErr := callOnce(ctx)
			if callErr != nil || status < 200 || status >= 300 {
				if nextRC != nil {
					nextRC.Close()
				}
				if callErr != nil {
					return false, callErr
				}
				return false, fmt.Errorf("upstream status %d on retry", status)
			}
			rc = nextRC
			continue
		}
		return false, runErr
	}
	return false, errStreamIncompleteNoCommit
}
