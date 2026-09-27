package app

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	keyPoolDefaultCooldownSecs   = 60
	keyPoolDefaultBlacklistAfter = 3
	keyPoolLongCooldown          = 15 * time.Minute
)

// keypoolMaxAttempts returns total pool attempts (1+max_retries, default 3).
func keypoolMaxAttempts() int {
	keypoolMu.RLock()
	maxRetries := keypoolCfg.MaxRetries
	keypoolMu.RUnlock()
	if maxRetries <= 0 {
		maxRetries = 2
	}
	return 1 + maxRetries
}

// keypoolAttemptsExhausted reports whether the pool failover budget is spent.
func keypoolAttemptsExhausted(attempt int) bool {
	return attempt+1 >= keypoolMaxAttempts()
}

type keypoolEntryState struct {
	cooldownUntil    time.Time
	consecutiveFails int
}

var (
	keypoolMu      sync.RWMutex
	keypoolCfg     KeyPool
	keypoolEntries []UpstreamKey
	keypoolState   = map[string]*keypoolEntryState{}
	keypoolRRIndex atomic.Uint64
)

// normalizeKeyPool assigns k1,k2… ids to entries missing one, drops empty-key
// and duplicate-id entries (first wins), and applies weight/group defaults.
func normalizeKeyPool(p KeyPool) KeyPool {
	out := p
	out.Keys = nil
	if strings.TrimSpace(out.Strategy) == "" {
		out.Strategy = "round_robin"
	}
	seen := map[string]bool{}
	nextAuto := 1
	for _, k := range p.Keys {
		if strings.TrimSpace(k.Key) == "" {
			continue
		}
		k = k.Normalized()
		if strings.TrimSpace(k.ID) == "" {
			for {
				candidate := "k" + strconv.Itoa(nextAuto)
				nextAuto++
				if !seen[candidate] {
					k.ID = candidate
					break
				}
			}
		}
		if seen[k.ID] {
			continue
		}
		seen[k.ID] = true
		out.Keys = append(out.Keys, k)
	}
	return out
}

// validateKeyPool strictly validates a key_pool section for admin POST:
// unknown strategy, empty key, duplicate id, or weight<1 fails with an
// indexed error. Missing ids are assigned k1,k2… at apply time, so only
// explicit ids participate in the duplicate check. A nil Keys slice means
// the section was absent and is accepted as-is (callers skip validation
// in that case, matching the protocol_rules nil-guard precedent).
func validateKeyPool(p KeyPool) error {
	switch s := strings.ToLower(strings.TrimSpace(p.Strategy)); s {
	case "", "round_robin", "weighted", "sticky":
	default:
		return fmt.Errorf("strategy must be round_robin, weighted, or sticky; got %q", p.Strategy)
	}
	seen := map[string]int{}
	for i, k := range p.Keys {
		if strings.TrimSpace(k.Key) == "" {
			return fmt.Errorf("keys[%d]: key must not be empty", i)
		}
		if k.Weight < 1 {
			return fmt.Errorf("keys[%d]: weight must be >= 1", i)
		}
		id := strings.TrimSpace(k.ID)
		if id == "" {
			continue
		}
		if first, dup := seen[id]; dup {
			return fmt.Errorf("keys[%d]: duplicate id %q (first at keys[%d])", i, id, first)
		}
		seen[id] = i
	}
	return nil
}

// setKeyPool replaces entries and clears runtime state.
func setKeyPool(p KeyPool) {
	p = normalizeKeyPool(p)
	keypoolMu.Lock()
	keypoolCfg = p
	keypoolEntries = p.Keys
	keypoolState = map[string]*keypoolEntryState{}
	keypoolMu.Unlock()
	keypoolRRIndex.Store(0)
}

func poolEnabled() bool {
	keypoolMu.RLock()
	defer keypoolMu.RUnlock()
	return keypoolCfg.Enabled && len(keypoolEntries) > 0
}

// stickySessionBase mirrors stickyKeyForRequest's token part without
// importing body context: account token wins, else the public fallback.
func stickySessionBase(auth UpstreamAuth) string {
	if auth.Token != "" {
		return "tok:" + auth.Token
	}
	return stickyPublicFallback
}

func keyPoolGroupOK(group string, goSurface bool) bool {
	switch group {
	case "":
		return true
	case "go":
		return goSurface
	case "zen":
		return !goSurface
	default:
		return false
	}
}

