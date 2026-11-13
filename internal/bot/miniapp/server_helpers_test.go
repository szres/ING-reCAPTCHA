package miniapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// server_helpers_test.go covers small helpers that aren't reachable
// through the main /v/init + /v/answer paths: guessContentType,
// serveStatic (including its path-traversal guard), the static-route
// dispatch in handleStaticOrAPI, ExpireStaleChallenges, and the HTML
// shell at /v. These are the bits a typo could quietly break without
// failing any of the higher-level integration tests.

func TestGuessContentType(t *testing.T) {
	cases := map[string]string{
		"app.js":           "application/javascript; charset=utf-8",
		"styles.css":       "text/css; charset=utf-8",
		"icon.svg":         "image/svg+xml",
		"img.png":          "image/png",
		"index.html":       "text/html; charset=utf-8",
		"unknown.bin":      "application/octet-stream",
		"":                 "application/octet-stream",
		"weird.name.xyz":   "application/octet-stream",
		"app.JS":           "application/octet-stream", // matcher is case-sensitive on purpose
		"styles.css.bak":   "application/octet-stream",
		"deeply/nested.js": "application/javascript; charset=utf-8",
	}
	for name, want := range cases {
		assert.Equalf(t, want, guessContentType(name), "guessContentType(%q)", name)
	}
}

func TestServeStaticRoutesEmbeddedAssets(t *testing.T) {
	db := newServerTestDB(t)
	s := New("", Deps{DB: db, BotToken: testBotToken})

	tests := []struct {
		path        string
		wantStatus  int
		wantCT      string
		bodyContain string
	}{
		// The Mini App ships three embedded assets — verify every
		// one is reachable through the route table so a packaging
		// regression that drops a file fails CI.
		{"/v/static/app.js", http.StatusOK, "application/javascript; charset=utf-8", "Telegram"},
		{"/v/static/styles.css", http.StatusOK, "text/css; charset=utf-8", "--tg-theme-bg-color"},
		{"/v/static/index.html", http.StatusOK, "text/html; charset=utf-8", "<title>"},
		// Missing file → 404 (not 500).
		{"/v/static/does-not-exist.js", http.StatusNotFound, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantCT != "" {
				assert.Equal(t, tc.wantCT, rec.Header().Get("Content-Type"))
				assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
					"static assets must opt out of caching so a re-deploy is picked up immediately")
				assert.Contains(t, rec.Body.String(), tc.bodyContain)
			}
		})
	}
}

func TestServeStaticRejectsPathTraversal(t *testing.T) {
	// Mini App static handler must refuse `..` segments to prevent
	// reading arbitrary files via embed.FS. fs.Sub already restricts
	// to the embed root, but defense-in-depth is cheap and the
	// explicit reject also catches typoed routes earlier.
	//
	// We bypass the mux here — http.ServeMux normalises `..` segments
	// to a 301 BEFORE reaching the handler, which is itself a defense
	// but isn't what we're testing. We want to prove serveStatic's
	// inner guard works even when called with a name containing `..`,
	// which is the threat model if a future refactor inlines the
	// route differently.
	db := newServerTestDB(t)
	s := New("", Deps{DB: db, BotToken: testBotToken})

	for _, name := range []string{
		"../server.go",
		"sub/../../etc/passwd",
		"../../etc/passwd",
	} {
		rec := httptest.NewRecorder()
		s.serveStatic(rec, httptest.NewRequest(http.MethodGet, "/v/static/x", nil), name, "text/plain")
		assert.Equal(t, http.StatusNotFound, rec.Code, "serveStatic must reject %q", name)
	}
}

func TestHandleStaticOrAPIMethodFiltering(t *testing.T) {
	db := newServerTestDB(t)
	s := New("", Deps{DB: db, BotToken: testBotToken})

	// /v/static/* is GET-only — a POST hits the default 404 branch.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v/static/app.js", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// /v/init / /v/answer are POST-only; a GET goes to 404 too.
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v/init", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleIndexServesShellOnExactPath(t *testing.T) {
	db := newServerTestDB(t)
	s := New("", Deps{DB: db, BotToken: testBotToken})

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.True(t,
		strings.Contains(body, "<title>") && strings.Contains(body, "telegram-web-app.js"),
		"index must include the Telegram WebApp SDK so initData is populated")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	// /v/anything-else must NOT serve the shell.
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v/nonsense", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExpireStaleChallengesDropsOnlyExpired(t *testing.T) {
	// ExpireStaleChallenges is called from the bot's cleanup ticker;
	// missing this guard would leak temp PNGs forever. We assert that
	// stale entries are removed AND that CleanupTempFile is invoked
	// exactly once per dropped challenge (not zero, not twice).
	db := newServerTestDB(t)
	var cleanupCalls int32
	var cleanupPaths []string
	s := New("", Deps{
		DB:           db,
		BotToken:     testBotToken,
		CleanupTempFile: func(p string) {
			atomic.AddInt32(&cleanupCalls, 1)
			cleanupPaths = append(cleanupPaths, p)
		},
	})

	// Two stale challenges + one fresh; only the stale ones should
	// be dropped.
	now := time.Now()
	s.challenges.Store("stale-1", &challengeState{
		composedPath: "/tmp/stale-1.png",
		expiresAt:    now.Add(-1 * time.Minute),
	})
	s.challenges.Store("stale-2", &challengeState{
		composedPath: "/tmp/stale-2.png",
		expiresAt:    now.Add(-5 * time.Second),
	})
	s.challenges.Store("fresh", &challengeState{
		composedPath: "/tmp/fresh.png",
		expiresAt:    now.Add(5 * time.Minute),
	})

	s.ExpireStaleChallenges()

	// Fresh entry must remain.
	_, freshOK := s.challenges.Load("fresh")
	assert.True(t, freshOK, "fresh challenge must NOT be dropped")
	// Stale entries must be gone.
	_, stale1OK := s.challenges.Load("stale-1")
	_, stale2OK := s.challenges.Load("stale-2")
	assert.False(t, stale1OK)
	assert.False(t, stale2OK)
	assert.Equal(t, int32(2), atomic.LoadInt32(&cleanupCalls),
		"CleanupTempFile must fire exactly once per dropped challenge")
	assert.ElementsMatch(t, []string{"/tmp/stale-1.png", "/tmp/stale-2.png"}, cleanupPaths)
}

func TestExpireStaleChallengesSkipsEmptyPath(t *testing.T) {
	// A challenge without a composedPath (e.g. failed compose before
	// the file was created) must still be deleted from the map, but
	// must NOT call CleanupTempFile("") — that would dispatch a
	// remove against an empty path and could mask real bugs in the
	// cleanup function.
	db := newServerTestDB(t)
	var calls int32
	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		CleanupTempFile: func(string) { atomic.AddInt32(&calls, 1) },
	})

	s.challenges.Store("empty", &challengeState{
		composedPath: "",
		expiresAt:    time.Now().Add(-time.Second),
	})

	s.ExpireStaleChallenges()

	_, ok := s.challenges.Load("empty")
	require.False(t, ok)
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"CleanupTempFile must NOT be invoked for entries without a temp file")
}
