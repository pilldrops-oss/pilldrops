package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

func TestSetIdentityWritesFilesAndRefusesBadFields(t *testing.T) {
	good := sbtoken.IdentityPrefix + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, line, wantReply string
	}{
		{"valid", credSetIdentity + " " + good + " https://api.example.test sb-1-abc sb.example.test\n", "OK"},
		{"valid, no domain", credSetIdentity + " " + good + " http://10.128.0.1:18103 sb-1-abc -\n", "OK"},
		{"envd token, not an identity", credSetIdentity + " " + strings.Repeat("ab", 32) + " https://a b c\n", "ERR"},
		{"short token", credSetIdentity + " " + sbtoken.IdentityPrefix + "abcd https://api.example.test sb-1 -\n", "ERR"},
		{"url without scheme", credSetIdentity + " " + good + " api.example.test sb-1 -\n", "ERR"},
		{"url with userinfo", credSetIdentity + " " + good + " https://x:y@api.example.test sb-1 -\n", "ERR"},
		{"id with a slash", credSetIdentity + " " + good + " https://api.example.test ../etc -\n", "ERR"},
		{"id upper case", credSetIdentity + " " + good + " https://api.example.test SB-1 -\n", "ERR"},
		{"domain with a space-ish char", credSetIdentity + " " + good + " https://api.example.test sb-1 a_b.c\n", "ERR"},
		{"three fields", credSetIdentity + " " + good + " https://api.example.test sb-1\n", "ERR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			identityDir = filepath.Join(dir, "pilldrops")
			reply := handleCredCommand(tc.line, newHashStore(""))
			if !strings.HasPrefix(reply, tc.wantReply) {
				t.Fatalf("reply = %q, want prefix %q", reply, tc.wantReply)
			}
			_, statErr := os.Stat(filepath.Join(identityDir, "token"))
			if tc.wantReply == "ERR" {
				if statErr == nil {
					t.Fatal("a refused push must leave no token behind")
				}
				return
			}
			tok, err := os.ReadFile(filepath.Join(identityDir, "token"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(tok)) != good {
				t.Fatalf("token file = %q", tok)
			}
			if fi, _ := os.Stat(filepath.Join(identityDir, "token")); fi.Mode().Perm() != 0o600 {
				t.Fatalf("token mode = %o, want 0600", fi.Mode().Perm())
			}
			env := readEnvFile(filepath.Join(identityDir, "env"))
			if env["PD_SANDBOX_ID"] != "sb-1-abc" || !strings.HasPrefix(env["PILLDROPS_API_URL"], "http") {
				t.Fatalf("env file = %v", env)
			}
			if _, ok := env["PD_SANDBOX_DOMAIN"]; !ok {
				t.Fatal("PD_SANDBOX_DOMAIN must be present even when empty")
			}
			// The identity is layered into sessions and a request env still wins.
			got := envString(buildSessionEnvFrom(t.TempDir(), map[string]string{"PD_SANDBOX_ID": "override"}))
			if !strings.Contains(got, "PILLDROPS_API_URL="+env["PILLDROPS_API_URL"]) || !strings.Contains(got, "PD_SANDBOX_ID=override") {
				t.Fatalf("session env missing the identity or the request override:\n%s", got)
			}
			// The login-shell drop-in sources the same file.
			profile := filepath.Join(dir, "profile.d")
			if err := writeProfileDropIn(t.TempDir(), profile); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(profile, "zz-pilldrops-env.sh"))
			if !strings.Contains(string(data), ". "+identityDir+"/env") {
				t.Fatalf("drop-in does not source the identity env:\n%s", data)
			}
		})
	}
}
