package config

import (
	"slices"
	"testing"
)

func TestExcludeFromEnv(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "1")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Exclude, "private/**") || !slices.Contains(c.Exclude, "secret*.org") {
		t.Errorf("default exclude list: %v", c.Exclude)
	}
	t.Setenv("EXCLUDE", " diary-private.org , drafts/** ,")
	if c, _ = Load(); !slices.Equal(c.Exclude, []string{"diary-private.org", "drafts/**"}) {
		t.Errorf("EXCLUDE replaces the list: %v", c.Exclude)
	}
	t.Setenv("EXCLUDE", "")
	if c, _ = Load(); len(c.Exclude) != 0 {
		t.Errorf("empty EXCLUDE should exclude nothing: %v", c.Exclude)
	}
}

func TestPasswordRequired(t *testing.T) {
	if _, err := Load(); err == nil {
		t.Error("want an error when no password hash is set and auth is on")
	}
}
