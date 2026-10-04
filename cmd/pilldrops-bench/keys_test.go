package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// -help printed both keys whenever they came from the environment, because the
// env value was the flag's default.
func TestHelpDoesNotPrintKeysFromTheEnvironment(t *testing.T) {
	t.Setenv("PILLDROPS_API_KEY", "sk-tenant-secret")
	t.Setenv("PILLDROPS_OPERATOR_KEY", "sk-operator-secret")
	var help bytes.Buffer
	flag.CommandLine.SetOutput(&help)
	t.Cleanup(func() { flag.CommandLine.SetOutput(nil) })
	flag.PrintDefaults()
	if strings.Contains(help.String(), "secret") {
		t.Errorf("-help prints a key:\n%s", help.String())
	}

	oldKey, oldOp := *key, *operatorKey
	t.Cleanup(func() { *key, *operatorKey = oldKey, oldOp })
	*key, *operatorKey = "", ""
	keysFromEnv()
	if *key != "sk-tenant-secret" || *operatorKey != "sk-operator-secret" {
		t.Errorf("keys from env = %q, %q", *key, *operatorKey)
	}
}
