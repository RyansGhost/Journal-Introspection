// Package config reads all settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	OrgRoot      string        // ORG_ROOT: directory of org files, mounted read-only
	DataDir      string        // DATA_DIR: where the SQLite database lives
	Addr         string        // ADDR: listen address
	ScanInterval time.Duration // SCAN_INTERVAL: how often to look for changed files
	Exclude      []string      // EXCLUDE: comma-separated globs never indexed or served

	KevURL     string // KEV_URL: base URL of the Kev server; empty disables grading
	KevModel   string // KEV_MODEL: model name sent to Kev and stored with every grade
	RubricPath string // RUBRIC: optional path to a rubric.json overriding the built-in one

	PasswordHash string // AUTH_PASSWORD_HASH: from `introspect hash-password`
	TOTPSecret   string // AUTH_TOTP_SECRET: base32; empty disables the second factor
	AuthDisabled bool   // AUTH_DISABLED=1: no login at all (LAN or local development only)
	CookieSecure bool   // COOKIE_INSECURE=1 turns this off for plain-http development
	TrustProxy   bool   // TRUST_PROXY=1: take the client IP from X-Forwarded-For
}

// DefaultExclude keeps the most sensitive files out of the index entirely.
const DefaultExclude = "identities.org,crypto.org,finance*.org,invoices/**,bib/**,data/**,tmp/**"

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func Load() (Config, error) {
	c := Config{
		OrgRoot:      env("ORG_ROOT", "/org"),
		DataDir:      env("DATA_DIR", "/data"),
		Addr:         env("ADDR", ":8080"),
		KevURL:       strings.TrimRight(env("KEV_URL", ""), "/"),
		KevModel:     env("KEV_MODEL", "kev-latest"),
		RubricPath:   env("RUBRIC", ""),
		PasswordHash: env("AUTH_PASSWORD_HASH", ""),
		TOTPSecret:   env("AUTH_TOTP_SECRET", ""),
		AuthDisabled: env("AUTH_DISABLED", "") == "1",
		CookieSecure: env("COOKIE_INSECURE", "") != "1",
		TrustProxy:   env("TRUST_PROXY", "") == "1",
	}
	d, err := time.ParseDuration(env("SCAN_INTERVAL", "2m"))
	if err != nil || d < time.Second {
		return c, fmt.Errorf("SCAN_INTERVAL: want a duration like 2m, got %q", env("SCAN_INTERVAL", ""))
	}
	c.ScanInterval = d
	for _, p := range strings.Split(env("EXCLUDE", DefaultExclude), ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Exclude = append(c.Exclude, p)
		}
	}
	if c.PasswordHash == "" && !c.AuthDisabled {
		return c, fmt.Errorf("AUTH_PASSWORD_HASH is not set; run `introspect hash-password`, or set AUTH_DISABLED=1 for private networks")
	}
	return c, nil
}
