package golist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	Debug = false
)

type Package struct {
	Dir        string // directory containing package sources
	ImportPath string // import path of package in dir
	// ImportComment string   // path in import comment on package statement
	Name string // package name
	// Doc           string   // package documentation string
	// Target        string   // install path
	// Shlib         string   // the shared library that contains this package (only set when -linkshared)
	// Goroot        bool     // is this package in the Go root?
	Standard bool // is this package part of the standard Go library?
	// Stale         bool     // would 'go install' do anything for this package?
	// StaleReason   string   // explanation for Stale==true
	// Root          string   // Go root or Go path dir containing this package
	// ConflictDir   string   // this directory shadows Dir in $GOPATH
	// BinaryOnly    bool     // binary-only package: cannot be recompiled from sources
	ForTest string // package is only for use in named test
	// Export        string   // file containing export data (when using -export)
	Module *Module  // info about package's containing module, if any (can be nil)
	Match  []string // command-line patterns matching this package
	// DepOnly       bool     // package is only a dependency, not explicitly listed

	// Source files
	GoFiles  []string // .go source files (excluding CgoFiles, TestGoFiles, XTestGoFiles)
	CgoFiles []string // .go source files that import "C"
	// CompiledGoFiles []string // .go files presented to compiler (when using -compiled)
	// IgnoredGoFiles  []string // .go source files ignored due to build constraints
	// CFiles          []string // .c source files
	// CXXFiles        []string // .cc, .cxx and .cpp source files
	// MFiles          []string // .m source files
	// HFiles          []string // .h, .hh, .hpp and .hxx source files
	// FFiles          []string // .f, .F, .for and .f90 Fortran source files
	// SFiles          []string // .s source files
	// SwigFiles       []string // .swig files
	// SwigCXXFiles    []string // .swigcxx files
	// SysoFiles       []string // .syso object files to add to archive
	TestGoFiles  []string // _test.go files in package
	XTestGoFiles []string // _test.go files outside package

	// Cgo directives
	// CgoCFLAGS    []string // cgo: flags for C compiler
	// CgoCPPFLAGS  []string // cgo: flags for C preprocessor
	// CgoCXXFLAGS  []string // cgo: flags for C++ compiler
	// CgoFFLAGS    []string // cgo: flags for Fortran compiler
	// CgoLDFLAGS   []string // cgo: flags for linker
	// CgoPkgConfig []string // cgo: pkg-config names

	// Dependency information
	Imports []string // import paths used by this package
	// ImportMap    map[string]string // map from source import to ImportPath (identity entries omitted)
	// Deps         []string          // all (recursively) imported dependencies
	TestImports  []string // imports from TestGoFiles
	XTestImports []string // imports from XTestGoFiles

	// Error information
	// Incomplete bool            // this package or a dependency has an error
	// Error      *PackageError   // error loading package
	// DepsErrors []*PackageError // errors loading dependencies
}

type Module struct {
	Path    string // module path
	Version string // module version
	// Versions []string     // available module versions (with -versions)
	// Replace  *Module      // replaced by this module
	// Time     *time.Time   // time version was created
	// Update   *Module      // available update, if any (with -u)
	// Main     bool         // is this the main module?
	// Indirect bool         // is this module only an indirect dependency of main module?
	Dir   string // directory holding files for this module, if any
	GoMod string // path to go.mod file for this module, if any
	// Error    *ModuleError // error loading module
}

// type ModuleError struct {
// 	Err string // the error itself
// }

func List(patts []string, opts ...string) ([]*Package, error) {
	cmd := exec.Command("go", "list")
	now := time.Now()
	defer func() {
		if Debug {
			fmt.Fprintf(os.Stderr, "%% [%vms] %v\n", int64(time.Now().Sub(now)/time.Millisecond), strings.Join(cmd.Args, " "))
		}
	}()

	hasDeps := false
	for _, o := range opts {
		if o == "-deps" {
			hasDeps = true
		}
	}

	if !hasDeps {
		opts = append(opts, "-find")
	}

	opts = append(opts, "-json")
	opts, err := adjustTagsFlag(opts)
	if err != nil {
		return nil, err
	}

	cmd.Args = append(cmd.Args, opts...)
	cmd.Args = append(cmd.Args, patts...)

	// TODO optimise by using -f here for only the fields we need

	if Debug {
	}

	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}

		return nil, fmt.Errorf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, stderr)
	}

	dec := json.NewDecoder(bytes.NewReader(out))

	var res []*Package

	for {
		var p Package

		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to decode output from %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
		}

		// For anything go generate related we are simply interested
		// in the package itself. This includes all of the files that
		// we care about (that are not ignored by build constraints).
		if p.ForTest != "" || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}

		res = append(res, &p)
	}

	return res, nil
}

func adjustTagsFlag(opts []string) ([]string, error) {
	var other, tags []string
	if gf := os.Getenv("GOFLAGS"); gf != "" {
		gfs := strings.Fields(gf)
		for _, f := range gfs {
			if strings.HasPrefix(f, "-tags=") {
				tags = append(tags, strings.TrimPrefix(f, "-tags="))
			}
		}
	}
	for i := 0; i < len(opts); i += 1 {
		o := opts[i]
		if strings.HasPrefix(o, "-tags=") {
			tags = append(tags, strings.TrimPrefix(o, "-tags="))
		} else if o == "-tags" {
			if i == len(opts)-1 {
				return nil, fmt.Errorf("missing value for -tags")
			}
			tags = append(tags, opts[i+1])
			i += 1
		} else {
			other = append(other, o)
		}
	}
	if len(tags) > 0 {
		other = append(other, "-tags="+strings.Join(tags, " "))
	}
	return other, nil
}
