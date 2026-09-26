package store

import (
	"errors"
	"testing"
	"time"
)

func sampleAIAccount(id, label string) AIAccount {
	return AIAccount{
		ID: id, OwnerUserID: mailOwner, ProviderID: "anthropic", Label: label,
		APIKey: "sk-ant-secret", BaseURL: "",
		CreatedAt: time.Unix(1_700_000_000, 0), UpdatedAt: time.Unix(1_700_000_000, 0),
	}
}

func TestAIAccountCRUD(t *testing.T) {
	s := openWithOwner(t)
	a := sampleAIAccount("ai_1", "Work")
	if err := s.CreateAIAccount(a); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetAIAccount("ai_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != a {
		t.Fatalf("roundtrip: got %+v, want %+v", got, a)
	}

	// Duplicate id and duplicate label both conflict.
	if err := s.CreateAIAccount(sampleAIAccount("ai_1", "Other")); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup id: got %v, want ErrConflict", err)
	}
	if err := s.CreateAIAccount(sampleAIAccount("ai_2", "Work")); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup label: got %v, want ErrConflict", err)
	}

	// List is ordered by label.
	home := sampleAIAccount("ai_2", "Home")
	home.ProviderID, home.APIKey, home.BaseURL = "openai_compatible", "", "http://192.168.1.10:11434/v1"
	if err := s.CreateAIAccount(home); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := s.ListAIAccounts(mailOwner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0] != home || list[1].Label != "Work" {
		t.Fatalf("list order: got %+v", list)
	}

	// Update changes the mutable fields and keeps created_at.
	a.Label, a.APIKey, a.BaseURL = "Work 2", "sk-ant-new", "https://proxy.example.com/v1"
	a.UpdatedAt = time.Unix(1_700_000_500, 0)
	a.CreatedAt = time.Unix(1, 0) // ignored by Update
	if err := s.UpdateAIAccount(a); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetAIAccount("ai_1")
	if got.Label != "Work 2" || got.APIKey != "sk-ant-new" || got.BaseURL != "https://proxy.example.com/v1" ||
		got.UpdatedAt.Unix() != 1_700_000_500 || got.CreatedAt.Unix() != 1_700_000_000 {
		t.Fatalf("update roundtrip: got %+v", got)
	}
	// An empty key keeps the stored one, so an edit that sends no key never
	// writes back a key it read earlier.
	a.Label, a.APIKey = "Work 3", ""
	if err := s.UpdateAIAccount(a); err != nil {
		t.Fatalf("update without key: %v", err)
	}
	got, _ = s.GetAIAccount("ai_1")
	if got.Label != "Work 3" || got.APIKey != "sk-ant-new" {
		t.Fatalf("update without key: got %+v, want the stored key kept", got)
	}
	a.Label = "Home"
	if err := s.UpdateAIAccount(a); !errors.Is(err, ErrConflict) {
		t.Fatalf("update to taken label: got %v, want ErrConflict", err)
	}
	missing := sampleAIAccount("ai_missing", "Nope")
	if err := s.UpdateAIAccount(missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: got %v, want ErrNotFound", err)
	}

	if err := s.DeleteAIAccount("ai_1", mailOwner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetAIAccount("ai_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteAIAccount("ai_1", mailOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: got %v, want ErrNotFound", err)
	}
}

func TestAIAccountOwnerScoping(t *testing.T) {
	s := openWithOwner(t)
	addUser(t, s, "u_other", RoleMember, 1_600_000_100)

	if err := s.CreateAIAccount(sampleAIAccount("ai_1", "Claude")); err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs := sampleAIAccount("ai_2", "Claude")
	theirs.OwnerUserID = "u_other"
	if err := s.CreateAIAccount(theirs); err != nil {
		t.Fatalf("same label, other owner: %v", err)
	}

	mine, _ := s.ListAIAccounts(mailOwner)
	other, _ := s.ListAIAccounts("u_other")
	if len(mine) != 1 || mine[0].ID != "ai_1" || len(other) != 1 || other[0].ID != "ai_2" {
		t.Fatalf("lists not scoped to owner: mine=%+v other=%+v", mine, other)
	}
	if all, _ := s.ListAllAIAccounts(); len(all) != 2 {
		t.Fatalf("list all = %+v; want 2", all)
	}

	// An update or delete that names the wrong owner finds nothing.
	stolen := theirs
	stolen.OwnerUserID = mailOwner
	stolen.APIKey = "stolen"
	if err := s.UpdateAIAccount(stolen); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update other owner's row: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteAIAccount("ai_2", mailOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete other owner's row: got %v, want ErrNotFound", err)
	}
	if got, _ := s.GetAIAccount("ai_2"); got.APIKey != "sk-ant-secret" {
		t.Fatalf("other owner's row changed: %+v", got)
	}

	// An account needs an owner and a provider.
	orphan := sampleAIAccount("ai_3", "Orphan")
	orphan.OwnerUserID = ""
	if err := s.CreateAIAccount(orphan); err == nil {
		t.Fatal("create without an owner: want error")
	}
	noProvider := sampleAIAccount("ai_4", "None")
	noProvider.ProviderID = ""
	if err := s.CreateAIAccount(noProvider); err == nil {
		t.Fatal("create without a provider: want error")
	}
}

