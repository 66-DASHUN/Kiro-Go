package auth

import "testing"

// The API handler starts this flow with an ownership selection, then may omit
// the field on a later callback after a modal re-render. The auth session must
// retain the original selection until an explicit empty value clears it.
func TestMicrosoftSSOApiKeySnapshotDistinguishesOmissionFromExplicitClear(t *testing.T) {
	resetMicrosoftSSOSessionsForTest(t)

	sessionID, _, _, err := StartMicrosoftSSOLogin("key-a")
	if err != nil {
		t.Fatalf("StartMicrosoftSSOLogin() error = %v", err)
	}
	session := mustMicrosoftSession(t, sessionID)
	if session.ApiKeyID != "key-a" || !session.ApiKeyIDSet {
		t.Fatalf("initial api-key snapshot = (%q, %v), want (key-a, true)", session.ApiKeyID, session.ApiKeyIDSet)
	}

	// A malformed callback is enough to exercise the request-argument handling;
	// the OAuth session remains available for a retry.
	if _, err := ContinueMicrosoftSSOLogin(sessionID, "not-a-callback"); err == nil {
		t.Fatal("ContinueMicrosoftSSOLogin() unexpectedly accepted malformed callback")
	}
	session = mustMicrosoftSession(t, sessionID)
	if session.ApiKeyID != "key-a" || !session.ApiKeyIDSet {
		t.Fatalf("omitted api-key argument changed snapshot to (%q, %v)", session.ApiKeyID, session.ApiKeyIDSet)
	}

	if _, err := ContinueMicrosoftSSOLogin(sessionID, "not-a-callback", ""); err == nil {
		t.Fatal("ContinueMicrosoftSSOLogin() unexpectedly accepted malformed callback with explicit clear")
	}
	session = mustMicrosoftSession(t, sessionID)
	if session.ApiKeyID != "" || !session.ApiKeyIDSet {
		t.Fatalf("explicit empty api-key argument did not clear snapshot: (%q, %v)", session.ApiKeyID, session.ApiKeyIDSet)
	}
}
