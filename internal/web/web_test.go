package web

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryansghost/journal-introspection/internal/config"
	"github.com/ryansghost/journal-introspection/internal/grader"
	"github.com/ryansghost/journal-introspection/internal/indexer"
	"github.com/ryansghost/journal-introspection/internal/store"
)

const testSecret = "JBSWY3DPEHPK3PXP"

func newSite(t *testing.T, authDisabled bool) (*httptest.Server, *http.Client) {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../testdata/org")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(root+"/../outside.txt", []byte("outside the root"), 0o644)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{OrgRoot: root, Exclude: []string{"crypto.org"}, PasswordHash: HashPassword("correct horse"),
		TOTPSecret: testSecret, AuthDisabled: authDisabled, ScanInterval: time.Minute}
	ix := indexer.New(db, root, cfg.Exclude)
	if _, err := ix.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	rubric, _ := grader.LoadRubric("")
	s, err := New(cfg, db, ix, grader.New(db, rubric, "", "kev-test", "kev-test"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); db.Close() })
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func TestPasswordAndTOTP(t *testing.T) {
	h := HashPassword("hunter2hunter2")
	if !CheckPassword(h, "hunter2hunter2") || CheckPassword(h, "hunter3") || CheckPassword("garbage", "x") {
		t.Error("password check is wrong")
	}
	// RFC 6238 test vector: secret "12345678901234567890", T=59s -> 94287082 (8 digits), so 287082.
	code, err := totpCode("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", 59/30)
	if err != nil || code != "287082" {
		t.Errorf("totp vector: got %q, %v", code, err)
	}
	now := time.Unix(1_800_000_000, 0)
	cur, _ := totpCode(testSecret, now.Unix()/30)
	old, _ := totpCode(testSecret, now.Unix()/30-5)
	if _, ok := CheckTOTP(testSecret, cur, now); !ok {
		t.Error("current code refused")
	}
	if _, ok := CheckTOTP(testSecret, old, now); ok {
		t.Error("stale code accepted")
	}
}

func TestLoginRequired(t *testing.T) {
	ts, c := newSite(t, false)
	for _, p := range []string{"/", "/journal/2026-11-13", "/files/", "/files/life.org", "/search?q=a", "/status"} {
		if code, _ := get(t, c, ts.URL+p); code != http.StatusSeeOther {
			t.Errorf("%s without a session: want 303, got %d", p, code)
		}
	}
	if code, _ := get(t, c, ts.URL+"/healthz"); code != 200 {
		t.Errorf("healthz: %d", code)
	}

	code, _ := totpCode(testSecret, time.Now().Unix()/30)
	post := func(pass, otp string, hdr map[string]string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(url.Values{"password": {pass}, "code": {otp}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("correct horse", code, map[string]string{"Sec-Fetch-Site": "cross-site"}); got != http.StatusForbidden {
		t.Errorf("cross-site post: want 403, got %d", got)
	}
	if got := post("correct horse", "000000", nil); got != http.StatusUnauthorized {
		t.Errorf("wrong code: want 401, got %d", got)
	}
	if got := post("wrong", code, nil); got != http.StatusUnauthorized {
		t.Errorf("wrong password: want 401, got %d", got)
	}
	if got := post("correct horse", code, nil); got != http.StatusSeeOther {
		t.Fatalf("good login: want 303, got %d", got)
	}
	// The same code a second time is a replay, even with the right password.
	if got := post("correct horse", code, nil); got != http.StatusUnauthorized {
		t.Errorf("replayed code: want 401, got %d", got)
	}
	if code, body := get(t, c, ts.URL+"/journal/2026-11-13"); code != 200 || !strings.Contains(body, "chicken and sweet potatoes") {
		t.Errorf("journal after login: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	ts, c := newSite(t, false)
	last := 0
	for i := 0; i < loginMaxFails+1; i++ {
		resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"nope"}, "code": {"000000"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d failures: want 429, got %d", loginMaxFails, last)
	}
}

func TestPages(t *testing.T) {
	ts, c := newSite(t, true)
	check := func(path string, wantCode int, wants ...string) {
		t.Helper()
		code, body := get(t, c, ts.URL+path)
		if code != wantCode {
			t.Errorf("%s: want %d, got %d", path, wantCode, code)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: page lacks %q", path, w)
			}
		}
	}
	check("/", 200, "Friday, 13 November 2026", "<svg class=\"chart\"", "Evening notes", "<table>", "Earlier")
	check("/journal/2026-11-12", 200, "Slept badly")
	check("/journal/2026-11-11", 404)
	check("/journal/garbage", 404)
	check("/files/", 200, "journal/", "projects/", "quicknote.txt")
	check("/files/projects/introspection.org", 200, "Mac mini", "<table>")
	check("/files/quicknote.txt", 200, "plain text scratch")
	// Links between org files point inside the app; links to excluded files do not.
	check("/files/projects/introspection.org", 200, `href="/files/quicknote.txt"`, `href="/files/roam/20240101120000-zettel.org"`)
	if _, body := get(t, c, ts.URL+"/files/projects/introspection.org"); strings.Contains(body, `href="/files/crypto.org"`) {
		t.Error("link to an excluded file was resolved")
	}
	check("/search?q=zettelkasten", 200, "<mark>zettelkasten</mark>")
	check("/search?q=%22+OR+NEAR(", 200) // FTS syntax in the query must not break it
	check("/status", 200, "3 journal")

	// Excluded files and anything outside the root do not exist.
	_, listing := get(t, c, ts.URL+"/files/")
	if strings.Contains(listing, "crypto") {
		t.Error("excluded file is listed")
	}
	check("/files/crypto.org", 404)
	check("/search?q=seed", 200, "Nothing matches")
	check("/files/..%2Foutside.txt", 404)
	check("/files/%2e%2e/outside.txt", 404)

	resp, _ := c.Get(ts.URL + "/")
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("missing CSP: %q", csp)
	}
}
