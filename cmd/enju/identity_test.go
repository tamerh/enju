package main

import "testing"

func TestHasFullIdentity(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{"Alice", "alice@example.com", true},
		{"Alice", "", false},  // name-only: email is required
		{"", "alice@example.com", false},  // email-only: name is required
		{"", "", false},
	}
	for _, tt := range tests {
		got := hasFullIdentity(tt.name, tt.email)
		if got != tt.want {
			t.Errorf("hasFullIdentity(%q, %q) = %v, want %v", tt.name, tt.email, got, tt.want)
		}
	}
}

func TestResolveIdentityDoesNotOverwrite(t *testing.T) {
	name := "Explicit Name"
	email := "explicit@example.com"
	username := "explicit"
	resolveIdentity(&name, &email, &username)
	if name != "Explicit Name" {
		t.Errorf("resolveIdentity overwrote non-empty name: got %q", name)
	}
	if email != "explicit@example.com" {
		t.Errorf("resolveIdentity overwrote non-empty email: got %q", email)
	}
}
