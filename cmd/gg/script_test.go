package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/goproxytest"
	"github.com/rogpeppe/go-internal/gotooltest"
	"github.com/rogpeppe/go-internal/testscript"
)

var (
	proxyURL  string
	gobinPath string
)

func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(ggMain{m}, map[string]func() int{
		"gg": main1,
	}))
}

type ggMain struct {
	m *testing.M
}

func (m ggMain) Run() int {
	// Start the Go proxy server running for all tests.
	srv, err := goproxytest.NewServer("testdata/mod", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot start proxy: %v", err)
		return 1
	}
	proxyURL = srv.URL

	td, err := ioutil.TempDir("", "gg_gobin_tmp_")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir for gobin install: %v", err)
		return 1
	}

	defer func() {
		os.RemoveAll(td)
	}()

	cmd := exec.Command("go", "install", "github.com/myitcv/gobin")
	cmd.Env = append(os.Environ(), "GOBIN="+td)

	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
		return 1
	}

	gobinPath = filepath.Join(td, "gobin")

	return m.m.Run()
}

func TestScripts(t *testing.T) {
	p := testscript.Params{
		Dir: "testdata",
		Setup: func(e *testscript.Env) error {
			var newEnv []string
			var path string
			for _, e := range e.Vars {
				if strings.HasPrefix(e, "PATH=") {
					path = e
				} else {
					newEnv = append(newEnv, e)
				}
			}
			e.Vars = newEnv
			path = strings.TrimPrefix(path, "PATH=")
			e.Vars = append(e.Vars,
				"PATH="+filepath.Dir(gobinPath)+string(os.PathListSeparator)+path,
				"GOPROXY="+proxyURL,
				"GOBINPATH="+gobinPath,
			)

			return nil
		},
	}
	if err := gotooltest.Setup(&p); err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, p)
}
