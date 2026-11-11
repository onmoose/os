package store

import (
	"reflect"
	"testing"
	"time"
)

func TestMadeFolders_AddListIdempotentAndCascade(t *testing.T) {
	s := open(t)
	for _, u := range []User{
		{ID: "u_a", Username: "alex", DisplayName: "Alex", Role: RoleAdmin, CreatedAt: time.Now()},
		{ID: "u_b", Username: "bea", DisplayName: "Bea", Role: RoleMember, CreatedAt: time.Now()},
	} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	got, err := s.MadeFolders("u_a")
	if err != nil || len(got) != 0 {
		t.Fatalf("no rows yet: got %v, %v", got, err)
	}
	for _, n := range []string{"Recipes", "Garden", "Recipes"} {
		if err := s.AddMadeFolder("u_a", n); err != nil {
			t.Fatalf("add %q: %v", n, err)
		}
	}
	if err := s.AddMadeFolder("u_b", "Taxes"); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err = s.MadeFolders("u_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"Garden", "Recipes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("u_a rows = %v, want %v (sorted, once each, only u_a's)", got, want)
	}
	if err := s.DeleteUser("u_a"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, _ := s.MadeFolders("u_a"); len(got) != 0 {
		t.Errorf("rows outlived their user: %v", got)
	}
	if got, _ := s.MadeFolders("u_b"); !reflect.DeepEqual(got, []string{"Taxes"}) {
		t.Errorf("another user's rows changed: %v", got)
	}
}
