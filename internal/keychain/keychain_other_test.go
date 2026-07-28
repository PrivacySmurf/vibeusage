//go:build !darwin

package keychain

import "testing"

func TestReadGenericPassword_PlatformUnsupported(t *testing.T) {
	got, err := ReadGenericPassword("svc", "acct")
	if err == nil {
		t.Fatal("expected error on non-darwin platform")
	}
	if want := "keychain not available on this platform"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}
