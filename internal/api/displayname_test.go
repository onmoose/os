package api

import (
	"net/http"
	"testing"
)

// createUser posts a new account and returns the response, so a test can assert
// on the status as well as the body.
func (h *harness) createUser(displayName, password, role string) *http.Response {
	h.t.Helper()
	return h.do("POST", "/api/v1/users", map[string]string{
		"display_name": displayName, "password": password, "role": role,
	})
}

// TestSetupDerivesAccountNameFromDisplayName is the issue's worked example: an
// admin types "José Smith" and the box makes a Linux user "josesmith".
func TestSetupDerivesAccountNameFromDisplayName(t *testing.T) {
	h := newHarness(t)
	u := h.setupAdmin("José Smith", "hunter2")
	if u.DisplayName != "José Smith" {
		t.Errorf("display_name = %q; want %q", u.DisplayName, "José Smith")
	}
	if u.Username != "josesmith" {
		t.Errorf("username = %q; want josesmith", u.Username)
	}
	// The account the box made is the one that signs in.
	h.loginAs("josesmith", "hunter2")
}

func TestCreateUserDerivesAccountNameAndRejectsACallerSuppliedOne(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")
	h.elevate("hunter2")

	resp := h.createUser("José Smith", "bobpass12", "member")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d; want 200", resp.StatusCode)
	}
	u := decodeJSON[UserDTO](t, resp)
	if u.Username != "josesmith" || u.DisplayName != "José Smith" {
		t.Errorf("created %+v; want username josesmith, display_name José Smith", u)
	}

	// "username" is not a field on this route any more. The schema is strict, so
	// an old caller that still sends one is told plainly rather than having it
	// quietly ignored and getting an account name it did not ask for.
	r2 := h.do("POST", "/api/v1/users", map[string]string{
		"display_name": "Dana", "username": "hackerman", "password": "danapass12",
	})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("create with a caller-supplied username = %d; want 422", r2.StatusCode)
	}
}

// A second José is allowed, because the display names differ, and gets the
// suffixed account name.
func TestSecondSimilarNameGetsASuffixedAccount(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")
	h.elevate("hunter2")

	first := h.createUser("José Smith", "pass123456", "member")
	defer first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first = %d", first.StatusCode)
	}
	second := h.createUser("Jose Smith", "pass123456", "member")
	defer second.Body.Close()
	if second.StatusCode != 200 {
		t.Fatalf("second = %d; want 200", second.StatusCode)
	}
	if u := decodeJSON[UserDTO](t, second); u.Username != "josesmith1" {
		t.Errorf("second account = %q; want josesmith1", u.Username)
	}
}

// Two people with the same name is the confusing case the spec rejects.
func TestDuplicateDisplayNameIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")
	h.elevate("hunter2")

	first := h.createUser("Cindy", "pass123456", "member")
	first.Body.Close()
	for _, dupe := range []string{"Cindy", "cindy", "  CINDY  "} {
		resp := h.createUser(dupe, "pass123456", "member")
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("create %q = %d; want 409", dupe, resp.StatusCode)
		}
	}
}

// Somebody called "root" does not get the root account.
func TestReservedNameDoesNotGetTheSystemAccount(t *testing.T) {
	h := newHarness(t)
	u := h.setupAdmin("root", "hunter2")
	if u.Username == "root" {
		t.Fatal("derived account name is root")
	}
	if u.Username != "root1" {
		t.Errorf("username = %q; want root1", u.Username)
	}
	if u.DisplayName != "root" {
		t.Errorf("display_name = %q; want root", u.DisplayName)
	}
}

