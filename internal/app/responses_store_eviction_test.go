package app

import (
	"fmt"
	"strings"
	"testing"
)

func resetStoredResponses(t *testing.T) {
	t.Helper()
	storedResponsesMu.Lock()
	storedResponses = map[string]StoredResponseState{}
	storedOrder = nil
	storedBytes = 0
	storedResponsesMu.Unlock()
	t.Cleanup(func() {
		storedResponsesMu.Lock()
		storedResponses = map[string]StoredResponseState{}
		storedOrder = nil
		storedBytes = 0
		storedResponsesMu.Unlock()
	})
}

func storeTestResponse(id string, outputBytes int) {
	output := []any{map[string]any{
		"type": "message",
		"content": []any{map[string]any{
			"type": "output_text",
			"text": strings.Repeat("o", outputBytes),
		}},
	}}
	storeResponseState(map[string]any{"id": id, "output": output}, ResponsesAPIRequest{Model: "test-model"})
}

func TestStoredResponses_EvictsOldestPastEntryCap(t *testing.T) {
	resetStoredResponses(t)

	total := storedResponseMaxEntries + 10
	for i := range total {
		storeTestResponse(fmt.Sprintf("resp_%04d", i), 8)
	}

	storedResponsesMu.RLock()
	entries := len(storedResponses)
	_, oldestPresent := storedResponses["resp_0000"]
	_, newestPresent := storedResponses[fmt.Sprintf("resp_%04d", total-1)]
	storedResponsesMu.RUnlock()

	if entries > storedResponseMaxEntries {
		t.Fatalf("entries=%d exceeds cap %d", entries, storedResponseMaxEntries)
	}
	if oldestPresent {
		t.Fatalf("oldest entry survived past the entry cap")
	}
	if !newestPresent {
		t.Fatalf("newest entry must stay available for previous_response_id")
	}

	// 最近一轮仍必须能取回，否则续链会静默丢失 tools/instructions/output。
	if _, ok := loadResponseState(fmt.Sprintf("resp_%04d", total-1)); !ok {
		t.Fatalf("loadResponseState failed for the newest response")
	}
}

func TestStoredResponses_EvictsPastByteBudget(t *testing.T) {
	resetStoredResponses(t)

	// 每条输出 1 MiB 的文本，40 条总量远超 32 MiB 预算。
	payload := 1 << 20
	total := 40
	for i := range total {
		storeTestResponse(fmt.Sprintf("resp_%04d", i), payload)
	}

	storedResponsesMu.RLock()
	entries := len(storedResponses)
	bytes := storedBytes
	storedResponsesMu.RUnlock()

	if bytes > storedResponseMaxBytes {
		t.Fatalf("stored bytes=%d exceeds budget %d", bytes, storedResponseMaxBytes)
	}
	if entries >= total {
		t.Fatalf("no eviction happened: %d entries retained", entries)
	}
	if entries == 0 {
		t.Fatalf("budget eviction removed every entry")
	}
	if _, ok := loadResponseState(fmt.Sprintf("resp_%04d", total-1)); !ok {
		t.Fatalf("newest response was evicted")
	}
}

func TestStoredResponses_ByteAccountingTracksOverwrite(t *testing.T) {
	resetStoredResponses(t)

	storeTestResponse("resp_same", 1024)
	storedResponsesMu.RLock()
	first := storedBytes
	storedResponsesMu.RUnlock()

	// 同 ID 覆盖（重试/重复写入）不得重复计数。
	storeTestResponse("resp_same", 1024)
	storedResponsesMu.RLock()
	second := storedBytes
	entries := len(storedResponses)
	storedResponsesMu.RUnlock()

	if second != first {
		t.Fatalf("byte counter drifted on overwrite: %d -> %d", first, second)
	}
	if entries != 1 {
		t.Fatalf("entries=%d want 1", entries)
	}
}

func TestStoredResponses_StoreFalseDoesNotEnterStore(t *testing.T) {
	resetStoredResponses(t)

	off := false
	storeResponseState(map[string]any{
		"id":     "resp_no_store",
		"output": []any{map[string]any{"type": "message"}},
	}, ResponsesAPIRequest{Model: "test-model", Store: &off})

	storedResponsesMu.RLock()
	entries := len(storedResponses)
	storedResponsesMu.RUnlock()
	if entries != 0 {
		t.Fatalf("store:false response entered the store")
	}
}
