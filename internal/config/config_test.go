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
	if !slices.Contains(c.Exclude, "crypto.org") || !slices.Contains(c.Exclude, "invoices/**") {
		t.Errorf("default exclude list: %v", c.Exclude)
	}
	t.Setenv("EXCLUDE", " people.org , scratch/** ,")
	if c, _ = Load(); !slices.Equal(c.Exclude, []string{"people.org", "scratch/**"}) {
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