// Renaming changes what the box shows and nothing else.
func TestRenameLeavesTheAccountNameAlone(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("José Smith", "hunter2")
	h.loginAs(admin.Username, "hunter2")

	resp := h.do("POST", "/api/v1/users/"+admin.ID+"/name",
		map[string]string{"display_name": "Jo"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("rename = %d; want 200", resp.StatusCode)
	}
	got := decodeJSON[UserDTO](t, resp)
	if got.DisplayName != "Jo" {
		t.Errorf("display_name = %q; want Jo", got.DisplayName)
	}
	if got.Username != "josesmith" {
		t.Errorf("username = %q; want josesmith unchanged", got.Username)
	}

	// The old password still works against the unchanged account, which is the
	// observable proof the Linux side was not touched.
	h.loginAs("josesmith", "hunter2")
	meResp := h.do("GET", "/api/v1/me", nil)
	defer meResp.Body.Close()
	if me := decodeJSON[UserDTO](t, meResp); me.DisplayName != "Jo" || me.Username != "josesmith" {
		t.Errorf("after rename /me = %+v; want Jo / josesmith", me)
	}
}

func TestRenameRefusesADuplicateName(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")
	h.elevate("hunter2")
	h.createUser("Cindy", "pass123456", "member").Body.Close()

	resp := h.do("POST", "/api/v1/users/"+admin.ID+"/name",
		map[string]string{"display_name": "cindy"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("rename to a taken name = %d; want 409", resp.StatusCode)
	}
}

// Renaming yourself to the name you already have is not a conflict with
// yourself.
func TestRenameToOwnNameSucceeds(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")

	resp := h.do("POST", "/api/v1/users/"+admin.ID+"/name",
		map[string]string{"display_name": "Alice"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("rename to own name = %d; want 200", resp.StatusCode)
	}
}

func TestRenameSomebodyElseNeedsAdminAndElevation(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("Alice", "hunter2")
	h.loginAs(admin.Username, "hunter2")
	h.elevate("hunter2")
	bobResp := h.createUser("Bob", "bobpass1234", "member")
	bob := decodeJSON[UserDTO](t, bobResp)
	bobResp.Body.Close()

	// A member may not rename anyone else.
	h.loginAs(bob.Username, "bobpass1234")
	resp := h.do("POST", "/api/v1/users/"+admin.ID+"/name",
		map[string]string{"display_name": "Mallory"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("member renaming an admin = %d; want 403", resp.StatusCode)
	}

	// A member may rename themselves, with no elevation.
	self := h.do("POST", "/api/v1/users/"+bob.ID+"/name",
		map[string]string{"display_name": "Bobby"})
	self.Body.Close()
	if self.StatusCode != 200 {
		t.Errorf("member renaming self = %d; want 200", self.StatusCode)
	}

	// An admin renaming somebody else needs the elevation window.
	h.loginAs(admin.Username, "hunter2")
	unelevated := h.do("POST", "/api/v1/users/"+bob.ID+"/name",
		map[string]string{"display_name": "Robert"})
	unelevated.Body.Close()
	if unelevated.StatusCode != http.StatusForbidden {
		t.Errorf("unelevated admin rename = %d; want 403", unelevated.StatusCode)
	}
	h.elevate("hunter2")
	elevated := h.do("POST", "/api/v1/users/"+bob.ID+"/name",
		map[string]string{"display_name": "Robert"})
	elevated.Body.Close()
	if elevated.StatusCode != 200 {
		t.Errorf("elevated admin rename = %d; want 200", elevated.StatusCode)
	}
}

// The login picker carries the display name to render and the account name to
// post back, and stays shut on hosted.
func TestLoginPickerCarriesBothNames(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("José Smith", "hunter2")

	resp := h.do("GET", "/api/v1/auth/users", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("auth/users = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Users []loginPickerUser `json:"users"`
	}](t, resp)
	if len(body.Users) != 1 {
		t.Fatalf("picker has %d users; want 1", len(body.Users))
	}
	if body.Users[0].DisplayName != "José Smith" || body.Users[0].Username != "josesmith" {
		t.Errorf("picker entry = %+v; want José Smith / josesmith", body.Users[0])
	}
}
