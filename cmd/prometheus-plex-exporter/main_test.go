package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The chatter check runs in a SUBPROCESS on purpose: the claim is that fmt.Println reaches
// nowhere, which a same-process assertion on the value of os.Stdout would not establish.
func TestMain(m *testing.M) {
	if os.Getenv("GO_TEST_CHATTER_SUBPROCESS") != "" {
		silenceClientChatter()
		fmt.Println("client-chatter")           // stands in for the plex client's fmt.Print*
		fmt.Fprintln(os.Stderr, "our-log-line") // stands in for our kitlog output
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runChatter(t *testing.T, extraEnv ...string) (stdout, stderr string) {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	// PLEX_CLIENT_DEBUG is cleared first so an operator's shell cannot flip the result;
	// extraEnv comes last because exec keeps the final value of a repeated key.
	cmd.Env = append(os.Environ(), "GO_TEST_CHATTER_SUBPROCESS=1", "PLEX_CLIENT_DEBUG=")
	cmd.Env = append(cmd.Env, extraEnv...)

	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("chatter subprocess failed: %v (stderr %q)", err, errOut.String())
	}
	return out.String(), errOut.String()
}

// TestSilenceClientChatterDiscardsStdout is the whole point: the plex client's unconditional
// fmt.Print* was 98.3% of this pod's log lines and it offers no toggle, so stdout is thrown
// away. ⚠ Our own logging must survive, which is why stderr is asserted in the same run.
func TestSilenceClientChatterDiscardsStdout(t *testing.T) {
	stdout, stderr := runChatter(t)

	if strings.Contains(stdout, "client-chatter") {
		t.Errorf("client chatter still reached stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "our-log-line") {
		t.Errorf("our own stderr logging was suppressed as collateral: %q", stderr)
	}
}

// TestPlexClientDebugKeepsStdout is the control. Without it a pass above proves only that
// the subprocess produced no stdout — including the case where it never ran at all.
func TestPlexClientDebugKeepsStdout(t *testing.T) {
	stdout, _ := runChatter(t, "PLEX_CLIENT_DEBUG=1")

	if !strings.Contains(stdout, "client-chatter") {
		t.Errorf("PLEX_CLIENT_DEBUG did not restore stdout: %q", stdout)
	}
}
