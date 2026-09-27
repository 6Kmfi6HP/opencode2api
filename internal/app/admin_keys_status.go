package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// keyPoolStatusHandler serves GET /api/key_status behind requireAuth,
// returning the live pool snapshot plus the pool toggle/strategy.
func keyPoolStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	keypoolMu.RLock()
	enabled := keypoolCfg.Enabled
	strategy := keypoolCfg.Strategy
	keypoolMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"keys": keyPoolStatus(),
		"pool": map[string]any{"enabled": enabled, "strategy": strategy},
	})
}

// keyPoolParseHandler serves POST /api/key_parse behind requireAuth: the
// single canonical implementation of the batch textarea grammar. The panel
// delegates parsing here so client and server can never drift.
func keyPoolParseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if len(payload.Text) > 1024*1024 {
		http.Error(w, `{"error":"batch text too large (max 1MiB)"}`, http.StatusBadRequest)
		return
	}
	keys, added, skipped := parseBatchKeys(payload.Text)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"keys":    keys,
		"added":   added,
		"skipped": skipped,
	})
}

// parseBatchKeys parses pasted batch key lines into pool entries.
//
// Grammar per line: key [| note] [| group] [| weight].
// Lines are trimmed; blank lines and `#` comments are silently ignored
// (not counted as skipped). Group is lowercased and kept only for zen/go,
// otherwise cleared. Weight defaults to 1 when absent; a present but
// unparseable or <1 weight invalidates the line. Lines with an empty key
// or more than 4 columns are invalid. skipped counts only invalid
// (non-blank, non-comment) lines.
func parseBatchKeys(text string) (keys []UpstreamKey, added, skipped int) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) > 4 {
			skipped++
			continue
		}
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}
		if cols[0] == "" {
			skipped++
			continue
		}
		entry := UpstreamKey{Key: cols[0], Weight: 1}
		if len(cols) > 1 {
			entry.Note = cols[1]
		}
		if len(cols) > 2 {
			if g := strings.ToLower(cols[2]); g == "zen" || g == "go" {
				entry.Group = g
			}
		}
		if len(cols) > 3 && cols[3] != "" {
			w, err := strconv.Atoi(cols[3])
			if err != nil || w < 1 {
				skipped++
				continue
			}
			entry.Weight = w
		}
		keys = append(keys, entry)
		added++
	}
	return keys, added, skipped
}
