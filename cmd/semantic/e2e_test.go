package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/reactor-team/semantic/pkg/embed"
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
// Most scripts stay off the embedding path, which needs a ~127 MB model and a
// native ONNX runtime that CI does not have. That still leaves most of the
// program covered — `lint` indexes the whole vault without embedding, and
// `--no-reindex` lets the graph commands read what it built. What embedding
// gates is asserted the other way round: that the command fails with the
// message telling the user to run `semantic init`.
//
// The scripts that do want a real model declare `[embed]` and read the
// developer's own cache through $REAL_MODEL_DIR and $REAL_ORT_LIB. They skip
// where the model is absent, so CI stays offline and a contributor who has run
// `semantic init` gets the coverage for free.
func TestE2E(t *testing.T) {
	t.Parallel()
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		// SEMANTIC_NO_DOWNLOAD is set here rather than per script because the
		// commands now fetch a missing model instead of failing. Each script
		// runs in its own $WORK, so one that reached the download path would
		// pull 127 MB, and every other script would pull it again.
		Setup: func(env *testscript.Env) error {
			env.Vars = append(env.Vars,
				"SEMANTIC_NO_DOWNLOAD=1",
				"REAL_MODEL_DIR="+embed.ModelCacheDir(),
				"REAL_ORT_LIB="+filepath.Join(embed.OrtCacheDir(), embed.OrtLibFilename()),
			)
			return nil
		},
		Condition: func(cond string) (bool, error) {
			if cond == "embed" {
				return embed.Check() == nil, nil
			}
			return false, fmt.Errorf("unknown condition %q", cond)
		},
	})
}
