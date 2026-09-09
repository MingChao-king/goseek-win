package main

import "testing"

func TestBrowserAPIPathUsesSessionEnvironment(t *testing.T) {
	t.Setenv("GOSEEK_SESSION_ID", "ses_one/two")

	got := browserAPIPath("/api/v1/browser/read-page")
	want := "/api/v1/browser/read-page?sessionId=ses_one%2Ftwo"
	if got != want {
		t.Fatalf("browserAPIPath = %q, want %q", got, want)
	}
}

func TestBrowserAPIPathFallsBackToDefaultSession(t *testing.T) {
	t.Setenv("GOSEEK_SESSION_ID", "")

	got := browserAPIPath("/api/v1/browser/tabs")
	want := "/api/v1/browser/tabs?sessionId=default"
	if got != want {
		t.Fatalf("browserAPIPath = %q, want %q", got, want)
	}
}
