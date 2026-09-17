package app

import (
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The upstream free tier only accepts requests whose x-opencode-session
// matches the OpenCode client ID shape: "ses_" + 12 hex chars + 14 base62
// chars. Anything else is rejected with 403 FreeTierError ("OpenCode's free
// tier can only be used from within OpenCode"). The request ID uses the same
// 26-char body with a "msg_" prefix.
func TestOpenCode_SessionIDMatchesClientFormat(t *testing.T) {
	var re = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 1000; i++ {
		id := newOCSessionID()
		if !re.MatchString(id) {
			t.Fatalf("session id %q does not match client format", id)
		}
	}
}

func TestOpenCode_RequestIDMatchesClientFormat(t *testing.T) {
	var re = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 1000; i++ {
		id := newOCRequestID()
		if !re.MatchString(id) {
			t.Fatalf("request id %q does not match client format", id)
		}
	}
}

// The timestamp prefix derives from the current wall clock; consecutive
// generations must stay distinct (counter + random suffix).
func TestOpenCode_DescendingIDUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := opencodeDescendingID()
		if seen[id] {
			t.Fatalf("duplicate id %q generated", id)
		}
		seen[id] = true
	}
}

// The 12-hex prefix encodes ^(now*0x1000+counter) truncated to 48 bits
// (matching TypeID's "descending" algorithm). Only the low 48 bits of
// now*0x1000 carry through, so the recoverable timestamp domain is ms
// mod 2^36 (~790 days). Inverting within the 48-bit domain (MASK48 - v)
// and shifting off the 12-bit counter must recover "now mod 2^36"; a
// regression that drops the inversion or mask would not round-trip.
func TestOpenCode_DescendingIDTimestampRoundTrip(t *testing.T) {
	before := time.Now().UnixMilli()
	id := newOCSessionID()
	after := time.Now().UnixMilli()

	v, err := strconv.ParseUint(id[4:16], 16, 64)
	if err != nil {
		t.Fatalf("session id %q prefix is not hex: %v", id, err)
	}
	const domain int64 = 1 << 36
	recovered := int64((0xFFFFFFFFFFFF - v) >> 12)
	// Distance in the 2^36 modular domain — handles the wrap boundary too.
	dist := (recovered - before%domain + domain) % domain
	if dist > domain/2 {
		dist = domain - dist
	}
	if dist > 5000 {
		t.Fatalf("session id %q recovers ms≡%d (mod 2^36), want wall-clock %d±5000 (mod 2^36)",
			id, recovered, before%domain)
	}
	// Counter is rand.Int64N(0xFFF)+1 per call — intra-ms ordering is not
	// guaranteed to be monotonic (and upstream only checks the format regex,
	// not relative order), so we skip same-ms ordering assertions. The
	// round-trip ms check above is the load-bearing invariant.
	_ = after
}

// buildOCRequestWithSubpath must send the client-shaped headers: UA version,
// client tag, session and request IDs.
func TestOpenCode_RequestHeadersMirrorClient(t *testing.T) {
	ocClientVer = ocMinFreeTierVersion
	ocSessionID = newOCSessionID()
	ocProjectID = randomHex(40)
	auth := UpstreamAuth{}
	body := map[string]any{"messages": []any{}}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", body, auth, false, "https://opencode.ai", "chat/completions", ocSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "opencode/"+ocMinFreeTierVersion {
		t.Fatalf("User-Agent = %q, want opencode/%s", got, ocMinFreeTierVersion)
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, want cli", got)
	}
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-session")) {
		t.Fatalf("x-opencode-session = %q has wrong format", req.Header.Get("x-opencode-session"))
	}
	if !regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-request")) {
		t.Fatalf("x-opencode-request = %q has wrong format", req.Header.Get("x-opencode-request"))
	}
}
