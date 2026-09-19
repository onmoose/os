package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
	"golang.org/x/crypto/bcrypt"
)

// startFakeAuthAgent spins up a minimal stand-in for the fake host-agent's
// auth surface on a UNIX socket in t.TempDir(). We mirror the fake's handler
// logic (bcrypt-backed in-memory hashes) rather than importing cmd/host-agent,
// which is a main package. The point of this test is the wire seam:
// hostclient ↔ socket ↔ /v1/auth/{verify,set,delete}-{password,user}.
func startFakeAuthAgent(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	passwords := map[string][]byte{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
		mu.Lock()
		passwords[req.User] = hash
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	mux.HandleFunc("POST /v1/auth/verify-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.VerifyPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		hash, ok := passwords[req.User]
		mu.Unlock()
		valid := ok && bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) == nil
		_ = json.NewEncoder(w).Encode(protocol.VerifyPasswordResponse{Valid: valid})
	})
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		delete(passwords, req.User)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})

	roles := map[string]string{}
	mux.HandleFunc("POST /v1/auth/set-role", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetRoleRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Role != "admin" && req.Role != "member" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "bad-request", Message: "role must be admin or member"})
			return
		}
		mu.Lock()
		roles[req.User] = req.Role
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	_ = roles

	mux.HandleFunc("GET /v1/users/{username}/home", func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		if username == "ghost" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "unknown-user", Message: "user not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.ResolveHomeResponse{
			HomePath: "/home/" + username,
			UID:      3001,
			GID:      3001,
		})
	})

	mux.HandleFunc("GET /v1/identity/well-known", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.WellKnownIdentityResponse{
			MooseAppUID:    2000,
			MooseAppGID:    2000,
			MooseSharedGID: 2001,
		})
	})

	mux.HandleFunc("POST /v1/identity/app-service", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.AllocateAppServiceIdentityRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.InstanceID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "bad-request", Message: "instance_id is required"})
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.AllocateAppServiceIdentityResponse{UID: 2100, GID: 2100})
	})
	mux.HandleFunc("POST /v1/identity/app-service/release", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ReleaseAppServiceIdentityRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.UID < protocol.AppServiceUIDMin || req.UID > protocol.AppServiceUIDMax {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "bad-request", Message: "uid outside the app-service band"})
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /v1/system/resources", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.SystemResources{
			TsNs:    84021000000000,
			CPU:     protocol.CPUCounters{TotalJiffies: 12044910, IdleJiffies: 9881233},
			LoadAvg: [3]float64{0.42, 0.51, 0.48},
			Mem:     protocol.MemCounters{TotalBytes: 16728338432, AvailableBytes: 9214455808, UsedBytes: 7513882624},
			Net:     []protocol.NetCounters{{Iface: "enp3s0", RxBytes: 99201234, TxBytes: 41200934}},
			Disk:    []protocol.DiskCounters{{Dev: "sda", ReadBytes: 81002496, WriteBytes: 12300288}},
			UptimeS: 84021,
		})
	})

	mux.HandleFunc("GET /v1/system/gpu", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104})
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

func TestSystemResources(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	resp, err := c.SystemResources(context.Background())
	if err != nil {
		t.Fatalf("SystemResources: %v", err)
	}
	if resp.TsNs != 84021000000000 {
		t.Errorf("ts_ns: want 84021000000000, got %d", resp.TsNs)
	}
	if resp.CPU.TotalJiffies != 12044910 || resp.CPU.IdleJiffies != 9881233 {
		t.Errorf("cpu: want {12044910, 9881233}, got %+v", resp.CPU)
	}
	if resp.Mem.AvailableBytes != 9214455808 {
		t.Errorf("mem available_bytes: want 9214455808, got %d", resp.Mem.AvailableBytes)
	}
	if len(resp.Net) != 1 || resp.Net[0].Iface != "enp3s0" || resp.Net[0].RxBytes != 99201234 {
		t.Errorf("net: want one enp3s0 entry, got %+v", resp.Net)
	}
	if len(resp.Disk) != 1 || resp.Disk[0].Dev != "sda" {
		t.Errorf("disk: want one sda entry, got %+v", resp.Disk)
	}
	if resp.LoadAvg != [3]float64{0.42, 0.51, 0.48} {
		t.Errorf("loadavg: want [0.42 0.51 0.48], got %v", resp.LoadAvg)
	}
}

func TestSystemGPU(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	resp, err := c.SystemGPU(context.Background())
	if err != nil {
		t.Fatalf("SystemGPU: %v", err)
	}
	if !resp.Present || resp.Vendor != "intel" || resp.RenderGID != 104 {
		t.Errorf("want {present:true intel 104}, got %+v", resp)
	}
}

