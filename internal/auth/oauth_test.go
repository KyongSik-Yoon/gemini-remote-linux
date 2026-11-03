package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCredentialsFromApp(t *testing.T) {
	tmpDir := t.TempDir()
	macOSDir := filepath.Join(tmpDir, "Gemini.app", "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(macOSDir, "Gemini")

	wantID := "123456789012-abcdefghijklmnopqrstuvwx12345678.apps.googleusercontent.com"
	wantSecret := "GOCSPX-TestSecretValue1234567890ABC"
	xorKey := byte(0x5a)
	maskedSecret := make([]byte, len(wantSecret))
	for i := 0; i < len(wantSecret); i++ {
		maskedSecret[i] = wantSecret[i] ^ xorKey
	}

	var payload []byte
	payload = append(payload, make([]byte, 128)...)
	payload = append(payload, []byte(wantID)...)
	payload = append(payload, make([]byte, 64)...)
	payload = append(payload, maskedSecret...)
	payload = append(payload, make([]byte, 128)...)

	if err := os.WriteFile(binPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	gotID, gotSecret, err := ExtractCredentialsFromApp(filepath.Join(tmpDir, "Gemini.app"))
	if err != nil {
		t.Fatalf("ExtractCredentialsFromApp 실패: %v", err)
	}
	if gotID != wantID {
		t.Fatalf("ClientID 불일치: got=%q, want=%q", gotID, wantID)
	}
	if gotSecret != wantSecret {
		t.Fatalf("ClientSecret 불일치: got=%q, want=%q", gotSecret, wantSecret)
	}
}
