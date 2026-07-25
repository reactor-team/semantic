package main

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestMain installs this test binary on $PATH under the name `semantic`, so a
// script line reading `semantic lint` runs the real command in a subprocess —
// same argument parsing, same exit codes, same stdout and stderr a user sees.
//
// The scripts live beside this file because a command tree is `package main`
// and cannot be imported. Splitting a shim package out just to host the tests
// elsewhere would change the shape of the program to suit its test.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"semantic": main,
	})
}

// TestE2E runs every script in testdata as an end-to-end exercise of the CLI:
// a real vault on disk, a real SQLite index, real chunking and link
// extraction.
//
// The scripts stay off the embedding path, which needs a ~90 MB model and a
// native ONNX runtime that CI does not have. That still leaves most of the
// program covered — `lint --no-embed` indexes the whole vault, and
// `--no-reindex` lets the graph commands read what it built. What embedding
// gates is asserted the other way round: that the command fails with the
// message telling the user to run `semantic init`.
func TestE2E(t *testing.T) {
	t.Parallel()
	testscript.Run(t, testscript.Params{Dir: "testdata"})
}
