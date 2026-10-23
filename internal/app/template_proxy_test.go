package app

import (
	"testing"
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

func TestExpandProxyCredentials(t *testing.T) {
	cases := []struct {
		name    string
		proxy   domain.Socks5Proxy
		scope   string
		wantU   string
		wantP   string
	}{
		{
			name:  "no template passes through",
			proxy: domain.Socks5Proxy{Addr: "p:1080", Username: "user", Password: "pass"},
			scope: "abc123",
			wantU: "user", wantP: "pass",
		},
		{
			name:  "username template replaced",
			proxy: domain.Socks5Proxy{Addr: "p:1080", Username: "user-{{session-id}}", Password: "pass"},
			scope: "deadbeef",
			wantU: "user-deadbeef", wantP: "pass",
		},
		{
			name:  "password template replaced",
			proxy: domain.Socks5Proxy{Addr: "p:1080", Username: "user", Password: "{{session-id}}"},
			scope: "cafebabe",
			wantU: "user", wantP: "cafebabe",
		},
		{
			name:  "both fields replaced",
			proxy: domain.Socks5Proxy{Addr: "p:1080", Username: "{{session-id}}", Password: "{{session-id}}-secret"},
			scope: "1234abcd",
			wantU: "1234abcd", wantP: "1234abcd-secret",
		},
		{
			name:  "empty scope leaves template as-is",
			proxy: domain.Socks5Proxy{Addr: "p:1080", Username: "{{session-id}}", Password: "pass"},
			scope: "",
			wantU: "{{session-id}}", wantP: "pass",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expandProxyCredentials(c.proxy, c.scope)
			if got.Username != c.wantU {
				t.Errorf("Username: got %q want %q", got.Username, c.wantU)
			}
			if got.Password != c.wantP {
				t.Errorf("Password: got %q want %q", got.Password, c.wantP)
			}
			// original must not be mutated
			if c.proxy.Username != c.proxy.Username {
				t.Error("original mutated")
			}
		})
	}
}

func TestIsTemplateProxy(t *testing.T) {
	if isTemplateProxy(domain.Socks5Proxy{Username: "user", Password: "pass"}) {
		t.Error("plain proxy detected as template")
	}
	if !isTemplateProxy(domain.Socks5Proxy{Username: "{{session-id}}", Password: "pass"}) {
		t.Error("template username not detected")
	}
	if !isTemplateProxy(domain.Socks5Proxy{Username: "user", Password: "{{session-id}}"}) {
		t.Error("template password not detected")
	}
}
