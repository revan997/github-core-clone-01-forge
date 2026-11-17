package scenario

import "testing"

func TestUsersMatchKnownValues(t *testing.T) {
	if Viewer.Login != "adaiversen" {
		t.Fatalf("viewer login = %q, want adaiversen", Viewer.Login)
	}
	if Viewer.Name != "ada iversen" {
		t.Fatalf("viewer name = %q, want \"ada iversen\"", Viewer.Name)
	}
	if Viewer.ID != 1000 {
		t.Fatalf("viewer id = %d, want 1000", Viewer.ID)
	}
	if Viewer.Email != "adaiversen@example.invalid" {
		t.Fatalf("viewer email = %q, want adaiversen@example.invalid", Viewer.Email)
	}
	if Teammate.Login != "quinnkeller" {
		t.Fatalf("teammate login = %q, want quinnkeller", Teammate.Login)
	}
	if Teammate.Name != "quinn keller" {
		t.Fatalf("teammate name = %q, want \"quinn keller\"", Teammate.Name)
	}
	if Teammate.ID != 1001 {
		t.Fatalf("teammate id = %d, want 1001", Teammate.ID)
	}
	if Stranger.Login != "olgabauer" {
		t.Fatalf("stranger login = %q, want olgabauer", Stranger.Login)
	}
	if Stranger.Name != "olga bauer" {
		t.Fatalf("stranger name = %q, want \"olga bauer\"", Stranger.Name)
	}
	if Stranger.ID != 1002 {
		t.Fatalf("stranger id = %d, want 1002", Stranger.ID)
	}
	// All 12 generated users are distinct logins.
	seen := map[string]bool{}
	for _, u := range users {
		if seen[u.Login] {
			t.Fatalf("duplicate login %q", u.Login)
		}
		seen[u.Login] = true
	}
	if len(users) != 12 {
		t.Fatalf("len(users) = %d, want 12", len(users))
	}
}

func TestForgeIdentity(t *testing.T) {
	if ForgeIdentity != "ada iversen <adaiversen@example.invalid>" {
		t.Fatalf("ForgeIdentity = %q", ForgeIdentity)
	}
}

func TestVerifyDoesNotPanic(t *testing.T) {
	// With correct generated values Verify must stay silent and not panic.
	Verify()
}
