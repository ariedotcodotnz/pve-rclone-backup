// SPDX-License-Identifier: AGPL-3.0-or-later

package recoverykit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

func init() { scryptN = 1 << 10 } // keep tests fast; production uses 2^17

func testKit(t *testing.T) *Kit {
	t.Helper()
	keys, err := secrets.NewRepoKeys("6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", "base32768", time.Unix(1790000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	k := New("pve1", time.Unix(1790000000, 0))
	k.Repos = []Repo{{
		StorageID: "offsite", RepoUUID: keys.RepoUUID, Remote: "onedrive-main", Path: "pve-backups",
		Source: "homelab", Encryption: "crypt", Keys: keys,
		RemoteConfig: map[string]string{"type": "onedrive", "token": `{"refresh_token":"very-secret-refresh"}`},
	}}
	return k
}

func headerOf(text string) string { return text[:strings.Index(text, beginArmor)] }

func TestRoundTrip(t *testing.T) {
	k := testKit(t)
	secret := k.Repos[0].Keys.Generations[0].Password
	for _, pass := range []string{"", "correct horse battery staple"} {
		text, sum, err := Encode(k, pass)
		if err != nil {
			t.Fatal(err)
		}
		h := headerOf(text)
		for _, s := range []string{secret, k.Repos[0].Keys.Generations[0].Password2, "very-secret-refresh"} {
			if strings.Contains(h, s) {
				t.Fatalf("header leaks a secret (passphrase %q)", pass)
			}
		}
		if !strings.Contains(h, "Checksum: "+sum) || !strings.Contains(h, "filename_encoding=base32768") ||
			!strings.Contains(h, "onedrive-main") || !strings.Contains(h, "OAuth token") {
			t.Fatalf("header incomplete:\n%s", h)
		}
		if (pass == "") != strings.Contains(h, "Encrypted: NO") {
			t.Fatalf("encryption state not stated correctly:\n%s", h)
		}
		if got, _ := TextChecksum(text); got != sum {
			t.Fatalf("checksum %s != %s", got, sum)
		}
		got, err := Decode(text, pass)
		if err != nil {
			t.Fatal(err)
		}
		if got.Repos[0].Keys.Generations[0].Password != secret || got.Repos[0].RemoteConfig["token"] == "" {
			t.Fatalf("decoded kit = %+v", got.Repos[0])
		}
		// Surrounding text, e.g. from an email or a printout scan, is ignored.
		if _, err := Decode("Fwd: my kit\n\n"+text+"\n-- \nsignature", pass); err != nil {
			t.Fatalf("surrounding text: %v", err)
		}
	}
}

func TestEncryptedKitNeedsCorrectPassphrase(t *testing.T) {
	text, _, err := Encode(testKit(t), "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "very-secret-refresh") {
		t.Fatal("token visible in encrypted kit")
	}
	if _, err := Decode(text, ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("no passphrase: %v", err)
	}
	if _, err := Decode(text, "wrong"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestDamageIsDetected(t *testing.T) {
	text, _, err := Encode(testKit(t), "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l == beginArmor {
			b := []byte(lines[i+2])
			b[10] ^= 1 // flip one character in the payload
			lines[i+2] = string(b)
			break
		}
	}
	if _, err := Decode(strings.Join(lines, "\n"), ""); !errors.Is(err, ErrDamaged) {
		t.Fatalf("altered payload: %v", err)
	}
	truncated := text[:strings.Index(text, endArmor)-30] + "\n" + endArmor + "\n"
	if _, err := Decode(truncated, ""); !errors.Is(err, ErrDamaged) {
		t.Fatalf("truncated payload: %v", err)
	}
	if _, err := Decode("no kit here", ""); !errors.Is(err, ErrDamaged) {
		t.Fatalf("missing armor: %v", err)
	}
}