// Deleting a user deletes their AI accounts and nobody else's.
func TestDeleteUserCascadesAIAccounts(t *testing.T) {
	s := openWithOwner(t)
	addUser(t, s, "u_other", RoleMember, 1_600_000_100)
	theirs := sampleAIAccount("ai_2", "Theirs")
	theirs.OwnerUserID = "u_other"
	if err := s.CreateAIAccount(theirs); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateAIAccount(sampleAIAccount("ai_1", "Mine")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteUser("u_other"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.GetAIAccount("ai_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user's account survived: %v", err)
	}
	if _, err := s.GetAIAccount("ai_1"); err != nil {
		t.Fatalf("another user's account went with it: %v", err)
	}
}

// Bindings round-trip their models, replace per instance, list by account, and
// cascade with both the instance and the account.
func TestInstanceAIBindings(t *testing.T) {
	s := openWithOwner(t)
	for _, id := range []string{"a", "b"} {
		if err := s.Create(sample(id, "app-"+id)); err != nil {
			t.Fatalf("create instance %s: %v", id, err)
		}
	}
	for _, a := range []AIAccount{sampleAIAccount("ai_1", "Work"), sampleAIAccount("ai_2", "Home")} {
		if err := s.CreateAIAccount(a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	bs := []AIBinding{
		{Slot: "ai.openai_compatible", AccountID: "ai_2", Models: map[string][]string{"models.chat": {"m1", "m2"}}},
		{Slot: "ai.anthropic", AccountID: "ai_1"},
	}
	if err := s.SetInstanceAIBindings("a", bs); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetInstanceAIBindings("b", []AIBinding{{Slot: "ai.anthropic", AccountID: "ai_1"}}); err != nil {
		t.Fatalf("set b: %v", err)
	}
	got, err := s.ListInstanceAIBindings("a")
	if err != nil || len(got) != 2 {
		t.Fatalf("list a = %+v (%v)", got, err)
	}
	if got[0].Slot != "ai.anthropic" || got[0].InstanceID != "a" || len(got[0].Models) != 0 {
		t.Fatalf("first binding = %+v", got[0])
	}
	if m := got[1].Models["models.chat"]; got[1].AccountID != "ai_2" || len(m) != 2 || m[0] != "m1" || m[1] != "m2" {
		t.Fatalf("second binding = %+v", got[1])
	}

	byAcct, err := s.ListAIBindingsForAccount("ai_1")
	if err != nil || len(byAcct) != 2 || byAcct[0].InstanceID != "a" || byAcct[1].InstanceID != "b" {
		t.Fatalf("by account = %+v (%v)", byAcct, err)
	}

	// Set replaces the instance's whole set.
	if err := s.SetInstanceAIBindings("a", []AIBinding{{Slot: "ai.anthropic", AccountID: "ai_2"}}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := s.ListInstanceAIBindings("a"); len(got) != 1 || got[0].AccountID != "ai_2" {
		t.Fatalf("after replace = %+v", got)
	}
	// A missing account fails the foreign key.
	if err := s.SetInstanceAIBindings("a", []AIBinding{{Slot: "ai.x", AccountID: "ai_ghost"}}); err == nil {
		t.Fatal("binding to a missing account must fail")
	}

	// Deleting the account removes its bindings; the instance survives.
	if err := s.DeleteAIAccount("ai_1", mailOwner); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if got, _ := s.ListInstanceAIBindings("b"); len(got) != 0 {
		t.Fatalf("binding must cascade with the account: %+v", got)
	}
	if _, err := s.Get("b"); err != nil {
		t.Fatalf("instance must survive account delete: %v", err)
	}

	// PutAIBinding adds one row, and deleting the instance cascades it away.
	if err := s.PutAIBinding(AIBinding{InstanceID: "a", Slot: "ai.openai", AccountID: "ai_2"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, _ := s.ListInstanceAIBindings("a"); len(got) != 2 {
		t.Fatalf("after put = %+v", got)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	if got, _ := s.ListAIBindingsForAccount("ai_2"); len(got) != 0 {
		t.Fatalf("binding must cascade with the instance: %+v", got)
	}
}
