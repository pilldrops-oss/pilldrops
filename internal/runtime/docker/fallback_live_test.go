package docker

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEngineAndCLIPathsAgree(t *testing.T) {
	eng := newEngine()
	if eng == nil {
		t.Skip("no reachable docker socket — nothing to compare against")
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "alpine:3", "sleep", "120").Output()
	if err != nil {
		t.Skipf("cannot start a container: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })

	viaEngine := &dockerFS{cid: cid, eng: eng}
	viaCLI := &dockerFS{cid: cid} // eng nil -> CLI

	ctx := t.Context()
	for _, name := range []string{
		"/tmp/plain.txt",
		"/tmp/with space.txt",
		"/tmp/with$dollar.txt",
		"/tmp/x$(touch /tmp/PWNED)y",
	} {
		body := []byte("content of " + name)

		if err := viaEngine.Write(ctx, name, body); err != nil {
			t.Errorf("engine write %q: %v", name, err)
			continue
		}
		got, err := viaCLI.Read(ctx, name)
		if err != nil || string(got) != string(body) {
			t.Errorf("engine wrote, CLI read %q: err=%v got=%q", name, err, got)
		}

		if err := viaCLI.Write(ctx, name+".cli", body); err != nil {
			t.Errorf("cli write %q: %v", name, err)
			continue
		}
		got, err = viaEngine.Read(ctx, name+".cli")
		if err != nil || string(got) != string(body) {
			t.Errorf("CLI wrote, engine read %q: err=%v got=%q", name, err, got)
		}

		se, errE := viaEngine.Stat(ctx, name)
		sc, errC := viaCLI.Stat(ctx, name)
		if (errE == nil) != (errC == nil) {
			t.Errorf("stat %q disagrees on error: engine=%v cli=%v", name, errE, errC)
			continue
		}
		if errE == nil && (se.Size != sc.Size || se.IsDir != sc.IsDir) {
			t.Errorf("stat %q disagrees: engine=%+v cli=%+v", name, se, sc)
		}
	}

	if _, _, code, _ := viaCLI.exec(ctx, "test", "-e", "/tmp/PWNED"); code == 0 {
		t.Fatal("COMMAND INJECTION: /tmp/PWNED exists")
	}

	// The HTTP layer maps fs.ErrNotExist to 404, so both paths must agree on it.
	_, errE := viaEngine.Read(ctx, "/tmp/absent")
	_, errC := viaCLI.Read(ctx, "/tmp/absent")
	if (errE == nil) != (errC == nil) || errE.Error() == "" || errC.Error() == "" {
		t.Errorf("missing-file behaviour differs: engine=%v cli=%v", errE, errC)
	}
}