func TestResolveHome(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	ctx := context.Background()

	resp, err := c.ResolveHome(ctx, "alice")
	if err != nil {
		t.Fatalf("ResolveHome: %v", err)
	}
	if resp.HomePath != "/home/alice" {
		t.Errorf("home_path: want /home/alice, got %q", resp.HomePath)
	}
	if resp.UID != 3001 || resp.GID != 3001 {
		t.Errorf("uid/gid: want 3001/3001, got %d/%d", resp.UID, resp.GID)
	}

	// Unknown user: 404 must surface as ErrUnknownUser so the brain can
	// errors.Is-discriminate it from a generic host failure (abort vs. retry).
	_, err = c.ResolveHome(ctx, "ghost")
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("ResolveHome(unknown user) = %v; want ErrUnknownUser", err)
	}
}

func TestWellKnownIdentity(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	ctx := context.Background()

	resp, err := c.WellKnownIdentity(ctx)
	if err != nil {
		t.Fatalf("WellKnownIdentity: %v", err)
	}
	if resp.MooseAppUID != 2000 {
		t.Errorf("moose_app_uid: want 2000, got %d", resp.MooseAppUID)
	}
	if resp.MooseAppGID != 2000 {
		t.Errorf("moose_app_gid: want 2000, got %d", resp.MooseAppGID)
	}
	if resp.MooseSharedGID != 2001 {
		t.Errorf("moose_shared_gid: want 2001, got %d", resp.MooseSharedGID)
	}
}

func TestAppServiceIdentity(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	ctx := context.Background()

	resp, err := c.AllocateAppServiceIdentity(ctx, "inst_a")
	if err != nil {
		t.Fatalf("AllocateAppServiceIdentity: %v", err)
	}
	if resp.UID != 2100 || resp.GID != 2100 {
		t.Errorf("uid/gid: want 2100/2100, got %d/%d", resp.UID, resp.GID)
	}

	if err := c.ReleaseAppServiceIdentity(ctx, 2100); err != nil {
		t.Fatalf("ReleaseAppServiceIdentity: %v", err)
	}
	// Out-of-band UID: the agent's 400 must surface as an error.
	if err := c.ReleaseAppServiceIdentity(ctx, 1000); err == nil {
		t.Fatal("ReleaseAppServiceIdentity(out-of-band) = nil; want error")
	}
}

func TestSetRole(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	ctx := context.Background()

	if err := c.SetRole(ctx, "alice", "admin"); err != nil {
		t.Fatalf("SetRole admin: %v", err)
	}
	if err := c.SetRole(ctx, "alice", "member"); err != nil {
		t.Fatalf("SetRole member: %v", err)
	}
	if err := c.SetRole(ctx, "alice", "superuser"); err == nil {
		t.Fatal("SetRole(bogus role) = nil; want error")
	}
}

func TestAuthEndpoints(t *testing.T) {
	c := New(startFakeAuthAgent(t))
	ctx := context.Background()

	// Set, then verify the right and wrong passwords.
	if err := c.SetPassword(ctx, "cindy", "hunter2"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if ok, err := c.VerifyPassword(ctx, "cindy", "hunter2"); err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := c.VerifyPassword(ctx, "cindy", "wrong"); err != nil || ok {
		t.Fatalf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}

	// Unknown user is false, not an error (mirrors PAM's posture).
	if ok, err := c.VerifyPassword(ctx, "ghost", "anything"); err != nil || ok {
		t.Fatalf("VerifyPassword(unknown) = %v, %v; want false, nil", ok, err)
	}

	// Upsert: changing the password makes the old one invalid.
	if err := c.SetPassword(ctx, "cindy", "newpass"); err != nil {
		t.Fatalf("SetPassword (upsert): %v", err)
	}
	if ok, _ := c.VerifyPassword(ctx, "cindy", "hunter2"); ok {
		t.Fatal("old password still valid after upsert")
	}
	if ok, _ := c.VerifyPassword(ctx, "cindy", "newpass"); !ok {
		t.Fatal("new password not valid after upsert")
	}

	// Delete is idempotent and clears credentials.
	if err := c.DeleteUser(ctx, "cindy"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := c.DeleteUser(ctx, "cindy"); err != nil {
		t.Fatalf("DeleteUser (second call should be idempotent): %v", err)
	}
	if ok, _ := c.VerifyPassword(ctx, "cindy", "newpass"); ok {
		t.Fatal("password still valid after delete")
	}
}
