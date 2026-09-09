package app

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

// TestMain keeps the app test suite hermetic: tests that reach
// modelsdev.GetCachedModalities through modelIsTextOnly must inject state via
// modelsdev.SetModalitiesForTest instead of hitting the real catalog. Tests
// that need network behavior point modelsdev at their own httptest server.
func TestMain(m *testing.M) {
	installBlocker()
	os.Exit(m.Run())
}

func installBlocker() {
	modelsdev.ResetForTest()
	modelsdev.SetClientGetter(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("app test attempted a real models.dev fetch; use SetModalitiesForTest or an httptest catalog URL")
		})}
	})
}

// ResetForTest on modelsdev also resets the client getter, so re-install the
// blocking transport before each test that manipulates modelsdev state.
func blockModelsDevTransport(t *testing.T) {
	t.Helper()
	installBlocker()
	t.Cleanup(installBlocker)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
