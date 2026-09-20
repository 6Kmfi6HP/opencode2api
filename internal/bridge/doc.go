// Package bridge holds the pure protocol-conversion logic extracted from
// internal/app. It translates between the inbound OpenAI Chat / Responses and
// Anthropic Messages wire shapes and the OpenCode upstream wire shape, and
// back, without performing any I/O, logging, metrics, or configuration reads.
//
// # Purity contract
//
// This package is a pure function / value layer. It may ONLY import:
//
//   - the Go standard library (with the single exception of net/http, which
//     is forbidden to keep this layer free of any HTTP coupling);
//   - github.com/6Kmfi6HP/opencode2api/internal/util;
//   - github.com/6Kmfi6HP/opencode2api/internal/random;
//   - github.com/6Kmfi6HP/opencode2api/internal/domain.
//
// It must NOT import net/http, internal/stats, internal/logging, or
// internal/config, and it must never import internal/app. The dependency
// direction is strictly app -> bridge -> domain/util/random.
//
// # Side-effect channels
//
// Instead of touching side-effecting dependencies directly, every external
// concern is injected or returned:
//
//   - HTTP streaming: bridge produces OutEvent values; the app shell
//     serializes them and performs the ResponseWriter/Flusher writes.
//   - Stream statistics: bridge tracks plain counters internally; the app
//     reads a snapshot at end-of-stream and assembles/logging StreamStats.
//   - Stats recording: bridge returns usage values; the app calls
//     statsx.Record* itself.
//   - Configuration: bridge receives a ConfigView snapshot instead of reading
//     the global config under configMu.
//   - ID generation and timestamps: bridge receives an idGen function and a
//     nowFn function (production defaults wrap internal/random and
//     time.Now().Unix).
//
// The TestNoForbiddenImports test in this package enforces the import
// allow-list mechanically.
package bridge
