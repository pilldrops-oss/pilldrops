package nodeboot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirecrackerDirFindsAProvisionedKernel(t *testing.T) {
	data, provisioned := t.TempDir(), t.TempDir()
	kernelIn := func(dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "vmlinux"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := firecrackerDir(data, provisioned); got != data {
		t.Errorf("no kernel anywhere: %q, want the data dir", got)
	}
	kernelIn(provisioned)
	if got := firecrackerDir(data, provisioned); got != provisioned {
		t.Errorf("kernel only provisioned: %q, want %q", got, provisioned)
	}
	kernelIn(data)
	if got := firecrackerDir(data, provisioned); got != data {
		t.Errorf("kernel in both: %q, want the data dir", got)
	}
}
