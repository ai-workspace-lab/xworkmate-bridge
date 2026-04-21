package service

import "testing"

func TestStaticTokenAuthServiceValidateToken(t *testing.T) {
	svc := NewStaticTokenAuthService("secret")
	if !svc.ValidateToken("secret") {
		t.Fatal("expected valid token")
	}
	if svc.ValidateToken("wrong") {
		t.Fatal("expected invalid token")
	}
}

func TestStaticTokenAuthServiceValidateAuthorizationHeaderPermissive(t *testing.T) {
	svc := NewStaticTokenAuthService("")
	if !svc.ValidateAuthorizationHeader("Bearer test-token") {
		t.Fatal("expected bearer header to be accepted")
	}
	if !svc.ValidateAuthorizationHeader("Basic abc") {
		t.Fatal("expected any header to be accepted when no token is set")
	}
}

func TestStaticTokenAuthServiceValidateAuthorizationHeaderStrictWhenSet(t *testing.T) {
	svc := NewStaticTokenAuthService("secret")
	if !svc.ValidateAuthorizationHeader("Bearer secret") {
		t.Fatal("expected bearer header to be accepted")
	}
	if svc.ValidateAuthorizationHeader("Bearer wrong") {
		t.Fatal("expected wrong bearer token to be rejected")
	}
	if svc.ValidateAuthorizationHeader("Basic abc") {
		t.Fatal("expected non-bearer header to be rejected")
	}
}
