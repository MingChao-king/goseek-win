package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"goseek/internal/domain"
)

func TestLegacyBrowserCanOnlyBeClaimedByOneSession(t *testing.T) {
	server := &Server{
		dataDirectory: t.TempDir(),
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	first := domain.SessionID("ses_first")
	second := domain.SessionID("ses_second")

	claimed, err := server.claimLegacyBrowser(first)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	claimed, err = server.claimLegacyBrowser(first)
	if err != nil || !claimed {
		t.Fatalf("repeat claim = %v, %v", claimed, err)
	}
	claimed, err = server.claimLegacyBrowser(second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("second session claimed legacy browser")
	}
	owner, err := server.legacyBrowserOwner()
	if err != nil || owner != first {
		t.Fatalf("owner = %q, %v", owner, err)
	}
}

func TestSessionBrowserDirectoryIsPerSession(t *testing.T) {
	server := &Server{dataDirectory: t.TempDir()}
	first := server.sessionBrowserDirectory("ses_first")
	second := server.sessionBrowserDirectory("ses_second")

	if first == second {
		t.Fatal("different sessions share browser directory")
	}
	if first != filepath.Join(server.dataDirectory, "browsers", "ses_first") {
		t.Fatalf("first directory = %q", first)
	}
}

func TestCloseBrowserTabDoesNotStartMissingSessionBrowser(t *testing.T) {
	dataDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDirectory, "browser-legacy-owner"), []byte("ses_other\n"), 0o600); err != nil {
		t.Fatalf("write legacy owner: %v", err)
	}
	server := &Server{
		dataDirectory: dataDirectory,
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	request := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/browser/tabs/missing-target?sessionId=ses_missing",
		nil,
	)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(server.sessionBrowserDirectory("ses_missing")); !os.IsNotExist(err) {
		t.Fatalf("close request created browser profile, stat error = %v", err)
	}
}
