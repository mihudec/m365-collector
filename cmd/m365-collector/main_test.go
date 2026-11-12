package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"collect", "--help"}} {
		cmd := command()
		var b bytes.Buffer
		cmd.SetOut(&b)
		cmd.SetArgs(args)
		if e := cmd.Execute(); e != nil {
			t.Fatal(e)
		}
		if b.Len() == 0 {
			t.Fatal("empty output")
		}
	}
}
func TestInvalidConfiguration(t *testing.T) {
	cmd := command()
	cmd.SetArgs([]string{"collect", "--tenant-id", "invalid"})
	if e := cmd.Execute(); e == nil || !strings.Contains(e.Error(), "UUID") {
		t.Fatalf("expected validation error: %v", e)
	}
}
