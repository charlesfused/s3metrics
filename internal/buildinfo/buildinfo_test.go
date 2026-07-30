package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestIsDevWhenUnstamped(t *testing.T) {
	if !IsDev() {
		t.Fatalf("IsDev() = false for an unstamped build; Version = %q", Version)
	}
}

func TestIsDevFalseWhenStamped(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "v1.2.3"
	if IsDev() {
		t.Fatal("IsDev() = true for stamped version v1.2.3")
	}
}

func TestStringIncludesVersionAndPlatform(t *testing.T) {
	got := String()
	for _, want := range []string{Version, Commit, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