// selectPoolKey picks a pooled key for this request.
// ok=false means caller falls back to client-token passthrough
// (pool disabled, public auth, or no surface candidate).
func selectPoolKey(auth UpstreamAuth, modelID string, attempt ...int) (UpstreamAuth, string, bool) {
	_ = attempt
	if auth.Mode == AuthRoutePublic {
		return auth, "", false
	}
	keypoolMu.RLock()
	enabled := keypoolCfg.Enabled
	strategy := keypoolCfg.Strategy
	entries := keypoolEntries
	keypoolMu.RUnlock()
	if !enabled || len(entries) == 0 {
		return auth, "", false
	}
	goSurface := auth.shouldUseGoEndpoint(modelID)
	now := time.Now()

	keypoolMu.RLock()
	candidates := make([]UpstreamKey, 0, len(entries))
	for _, e := range entries {
		if !e.IsEnabled() || !keyPoolGroupOK(e.Group, goSurface) {
			continue
		}
		candidates = append(candidates, e)
	}
	states := make(map[string]keypoolEntryState, len(candidates))
	for _, c := range candidates {
		if s, ok := keypoolState[c.ID]; ok {
			states[c.ID] = *s
		}
	}
	keypoolMu.RUnlock()
	if len(candidates) == 0 {
		return auth, "", false
	}

	avail := candidates[:0:0]
	for _, c := range candidates {
		if s, ok := states[c.ID]; !ok || !now.Before(s.cooldownUntil) {
			avail = append(avail, c)
		}
	}
	pool := avail
	if len(pool) == 0 {
		// All cooling: serve the earliest-expiring one, never hard-fail.
		earliest := candidates[0]
		for _, c := range candidates[1:] {
			if states[c.ID].cooldownUntil.Before(states[earliest.ID].cooldownUntil) {
				earliest = c
			}
		}
		pool = []UpstreamKey{earliest}
	}

	var picked UpstreamKey
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "weighted":
		total := 0
		for _, c := range pool {
			total += c.Weight
		}
		if total < 1 {
			total = len(pool)
		}
		slot := int(keypoolRRIndex.Add(1)-1) % total
		for _, c := range pool {
			w := c.Weight
			if w < 1 {
				w = 1
			}
			if slot < w {
				picked = c
				break
			}
			slot -= w
		}
		if picked.ID == "" {
			picked = pool[len(pool)-1]
		}
	case "sticky":
		h := fnv.New32a()
		_, _ = h.Write([]byte(stickySessionBase(auth)))
		picked = pool[int(h.Sum32()%uint32(len(pool)))]
	default: // round_robin
		picked = pool[int(keypoolRRIndex.Add(1)-1)%len(pool)]
	}

	out := auth
	out.Token = picked.Key
	return out, picked.ID, true
}

// reportKeyResult records an upstream attempt for failover accounting.
// Optional body enables billing-error detection (isNonRetryableUpstreamError
// parses the payload); without it, 402/403 fall through to retry_on rules.
func reportKeyResult(id string, status int, transportErr error, body ...[]byte) {
	keypoolMu.RLock()
	cooldownSecs := keypoolCfg.CooldownSecs
	blacklistAfter := keypoolCfg.BlacklistAfter
	retryOn := keypoolCfg.RetryOn
	keypoolMu.RUnlock()
	if cooldownSecs <= 0 {
		cooldownSecs = keyPoolDefaultCooldownSecs
	}
	if blacklistAfter <= 0 {
		blacklistAfter = keyPoolDefaultBlacklistAfter
	}
	now := time.Now()

	keypoolMu.Lock()
	defer keypoolMu.Unlock()
	s := keypoolState[id]
	if s == nil {
		s = &keypoolEntryState{}
		keypoolState[id] = s
	}
	if status >= 200 && status < 300 {
		s.consecutiveFails = 0
		return
	}
	var payload []byte
	if len(body) > 0 {
		payload = body[0]
	}
	if status == http.StatusUnauthorized || isNonRetryableUpstreamError(status, payload) {
		// Invalid token or billing failure: long cooldown.
		s.consecutiveFails++
		s.cooldownUntil = now.Add(keyPoolLongCooldown)
		return
	}
	retryable := transportErr != nil || status == 429 || (status >= 500 && status < 600)
	if !retryable && len(retryOn) > 0 {
		for _, code := range retryOn {
			if code == status {
				retryable = true
				break
			}
		}
	}
	if !retryable {
		return
	}
	s.consecutiveFails++
	if s.consecutiveFails >= blacklistAfter {
		s.cooldownUntil = now.Add(keyPoolLongCooldown)
		return
	}
	s.cooldownUntil = now.Add(time.Duration(cooldownSecs) * time.Second)
}

// KeyPoolEntryStatus is the admin snapshot row for one pooled key.
type KeyPoolEntryStatus struct {
	ID                    string `json:"id"`
	Group                 string `json:"group,omitempty"`
	Weight                int    `json:"weight"`
	Enabled               bool   `json:"enabled"`
	Note                  string `json:"note,omitempty"`
	ConsecutiveFails      int    `json:"consecutive_fails"`
	InCooldown            bool   `json:"in_cooldown"`
	CooldownRemainingSecs int64  `json:"cooldown_remaining_secs"`
}

// keyPoolStatus snapshots pool entries with memory-only cooldown state.
func keyPoolStatus() []KeyPoolEntryStatus {
	now := time.Now()
	keypoolMu.RLock()
	defer keypoolMu.RUnlock()
	out := make([]KeyPoolEntryStatus, 0, len(keypoolEntries))
	for _, e := range keypoolEntries {
		row := KeyPoolEntryStatus{
			ID:      e.ID,
			Group:   e.Group,
			Weight:  e.Weight,
			Enabled: e.IsEnabled(),
			Note:    e.Note,
		}
		if s, ok := keypoolState[e.ID]; ok {
			row.ConsecutiveFails = s.consecutiveFails
			if now.Before(s.cooldownUntil) {
				row.InCooldown = true
				row.CooldownRemainingSecs = int64(s.cooldownUntil.Sub(now).Seconds())
			}
		}
		out = append(out, row)
	}
	return out
}
