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

func TestStaticTokenAuthServiceValidateAuthorizationHeaderAcceptsReviewToken(t *testing.T) {
	svc := NewStaticTokenAuthService("production-secret", "review-secret")
	if !svc.ValidateAuthorizationHeader("Bearer production-secret") {
		t.Fatal("expected production bearer header to be accepted")
	}
	if !svc.ValidateAuthorizationHeader("Bearer review-secret") {
		t.Fatal("expected review bearer header to be accepted")
	}
	if svc.ValidateAuthorizationHeader("Bearer disabled-review-secret") {
		t.Fatal("expected unconfigured review bearer header to be rejected")
	}
}
