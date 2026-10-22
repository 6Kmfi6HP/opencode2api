package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 线上排障用的调试端点，默认不启用。
//
// 背景(2026-10-22 oracle-1/oracle-2 崩溃排查)：网关在高并发下 OS 线程数会长到
// 上千，顶到容器 pids 上限时 Go 抛 `fatal error: newosproc` 直接退出，把在途
// SSE 流全部斩断（下游表现为 unexpected EOF / 502）。但 /proc 只能看到线程
// **事后**停在 futex，看不到是谁把它们撑起来的 —— 需要进程内的 goroutine 栈。
//
// 启用方式：OPENCODE2API_DEBUG_ADDR=127.0.0.1:6060（务必绑回环或内网地址：
// /debug/pprof 会暴露完整调用栈与堆内容）。提供：
//
//	/debug/threads              线程/协程数量与运行时概览（JSON）
//	/debug/pprof/goroutine?debug=1   当前全部 goroutine 栈（定位阻塞点）
//	/debug/pprof/profile?seconds=30 CPU profile
//	/debug/pprof/heap, /trace, /cmdline, /symbol
//
// 该监听与主 API 完全分离，不影响鉴权面。
func startDebugServer() {
	addr := strings.TrimSpace(os.Getenv("OPENCODE2API_DEBUG_ADDR"))
	if addr == "" {
		return
	}

	mux := newDebugMux()

	go func() {
		slog.Warn("debug server listening (pprof exposed)", "addr", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			slog.Error("debug server stopped", "addr", addr, "error", err)
		}
	}()
}

// newDebugMux 组装调试端点路由（拆出来便于测试）。
func newDebugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/threads", handleDebugThreads)
	// 手动注册，避免 import _ "net/http/pprof" 把端点挂到 DefaultServeMux 上
	// （主 API 若复用 DefaultServeMux 会让 pprof 无鉴权暴露）。
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// handleDebugThreads 返回运行时概览：goroutine 数、OS 线程数（Linux 读
// /proc/self/status，其它平台为 -1）、GOMAXPROCS、堆占用。
//
// 线程数持续单调上涨而 goroutine 数平稳 = M 只增不减（阻塞 syscall 撑起来的
// M 不会被运行时回收），此时应抓 /debug/pprof/goroutine 看谁在阻塞。
func handleDebugThreads(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := map[string]any{
		"goroutines":       runtime.NumGoroutine(),
		"threads":          currentThreads(),
		"gomaxprocs":       runtime.GOMAXPROCS(0),
		"heap_inuse_bytes": ms.HeapInuse,
		"time":             time.Now().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// currentThreads 读取本进程的 OS 线程数；非 Linux 或读取失败返回 -1。
func currentThreads() int {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Threads:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Threads:")))
		if err != nil {
			return -1
		}
		return n
	}
	return -1
}
