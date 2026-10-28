package login

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func TestPasswordHashAndMalformedPHC(t *testing.T) {
	const password = "a sufficiently long local password"
	encoded, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=1" {
		t.Fatal("wrong Argon2id policy")
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt length %d, error %v", len(salt), err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) != 32 {
		t.Fatalf("key length %d, error %v", len(key), err)
	}
	independent := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	if subtle.ConstantTimeCompare(key, independent) != 1 {
		t.Fatal("hash is not Argon2id with the declared policy")
	}
	if !VerifyPassword(encoded, password) || VerifyPassword(encoded, password+"wrong") {
		t.Fatal("password verification accepted mismatch or rejected the password")
	}
	again, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if again == encoded {
		t.Fatal("password salt reused")
	}
	for _, password := range []string{strings.Repeat("a", 12), strings.Repeat("x", 1024), strings.Repeat("界", 4)} {
		encoded, err := HashPassword(password)
		if err != nil || !VerifyPassword(encoded, password) {
			t.Fatalf("valid byte-boundary password rejected: length %d, err %v", len(password), err)
		}
	}
	for _, password := range []string{"", strings.Repeat("a", 11), strings.Repeat("a", 1025), "long\x00password text", string([]byte{0xff, 0xfe}) + strings.Repeat("a", 12)} {
		if _, err := HashPassword(password); err == nil {
			t.Fatalf("invalid password accepted: length %d", len(password))
		}
		if VerifyPassword(encoded, password) {
			t.Fatal("invalid password verified")
		}
	}
	malformed := []string{
		"bad", "$argon2id$v=19$m=65536,t=3,p=1$missing", encoded + "$extra",
		strings.Replace(encoded, "argon2id", "argon2i", 1), strings.Replace(encoded, "v=19", "v=16", 1),
		strings.Replace(encoded, "m=65536", "m=4294967295", 1), strings.Replace(encoded, "m=65536", "m=0", 1), strings.Replace(encoded, "m=65536", "m=32768", 1),
		strings.Replace(encoded, "t=3", "t=4294967295", 1), strings.Replace(encoded, "t=3", "t=0", 1),
		strings.Replace(encoded, "p=1", "p=256", 1), strings.Replace(encoded, "p=1", "p=0", 1),
		strings.Replace(encoded, "m=65536,t=3,p=1", "t=3,m=65536,p=1", 1),
		strings.Replace(encoded, "m=65536", "m=065536", 1),
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4] + "=$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4] + "$" + parts[5] + "=",
		"$argon2id$v=19$m=65536,t=3,p=1$" + strings.Repeat("a", 23) + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4] + "$" + strings.Repeat("a", 44),
		"$argon2id$v=19$m=65536,t=3,p=1$" + strings.Repeat("!", 22) + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4] + "$" + strings.Repeat("!", 43),
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4][:21] + "B$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=1$" + parts[4] + "$" + parts[5][:42] + "B",
		strings.Repeat("a", 1<<20),
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i, value := range malformed {
		if VerifyPassword(value, password) {
			t.Fatalf("malformed PHC case %d accepted", i)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("malformed PHC allocated %d bytes; bounds must be checked before Argon2", allocated)
	}
	// Unknown accounts use the same bounded Argon2 path, never an empty-hash acceptance.
	runtime.ReadMemStats(&before)
	if VerifyPassword("", password) {
		t.Fatal("unknown login's dummy hash authenticated")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated < 64<<20 || allocated > 66<<20 {
		t.Fatalf("unknown login did not use one bounded 64 MiB Argon2 derivation: %d bytes", allocated)
	}
}

func TestPasswordHashWorkIsProcessBounded(t *testing.T) {
	// Occupy every permitted slot, then exercise both real derivation callers.
	// No test-only hook is needed in the cryptographic operation itself.
	held := 0
	var workers sync.WaitGroup
	defer func() {
		for held > 0 {
			<-argonSlots
			held--
		}
		workers.Wait()
	}()
	for i := 0; i < 8; i++ {
		select {
		case argonSlots <- struct{}{}:
			held++
		default:
			t.Fatal("password gate permits fewer than eight operations")
		}
	}
	select {
	case argonSlots <- struct{}{}:
		held++
		t.Fatal("password gate permits more than eight operations")
	default:
	}
	ready := make(chan struct{}, 2)
	done := make(chan error, 2)
	workers.Add(2)
	go func() {
		defer workers.Done()
		ready <- struct{}{}
		_, err := HashPassword("bounded local password")
		done <- err
	}()
	go func() {
		defer workers.Done()
		ready <- struct{}{}
		if VerifyPassword("", "bounded local password") {
			done <- fmt.Errorf("dummy password authenticated")
		} else {
			done <- nil
		}
	}()
	<-ready
	<-ready
	select {
	case <-done:
		t.Fatal("Argon2 work bypassed the shared full process gate")
	case <-time.After(500 * time.Millisecond):
	}
	for held > 0 {
		<-argonSlots
		held--
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Argon2 work did not resume after releasing its slots")
		}
	}
}
