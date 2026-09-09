package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

func TestModelsDevProxyRouting(t *testing.T) {
	// Full reset before and after: FetchCatalog uses the live client getter,
	// so it must restore the blocking TestMain transport for later tests.
	modelsdev.ResetForTest()
	t.Cleanup(modelsdev.ResetForTest)
	modelsdev.SetClientGetter(getHTTPClient)

	var proxyHits int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":{"test/proxy-model":{"id":"test/proxy-model","limit":{"context":32768}}}}`))
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	socks5Mu.Lock()
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	socks5Proxies = nil
	activeSocks5 = ""
	socks5Mu.Unlock()

	oldTransport := httpClient.Transport
	httpClient.Transport = &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	t.Cleanup(func() {
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5Mu.Unlock()
		httpClient.Transport = oldTransport
	})

	modelsdev.SetCatalogURL("http://models.dev/catalog.json")

	cat, err := modelsdev.FetchCatalog()
	if err != nil {
		t.Fatalf("modelsdev.FetchCatalog failed: %v", err)
	}

	if hits := atomic.LoadInt32(&proxyHits); hits == 0 {
		t.Fatalf("expected proxy hits > 0, got %d", hits)
	}

	if ctx := cat["proxy-model"]; ctx != 32768 {
		t.Fatalf("catalog[proxy-model] = %d, want 32768", ctx)
	}
}
