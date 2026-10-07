package app

import "testing"

// 动态模型连续透传失败达到阈值后自动剔除（故障自愈）；静态模型永不剔除。
func TestNativeResponsesFailureEviction(t *testing.T) {
	const flaky = "flaky-dynamic-model-xyz"
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[flaky] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, flaky)
		nativeResponsesModels.Unlock()
	})
	for i := 0; i < nativeResponsesEvictAfter; i++ {
		markNativeResponsesFailure(flaky)
	}
	if isNativeResponsesModel(flaky) {
		t.Fatal("dynamic model should be evicted after consecutive failures")
	}
	markNativeResponsesFailure("muse-spark-1.3-contributor")
	markNativeResponsesFailure("muse-spark-1.3-contributor")
	if !isNativeResponsesModel("muse-spark-1.3-contributor") {
		t.Fatal("static model must never be evicted")
	}
	// contributor-free 系列（上游 chat/completions 整档 500，只能走原生
	// responses）经模式匹配命中，同样永不剔除
	for _, m := range []string{"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free"} {
		if !isNativeResponsesModel(m) {
			t.Fatalf("contributor-free model %q must be native via static pattern", m)
		}
		for range nativeResponsesEvictAfter {
			markNativeResponsesFailure(m)
		}
		if !isNativeResponsesModel(m) {
			t.Fatalf("contributor-free model %q must never be evicted", m)
		}
	}
	if isNativeResponsesModel("muse-spark-1.3-contributor-preview") {
		t.Fatal("preview suffix must NOT match the contributor pattern (different billing tier)")
	}
}
