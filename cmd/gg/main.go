package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/rogpeppe/go-internal/cache"
	"github.com/rogpeppe/go-internal/imports"
	"myitcv.io/cmd/gg/internal/golist"
	"myitcv.io/gogenerate"
)

const (
	hashSize  = 32
	debug     = true
	trace     = false
	hashDebug = false
)

var (
	flagSet = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fDebug  = flagSet.Bool("debug", debug, "debug mode")
	fTrace  = flagSet.Bool("trace", trace, "trace timings")

	nilHash [hashSize]byte

	tabber    = tabwriter.NewWriter(os.Stderr, 0, 0, 1, ' ', tabwriter.AlignRight)
	startTime = time.Now()
	lastTime  = startTime
)

func logTiming(format string, args ...interface{}) {
	ms := func(pre, post time.Time) int64 {
		return int64(post.Sub(pre) / time.Millisecond)
	}
	if *fDebug {
		now := time.Now()
		fmt.Fprintf(tabber, "%v\t %v\t - %v\n", ms(startTime, now), ms(lastTime, now), fmt.Sprintf(format, args...))
		lastTime = now
	}
}

func main() {
	os.Exit(main1())
}

func main1() int {
	logTiming("start main")
	defer tabber.Flush()
	defer logTiming("end main")
	if err := mainerr(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newDepsMap() depsMap {
	return depsMap{
		deps:      make(map[dep]bool),
		rdeps:     make(map[dep]bool),
		dirtydeps: make(map[dep]bool),
	}
}

func mainerr() (reterr error) {
	flagSet.Usage = func() {
		mainUsage(os.Stderr)
	}
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return err
	}

	// golist.Debug = *fDebug

	mm, err := mainMod()
	if err != nil {
		return fmt.Errorf("failed to determine main module: %v", err)
	}

	// TODO officially support GOPATH mode at some point?
	if mm == "" {
		return fmt.Errorf("only support module mode for now")
	}

	ucd, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("failed to determine user cache dir: %v", err)
	}

	artefactsCacheDir := filepath.Join(ucd, "gg-artefacts")
	if err := os.MkdirAll(artefactsCacheDir, 0777); err != nil {
		return fmt.Errorf("failed to create build cache dir %v: %v", artefactsCacheDir, err)
	}

	artefactsCache, err := cache.Open(artefactsCacheDir)
	if err != nil {
		return fmt.Errorf("failed to open build cache dir: %v", err)
	}
	defer artefactsCache.Trim()

	td, err := ioutil.TempDir("", "gg-workings")
	if err != nil {
		return fmt.Errorf("failed to create temp dir for workings: %v", err)
	}
	defer os.RemoveAll(td)

	gg := &gg{
		pkgLookup:         make(map[string]*pkg),
		dirLookup:         make(map[string]*pkg),
		gobinModLookup:    make(map[string]*gobinModDep),
		gobinGlobalLookup: make(map[string]string),
		gobinGlobalCache:  make(map[string]*gobinGlobalDep),
		commLookup:        make(map[string]*commandDep),
		tags:              buildTags(),
		cache:             artefactsCache,
		tempDir:           td,
	}
	gg.cliPatts = flagSet.Args()
	gg.mainMod = mm

	if !*fDebug {
		defer func() {
			if err := recover(); err != nil {
				if rerr, ok := err.(error); ok {
					reterr = rerr
				} else {
					panic(fmt.Errorf("got something other than an error: %v [%T]", err, err))
				}
			}
		}()
	}

	gg.run()

	return reterr
}

func buildTags() map[string]bool {
	// TODO take -tags on the command line and also understand GOFLAGS with
	// -tags present

	res := make(map[string]bool)

	goos := os.Getenv("GOOS")
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := os.Getenv("GOARCH")
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	res[goos] = true
	res[goarch] = true

	return res
}

func mainMod() (string, error) {
	// TODO performance: instead of exec-ing we could work this out ourselves (~16ms)
	var stderr bytes.Buffer
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, stderr.Bytes())
	}

	return strings.TrimSpace(string(out)), nil
}

type gg struct {
	cliPatts []string
	mainMod  string

	// slice of all deps. Uniqueness is guaranteed by the maps that follow
	allDeps []dep

	// map of import path to pkg. This is the definitive set of all *pkg
	pkgLookup map[string]*pkg

	// map of directory to pkg
	dirLookup map[string]*pkg

	// map of import path to dep
	gobinModLookup map[string]*gobinModDep

	// map of import path to target file
	gobinGlobalLookup map[string]string
	// map of target file to dep
	gobinGlobalCache map[string]*gobinGlobalDep

	// map of command name to dep
	commLookup map[string]*commandDep

	tags map[string]bool

	cache *cache.Cache

	tempDir string
}

type directive struct {
	pkgName string
	file    string
	line    int
	args    []string
	gen     generator
	outDirs []string
}

func (d directive) String() string {
	return fmt.Sprintf("{pkgName: %v, pos: %v:%v, args: %v, gen: [%v], outDirs: %v}", d.pkgName, d.file, d.line, d.args, d.gen, d.outDirs)
}

func (d directive) HashString() string {
	return d.String()
}

type pkg struct {
	importPath string
	dir        string

	// generate indicates whether we should run generate on this package
	generate bool

	// generated indicates that the package has been generated
	generated bool

	*golist.Package

	hash [hashSize]byte

	depsMap

	dirs []directive
}

type depsMap struct {
	deps      map[dep]bool
	rdeps     map[dep]bool
	dirtydeps map[dep]bool
}

func (g *gg) addDep(d dep, nd dep) {
	if *fDebug {
		var count int
		_, depsok := d.Deps().deps[nd]
		if depsok {
			count++
		}
		_, ddepsok := d.Deps().dirtydeps[nd]
		if ddepsok {
			count++
		}
		_, rdepsok := nd.Deps().rdeps[d]
		if rdepsok {
			count++
		}
		if count != 0 && count != 3 {
			g.fatalf("inconsistency in terms of deps %v and %v; %v %v %v", d, nd, depsok, ddepsok, rdepsok)
		}
	}
	d.Deps().deps[nd] = true
	if !nd.Done() {
		d.Deps().dirtydeps[nd] = true
	}
	nd.Deps().rdeps[d] = true
}

func (g *gg) dropDep(d dep, od dep) {
	if _, ok := d.Deps().deps[od]; !ok {
		g.fatalf("inconsistency: %v is not a dep of %v", od, d)
	}
	if _, ok := od.Deps().rdeps[d]; !ok {
		g.fatalf("inconsistency: %v is not an rdep of %v", d, od)
	}
	delete(d.Deps().deps, od)
	delete(d.Deps().dirtydeps, od)
	delete(od.Deps().rdeps, d)
}

func (g *gg) doneDep(dd dep) []dep {
	if !dd.Done() {
		g.fatalf("tried to mark %v as done, but it's not done", dd)
	}
	var work []dep
	for d := range dd.Deps().rdeps {
		if _, ok := d.Deps().deps[dd]; !ok {
			g.fatalf("failed to mark dep %v as done; is not currently a dep", dd)
		}
		if _, ok := d.Deps().dirtydeps[dd]; !ok {
			g.fatalf("failed to mark dep %v as done; is not currently dirty", dd)
		}
		delete(d.Deps().dirtydeps, dd)
		if len(d.Deps().dirtydeps) == 0 {
			work = append(work, d)
		}
	}
	return work
}

func (p *pkg) Deps() depsMap {
	return p.depsMap
}

func (p *pkg) Ready() bool {
	return p.Package != nil && len(p.dirtydeps) == 0
}

func (p *pkg) Done() bool {
	return p.Ready() && (!p.generate || p.generated) && p.hash != nilHash
}

func (p *pkg) Undo() {
	p.generated = false
	p.hash = nilHash
}

func (p *pkg) String() string {
	var generate string
	if p.generate {
		generate = " [G]"
	}
	return fmt.Sprintf("{Pkg: %v%v}", p.Package.ImportPath, generate)
}

func (p *pkg) HashString() string {
	return fmt.Sprintf("pkg: %v [%x]", p.importPath, p.hash)
}

var _ dep = (*pkg)(nil)

// load loads the packages resolved by patts and their deps
func (g *gg) run() {
	logTiming("start run")
	// Resolve patterns to pkgs
	pkgs, err := golist.List(g.cliPatts, "-deps", "-test")
	if err != nil {
		g.fatalf("failed to load patterns [%v]: %v", strings.Join(g.cliPatts, " "), err)
	}
	logTiming("initial list complete")

	// If we are in module mode, work out whether we are being asked to generate
	// for packages outside the main module. If we are this is an error. In GOPATH
	// mode, there is no such thing as main module, and any other packages will be
	// within the writable GOPATH... so this is allowed.
	var badPkgs []string
	for _, p := range pkgs {
		if p.Module != nil && p.Module.GoMod != g.mainMod {
			badPkgs = append(badPkgs, p.ImportPath)
		}
	}
	if len(badPkgs) > 0 {
		g.fatalf("cannot generate packages outside the main module: %v", strings.Join(badPkgs, ", "))
	}

	// Populate the import path and directory lookup maps, and mark *pkg according
	// to whether they should be generated. Keep a slice of those to be generated
	// for convenience.
	for _, p := range pkgs {
		np := g.newPkg(p)
		if len(p.Match) != 0 {
			np.generate = true
		}
	}

	// Calculate dependency and reverse dependency graph. For packages that we are generating,
	// also consider test imports.
	for _, p := range g.pkgLookup {
		seen := make(map[string]bool)
		var imports []string
		imports = append(imports, p.Imports...)

		if p.generate {
			imports = append(imports, p.TestImports...)
			imports = append(imports, p.XTestImports...)
		}

		for _, ip := range imports {
			if seen[ip] {
				continue
			}
			dep, ok := g.pkgLookup[ip]
			if !ok {
				g.fatalf("failed to resolve import %v", ip)
			}
			g.addDep(p, dep)
			seen[ip] = true
		}
	}
	logTiming("dep graph complete")

	// There is one class of generator that we might not have
	// seen as part of the previous go list; gobin -m (and indeed
	// go run). Such generators will resolve their dependencies via
	// the main module, but go:generate directives are not seen by
	// the go tool. There is no way to automatically include such
	// tools (go list -tags tools results in multiple packages in the
	// the same directory).
	//
	// Similarly, when we are generating code later, it might be that
	// dependencies get "added" as part of the generation process
	// that we have not seen yet. After each phase (and this discovery
	// of the generators is such a phase) we might therefore need to
	// resolve a number of imports. We do this in a single command to
	// keep things efficient.
	gobinModMisses := make(missingDeps)

	// go:generate directive search amongst *pkg to generate. If we happen
	// to find a generator which itself is a generate target that's fine;
	// we will simply end up recording the dependency on that generator
	// (it can't by definition be an import dependency).
	for _, p := range g.pkgLookup {
		if !p.generate {
			continue
		}
		g.refreshDirectiveDeps(p, gobinModMisses)
	}
	logTiming("initial refreshDirectiveDeps complete")

	g.loadMisses(nil, gobinModMisses)

	logTiming("initial loadMisses complete")

	// At this point we should have a complete dependency graph, including the generators.
	// Find roots and start work
	var work []dep
	for _, d := range g.allDeps {
		if d.Ready() {
			work = append(work, d)
		}
	}

	logTiming("start work")

	for len(work) > 0 {
		// It may be that since we added a piece of work one of its deps has been
		// marked as not ready, and hence len(w.Deps().dirtyDeps) > 0. Drop any
		// such work
		var rem []dep
		todo := make(map[dep]bool)
		var haveGenerate bool
		for _, w := range work {
			if !w.Ready() {
				continue
			}
			if p, ok := w.(*pkg); ok && p.generate {
				if haveGenerate || len(todo) > 0 {
					rem = append(rem, w)
				} else {
					haveGenerate = true
					todo[w] = true
				}
			} else if haveGenerate {
				rem = append(rem, w)
			} else {
				todo[w] = true
			}
		}
		work = rem
		var todoOrder []dep
		for w := range todo {
			todoOrder = append(todoOrder, w)
		}
		sortDeps(todoOrder)
		var wg sync.WaitGroup
		for _, w := range todoOrder {
			wg.Add(1)
			func(w dep) {
				defer wg.Done()
				switch w := w.(type) {
				case *pkg:
					if w.generate {
						var pre map[string][hashSize]byte
						for {
							deltaDirs := make(map[string]bool)
							canContinue := func() bool {
								importMisses := make(missingDeps)
								var newWork []dep

								for od := range deltaDirs {
									odp, ok := g.dirLookup[od]
									if !ok {
										continue
									}
									g.undo(odp)
									g.refreshImports(odp, importMisses)
									newWork = append(newWork, odp)
								}

								dirMisses := make(missingDeps)
								g.refreshDirectiveDeps(w, dirMisses)

								g.loadMisses(importMisses, dirMisses)

								// if the current package is still ready then we continue
								// for another round of generation
								if w.Ready() {
									return true
								}

								var nw dep
								for len(newWork) > 0 {
									nw, newWork = newWork[0], newWork[1:]
									if nw.Done() {
										continue
									}
									if nw.Ready() {
										work = append(work, nw)
									} else {
										for d := range nw.Deps().deps {
											newWork = append(newWork, d)
										}
									}
								}

								// because at this point the current piece of work cannot
								// be finished and so we need to bail early
								return false
							}

							hw := newHash("## generate " + w.ImportPath)
							fmt.Fprintf(hw, "Deps:\n")
							g.hashDeps(hw, w)
							fmt.Fprintf(hw, "Directives:\n")
							outDirs := make(map[string]bool)
							dirNames := make(map[string][]generator)
							for _, d := range w.dirs {
								fmt.Fprintf(hw, "%v\n", d.HashString())
								dirNames[d.gen.DirectiveName()] = append(dirNames[d.gen.DirectiveName()], d.gen)
								for _, od := range d.outDirs {
									outDirs[od] = true
								}
							}
							outDirs[w.Dir] = true
							var outDirOrder []string
							for od := range outDirs {
								outDirOrder = append(outDirOrder, od)
							}
							sort.Strings(outDirOrder)
							for dn, gens := range dirNames {
								if len(gens) > 1 {
									var genStrings []string
									for _, g := range gens {
										genStrings = append(genStrings, g.String())
									}
									g.fatalf("package %v has go:generate directive name clashes (%v) between %v", w.ImportPath, dn, genStrings)
								}
							}
							fmt.Fprintf(hw, "Files:\n")
							var files []string
							files = append(files, w.GoFiles...)
							files = append(files, w.CgoFiles...)
							files = append(files, w.TestGoFiles...)
							files = append(files, w.XTestGoFiles...)
							for _, fn := range files {
								fmt.Fprintf(hw, "file: %v\n", fn)
								g.hashFile(hw, w.Dir, fn)
							}

							if fp, _, err := g.cache.GetFile(hw.Sum()); err == nil {
								r, err := newArchiveReader(fp)
								if err != nil {
									goto CacheMiss
								}
								for {
									fn, err := r.ExtractFile()
									if err != nil {
										if err == io.EOF {
											break
										}
										goto CacheMiss
									}
									deltaDirs[filepath.Dir(fn)] = true
								}
								if err := r.Close(); err != nil {
									goto CacheMiss
								}
								if len(deltaDirs) == 0 {
									// zero delta to apply; we are done
									break
								}

								if canContinue() {
									continue
								}
								return
							}
						CacheMiss:

							// if we get here we had a cache miss so we are going to have to run
							// go generate

							hashOutFiles := func() map[string][hashSize]byte {
								fileHashes := make(map[string][hashSize]byte)
								for _, od := range outDirOrder {
									g.hashOutDir(od, dirNames, fileHashes)
								}
								return fileHashes
							}

							if pre == nil {
								pre = hashOutFiles()
							}

							cmd := exec.Command("go", "generate")
							cmd.Dir = w.Dir

							out, err := cmd.CombinedOutput()
							if err != nil {
								g.fatalf("failed to run %v in %v: %v\n%s", strings.Join(cmd.Args, " "), w.Dir, err, out)
							}

							post := hashOutFiles()

							ar := g.newArchive()

							// TODO work out if/how we handle file removals, i.e. files that got _removed_
							// by any of the generators in any of the output directories
							var delta []string
							for fn, posth := range post {
								preh, ok := pre[fn]
								if !ok || preh != posth {
									delta = append(delta, fn)
									deltaDirs[filepath.Dir(fn)] = true
								}
							}
							if len(delta) == 0 {
								if err := g.cachePutArchive(hw.Sum(), ar); err != nil {
									g.fatalf("failed to put zero-length archive: %v", err)
								}
								break
							}
							pre = post

							sort.Strings(delta)
							fmt.Printf("generate for %v found delta: %v\n", w.ImportPath, delta)
							for _, f := range delta {
								if err := ar.PutFile(f); err != nil {
									g.fatalf("failed to put %v into archive: %v", f, err)
								}
							}
							if err := g.cachePutArchive(hw.Sum(), ar); err != nil {
								g.fatalf("failed to write archive to cache: %v", err)
							}

							if canContinue() {
								continue
							}
							return
						}
						w.generated = true
					}
					hw := newHash("## pkg " + w.ImportPath)
					fmt.Fprintf(hw, "## pkg %v\n", w.ImportPath)
					for _, i := range w.Imports {
						g.hashImport(hw, i)
					}
					var files []string
					files = append(files, w.GoFiles...)
					files = append(files, w.CgoFiles...)
					for _, fn := range files {
						g.hashFile(hw, w.Dir, fn)
					}
					w.hash = hw.Sum()
				case *commandDep:
					hw := newHash("## commandDep " + w.name)
					fp, err := exec.LookPath(w.name)
					if err != nil {
						g.fatalf("failed to find %v in PATH: %v", w.name, err)
					}
					g.hashFile(hw, "", fp)
					w.hash = hw.Sum()
				case *gobinGlobalDep:
					hw := newHash("## gobinGlobalDep " + w.targetPath)
					g.hashFile(hw, "", w.targetPath)
					w.hash = hw.Sum()
				case *gobinModDep:
					w.hash = w.pkg.hash
				}
			}(w)
		}
		wg.Wait()
		logTiming("round complete %v", todoOrder)
		for _, w := range todoOrder {
			work = append(work, g.doneDep(w)...)
		}
	}
}

type hash struct {
	h        *cache.Hash
	hw       io.Writer
	debugOut *bytes.Buffer
}

func (h *hash) Write(p []byte) (n int, err error) {
	return h.hw.Write(p)
}

func (h *hash) String() string {
	return h.debugOut.String()
}

func (h *hash) Sum() [hashSize]byte {
	return h.h.Sum()
}

func newHash(name string) *hash {
	res := &hash{
		h: cache.NewHash(name),
	}
	if hashDebug {
		res.debugOut = new(bytes.Buffer)
		res.hw = io.MultiWriter(res.debugOut, res.h)
	} else {
		res.hw = res.h
	}
	return res
}

func (g *gg) newArchive() *archiveWriter {
	ar, err := newArchiveWriter(g.tempDir, "")
	if err != nil {
		g.fatalf("failed to create archive: %v", err)
	}
	return ar
}

func (g *gg) cachePutArchive(id cache.ActionID, ar *archiveWriter) error {
	if err := ar.Close(); err != nil {
		return fmt.Errorf("failed to close archive: %v", ar)
	}
	f, err := os.Open(ar.file.Name())
	if err != nil {
		return fmt.Errorf("failed to open archive %v for reading: %v", ar.file.Name(), err)
	}
	if _, _, err := g.cache.Put(id, f); err != nil {
		return fmt.Errorf("failed to write archive to cache: %v", err)
	}
	return nil
}

func (g *gg) loadMisses(pkgMisses, dirMisses missingDeps) {
	if len(pkgMisses) == 0 && len(dirMisses) == 0 {
		return
	}
	var paths []string
	patts := make(map[string]bool)
	for ip := range pkgMisses {
		patts[ip] = true
		paths = append(paths, ip)
	}
	for ip := range dirMisses {
		patts[ip] = true
		paths = append(paths, ip)
	}
	pkgs, err := golist.List(paths, "-deps")
	if err != nil {
		g.fatalf("failed to load patterns [%v]: %v", strings.Join(paths, " "), err)
	}

	// We might already have loaded a pkg that is a dep of one of the misses
	// Skip those (after a quick dir check); should be a no-op

	// We also do a check to ensure that the number of packages returned with a
	// Match field is equal to the number of misses, and that furthermore the
	// misses' patterns match exactly the import path of the package returned
	// i.e. we don't support relative gobin -m -run $pkg specs.

	var newPkgs []*pkg
	for _, p := range pkgs {
		if len(p.Match) > 0 {
			if len(p.Match) != 1 {
				g.fatalf("multiple patterns resolved to %v: %v", p.ImportPath, p.Match)
			}
			if p.Match[0] != p.ImportPath {
				g.fatalf("pattern %v was not identical to import path %v", p.Match[0], p.ImportPath)
			}
			delete(patts, p.ImportPath)
		}
		if _, ok := g.pkgLookup[p.ImportPath]; ok {
			continue
		}
		newPkgs = append(newPkgs, g.newPkgImpl(p))
	}

	if len(patts) > 0 {
		var ps []string
		for p := range patts {
			ps = append(ps, p)
		}
		g.fatalf("failed to resolve patterns %v to package(s)", ps)
	}

	for _, p := range newPkgs {
		seen := make(map[string]bool)
		var imports []string
		imports = append(imports, p.Imports...)
		for _, ip := range imports {
			if seen[ip] {
				continue
			}
			dep, ok := g.pkgLookup[ip]
			if !ok {
				g.fatalf("failed to resolve import %v", ip)
			}
			g.addDep(p, dep)
			seen[ip] = true
		}
	}

	for ip, deps := range pkgMisses {
		np := g.pkgLookup[ip]
		if np == nil {
			g.fatalf("inconsistency in state of g.pkgLookup: could not find %v", ip)
		}
		for d := range deps {
			g.addDep(d, np)
		}
	}

	for ip, deps := range dirMisses {
		mod := g.gobinModLookup[ip]
		if mod == nil {
			g.fatalf("inconsistency in state of g.gobinModLookup: could not find %v", ip)
		}
		p := g.pkgLookup[ip]
		if p == nil {
			g.fatalf("inconsistency in state of g.pkgLookup: could not find %v", ip)
		}
		mod.pkg = p
		g.addDep(mod, p)
		for d := range deps {
			g.addDep(d, mod)
		}
	}
}

func (g *gg) refreshImports(p *pkg, misses missingDeps) {
	fset := token.NewFileSet()
	deps := make(map[*pkg]bool)
	matchFile := func(fi os.FileInfo) bool {
		return imports.MatchFile(fi.Name(), g.tags)
	}
	pkgs, err := parser.ParseDir(fset, p.Dir, matchFile, parser.ParseComments|parser.ImportsOnly)
	if err != nil {
		g.fatalf("failed to parse %v: %v", p.Dir, err)
	}
	p.GoFiles = nil
	p.CgoFiles = nil
	p.TestGoFiles = nil
	p.XTestGoFiles = nil
	for ppn, pp := range pkgs {
		isXTest := strings.HasSuffix(ppn, "_test")
		for _, f := range pp.Files {
			var b bytes.Buffer
			for _, cg := range f.Comments {
				if cg == f.Doc {
					break
				}
				for _, c := range cg.List {
					b.WriteString(c.Text + "\n")
				}
				b.WriteString("\n")
			}
			if !imports.ShouldBuild(b.Bytes(), g.tags) {
				continue
			}
			var cImport bool
			for _, i := range f.Imports {
				ip := strings.Trim(i.Path.Value, "\"")
				if ip == "C" {
					cImport = true
				}
				if rp, ok := g.pkgLookup[ip]; ok {
					deps[rp] = true
				} else {
					misses.add(ip, p)
				}
			}

			fn := filepath.Base(fset.Position(f.Pos()).Filename)
			if isXTest {
				p.XTestGoFiles = append(p.XTestGoFiles, fn)
			} else if strings.HasSuffix(fn, "_test.go") {
				p.TestGoFiles = append(p.TestGoFiles, fn)
			} else if cImport {
				p.CgoFiles = append(p.CgoFiles, fn)
			} else {
				p.GoFiles = append(p.GoFiles, fn)
			}
		}
	}
	for cd := range p.Deps().deps {
		if cd, ok := cd.(*pkg); ok {
			if _, isNd := deps[cd]; !isNd {
				g.dropDep(p, cd)
			}
		}
	}
	for nd := range deps {
		if _, isCd := p.Deps().deps[nd]; !isCd {
			g.addDep(p, nd)
		}
	}
	sort.Strings(p.GoFiles)
	sort.Strings(p.CgoFiles)
	sort.Strings(p.TestGoFiles)
	sort.Strings(p.XTestGoFiles)
}

func (g *gg) refreshDirectiveDeps(p *pkg, misses missingDeps) {
	if !p.generate {
		g.fatalf("inconsistency: %v is not marked for generation", p)
	}

	p.dirs = nil

	var files []string
	files = append(files, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	files = append(files, p.TestGoFiles...)
	files = append(files, p.XTestGoFiles...)

	for _, file := range files {
		err := gogenerate.DirFunc(p.Name, p.Dir, file, func(line int, dirArgs []string) error {
			gen := g.resolveDir(p.Dir, dirArgs)
			outDirs, err := parseOutDirs(p.Dir, dirArgs)
			if err != nil {
				return fmt.Errorf("failed to parse out dirs in %v:%v: %v", filepath.Join(p.Dir, file), line, err)
			}
			for i, v := range outDirs {
				if v == p.Dir {
					outDirs = append(outDirs[:i], outDirs[i+1:]...)
					break
				}
			}
			p.dirs = append(p.dirs, directive{
				pkgName: p.Name,
				file:    file,
				line:    line,
				args:    dirArgs,
				gen:     gen,
				outDirs: outDirs,
			})
			switch gen := gen.(type) {
			case *gobinModDep:
				// If this gobinModDep does not have an underlying pkg, then we haven't previously
				// seen the package's dependencies. We need to resolve the package first (next phase)
				// and then add the dependency. In this next phase we will also ensure that the
				// directive specified import path is valid, i.e. resolves to a single package
				if gen.pkg == nil {
					if _, ok := g.pkgLookup[gen.importPath]; !ok {
						misses.add(gen.importPath, p)
					}
				} else {
					if _, ok := p.Deps().deps[gen]; !ok {
						g.addDep(p, gen)
					}
				}
			default:
				if _, ok := p.Deps().deps[gen]; !ok {
					g.addDep(p, gen)
				}
			}
			return nil
		})

		if err != nil {
			g.fatalf("failed to walk %v%v%v for go:generate directives: %v", p.Dir, string(os.PathSeparator), file, err)
		}
	}
}

func (g *gg) undo(d dep) {
	var w dep
	work := []dep{d}
	for len(work) > 0 {
		w, work = work[0], work[1:]
		if !w.Done() {
			continue
		}
		w.Undo()
		for rd := range w.Deps().rdeps {
			work = append(work, rd)
		}
	}
}

func (g *gg) hashFile(hw io.Writer, dir, file string) {
	fp := filepath.Join(dir, file)
	fmt.Fprintf(hw, "file: %v\n", fp)
	f, err := os.Open(fp)
	if err != nil {
		g.fatalf("failed to open %v: %v", fp, err)
	}
	defer f.Close()
	if _, err := io.Copy(hw, f); err != nil {
		g.fatalf("failed to hash %v: %v", fp, err)
	}
}

func (g *gg) hashOutDir(dir string, dirNames map[string][]generator, fileHash map[string][hashSize]byte) {
	ls, err := ioutil.ReadDir(dir)
	if err != nil {
		g.fatalf("failed to list contents of %v: %v", dir, err)
	}
	for _, fi := range ls {
		if fi.IsDir() {
			continue
		}
		fn := fi.Name()
		if !g.shouldBuildFile(filepath.Join(dir, fn)) {
			continue
		}
		for dn := range dirNames {
			if gogenerate.AnyFileGeneratedBy(fn, dn) {
				fhw := newHash("## file " + fn)
				g.hashFile(fhw, dir, fn)
				fileHash[filepath.Join(dir, fn)] = fhw.Sum()
			}
		}
	}
}

func (g *gg) shouldBuildFile(path string) bool {
	mf := imports.MatchFile(path, g.tags)
	if !mf {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		g.fatalf("failed to open %v\n", f)
	}
	defer f.Close()
	cmts, err := imports.ReadComments(f)
	if err != nil {
		g.fatalf("failed to read comments from %v: %v", path, err)
	}
	return imports.ShouldBuild(cmts, g.tags)
}

func (g *gg) hashImport(hw io.Writer, path string) {
	d, ok := g.pkgLookup[path]
	if !ok {
		g.fatalf("failed to resolve import %v", path)
	}
	if !d.Done() {
		g.fatalf("inconsistent state: dependency %v is not done", path)
	}
	fmt.Fprintf(hw, "import %v: %x\n", d.ImportPath, d.hash)
}

func (g *gg) hashDeps(hw io.Writer, d dep) {
	var deps []dep
	for dd := range d.Deps().deps {
		if !dd.Done() {
			g.fatalf("consistency error: dep %v is not done", dd)
		}
		deps = append(deps, dd)
	}
	// order: *pkg, *gobinModDep, *gobinGlobalDep, *commandDep
	sortDeps(deps)
	for _, d := range deps {
		fmt.Fprintf(hw, "%v\n", d.HashString())
	}
}

func (g *gg) fatalf(format string, args ...interface{}) {
	panic(fmt.Errorf(format, args...))
}

func (g *gg) newPkg(p *golist.Package) *pkg {
	res := g.newPkgImpl(p)
	g.allDeps = append(g.allDeps, res)
	return res
}

func (g *gg) newPkgImpl(p *golist.Package) *pkg {
	if _, ok := g.pkgLookup[p.ImportPath]; ok {
		g.fatalf("tried to add pre-existing pkg %v", p.ImportPath)
	}
	if p2, ok := g.dirLookup[p.Dir]; ok {
		g.fatalf("directory overlap between packages %v and %v", p.ImportPath, p2.ImportPath)
	}
	res := &pkg{
		Package: p,
		depsMap: newDepsMap(),
	}
	g.pkgLookup[p.ImportPath] = res
	g.dirLookup[p.Dir] = res
	if p.ImportPath == "example.com/rename" {
		fmt.Printf("adding pkg dep example.com/rename\n")
	}
	return res
}

func (g *gg) resolveGobinModDep(patt string) *gobinModDep {
	if strings.Index(patt, "@") != -1 {
		g.fatalf("gobin -m directive cannot specify version: %v", patt)
	}
	// We still don't yet know that patt is a valid import path.
	// We assume it is and then check later when if we need to
	// resolve a missing package path.
	ip := patt
	if mod, ok := g.gobinModLookup[ip]; ok {
		return mod
	}
	d := &gobinModDep{
		importPath: ip,
		pkg:        g.pkgLookup[patt],
		depsMap:    newDepsMap(),
	}
	g.gobinModLookup[ip] = d
	g.allDeps = append(g.allDeps, d)
	return d
}

func (g *gg) resolveGobinGlobalDep(patt string) *gobinGlobalDep {
	if glob, ok := g.gobinGlobalCache[g.gobinGlobalLookup[patt]]; ok {
		return glob
	}
	// we need to use gobin -p to resolve patt
	cmd := exec.Command("gobin", "-p", patt)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if err, ok := err.(*exec.ExitError); ok {
			stderr = err.Stderr
		}
		g.fatalf("failed to resolve gobin global pattern via %v: %v\n%s", strings.Join(cmd.Args, " "), err, stderr)
	}

	target := strings.TrimSpace(string(out))

	if glob, ok := g.gobinGlobalCache[target]; ok {
		g.gobinGlobalLookup[patt] = target
		return glob
	}

	d := &gobinGlobalDep{
		targetPath: target,
		commandDep: &commandDep{
			name:    path.Base(target),
			depsMap: newDepsMap(),
		},
	}
	g.gobinGlobalLookup[patt] = target
	g.gobinGlobalCache[target] = d
	g.allDeps = append(g.allDeps, d)
	return d
}

func (g *gg) resolveCommandDep(name string) *commandDep {
	if comm, ok := g.commLookup[name]; ok {
		return comm
	}
	d := &commandDep{
		name:    name,
		depsMap: newDepsMap(),
	}
	g.commLookup[name] = d
	g.allDeps = append(g.allDeps, d)
	return d
}

var _ dep = (*pkg)(nil)

type dep interface {
	fmt.Stringer
	Deps() depsMap
	Ready() bool
	Done() bool
	Undo()
	HashString() string
}

type generator interface {
	dep
	DirectiveName() string
}

type gobinModDep struct {
	pkg        *pkg
	importPath string

	hash [hashSize]byte

	depsMap
}

func (g *gobinModDep) Deps() depsMap {
	return g.depsMap
}

func (g *gobinModDep) Ready() bool {
	return g.pkg != nil && len(g.dirtydeps) == 0
}

func (g *gobinModDep) Done() bool {
	return g.Ready() && g.hash != nilHash
}

func (g *gobinModDep) Undo() {
	g.hash = nilHash
}

func (g *gobinModDep) String() string {
	rslvd := "unresolved"
	if g.pkg != nil {
		rslvd = g.pkg.ImportPath
	}
	return fmt.Sprintf("gobinModDep: %v (%v)", g.importPath, rslvd)
}

func (g *gobinModDep) HashString() string {
	return fmt.Sprintf("gobinModDep: %v [%x]", g.importPath, g.pkg.hash)
}

func (g *gobinModDep) DirectiveName() string {
	return path.Base(g.importPath)
}

var _ generator = (*gobinModDep)(nil)

type gobinGlobalDep struct {
	*commandDep

	targetPath string
}

func (g *gobinGlobalDep) String() string {
	return fmt.Sprintf("gobinGlobalDep: %v", g.targetPath)
}

func (g *gobinGlobalDep) HashString() string {
	return fmt.Sprintf("gobinGlobalDep: %v [%x]", g.targetPath, g.hash)
}

var _ generator = (*gobinGlobalDep)(nil)

type commandDep struct {
	name string
	hash [hashSize]byte

	depsMap
}

func (c *commandDep) Deps() depsMap {
	return c.depsMap
}

func (c *commandDep) Ready() bool {
	return true
}

func (c *commandDep) Done() bool {
	return c.Ready() && c.hash != nilHash
}

func (c *commandDep) Undo() {
	c.hash = nilHash
}

func (c *commandDep) String() string {
	return fmt.Sprintf("commandDep: %v", c.name)
}

func (c *commandDep) HashString() string {
	return fmt.Sprintf("commandDep: %v [%x]", c.name, c.hash)
}

func (c *commandDep) DirectiveName() string {
	return c.name
}

var _ generator = (*commandDep)(nil)

func (g *gg) resolveDir(dir string, dirArgs []string) generator {
	switch dirArgs[0] {
	case "gobin":
		var gbargs struct {
			Args           []string
			Mode           string
			Patterns       []string
			MainModResolve bool
		}

		var stderr bytes.Buffer
		cmd := exec.Command("gobin", "-parseFlags")
		cmd.Args = append(cmd.Args, dirArgs[1:]...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			g.fatalf("failed to parse gobin args via [%v]: %v\n%s", strings.Join(cmd.Args, " "), err, stderr.Bytes())
		}
		if err := json.Unmarshal(out, &gbargs); err != nil {
			g.fatalf("failed to parse gobin parsed args output: %v\n%s", err, out)
		}
		if gbargs.Mode != "run" {
			g.fatalf("non-run gobin directive: [%v]", strings.Join(dirArgs, " "))
		}
		if len(gbargs.Patterns) != 1 {
			g.fatalf("gobin run directive had multiple patterns: [%v]", strings.Join(gbargs.Patterns, " "))
		}
		patt := gbargs.Patterns[0] // there will only be one
		if gbargs.MainModResolve {
			return g.resolveGobinModDep(patt)
		} else {
			return g.resolveGobinGlobalDep(patt)
		}
	case "go":
		g.fatalf("do not yet know how to handle go command-based directives")
	default:
		return g.resolveCommandDep(dirArgs[0])
	}

	panic("should not get here")
}

func parseOutDirs(dir string, args []string) ([]string, error) {
	outDirs := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		v := args[i]
		if v == "--" {
			break
		}
		if !strings.HasPrefix(v, "-"+gogenerate.FlagOutDirPrefix) {
			continue
		}
		v = strings.TrimPrefix(v, "-"+gogenerate.FlagOutDirPrefix)
		j := strings.Index(v, "=")
		var d string
		if j == -1 {
			if i+1 == len(args) || args[i+1] == "--" {
				return nil, fmt.Errorf("invalid output dir flag amongst: %v", args)
			}
			d = args[i+1]
		} else {
			d = v[j+1:]
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		ed, err := filepath.EvalSymlinks(d)
		if err != nil {
			return nil, fmt.Errorf("failed to eval symlinks for dir %v: %v", d, err)
		}
		outDirs[ed] = true
	}
	var dirs []string
	for d := range outDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func sortDeps(deps []dep) {
	sort.Slice(deps, func(i, j int) bool {
		lhs, rhs := deps[i], deps[j]
		switch lhs := lhs.(type) {
		case *pkg:
			switch rhs := rhs.(type) {
			case *pkg:
				return lhs.importPath < rhs.importPath
			case *gobinModDep, *gobinGlobalDep, *commandDep:
				return true
			default:
				panic(fmt.Errorf("could not compare %T with %T", lhs, rhs))
			}
		case *gobinModDep:
			switch rhs := rhs.(type) {
			case *pkg:
				return false
			case *gobinModDep:
				return lhs.importPath < rhs.importPath
			case *gobinGlobalDep, *commandDep:
				return true
			default:
				panic(fmt.Errorf("could not compare %T with %T", lhs, rhs))
			}
		case *gobinGlobalDep:
			switch rhs := rhs.(type) {
			case *pkg, *gobinModDep:
				return false
			case *gobinGlobalDep:
				return lhs.targetPath < rhs.targetPath
			case *commandDep:
				return true
			default:
				panic(fmt.Errorf("could not compare %T with %T", lhs, rhs))
			}
		case *commandDep:
			switch rhs := rhs.(type) {
			case *pkg, *gobinModDep, *gobinGlobalDep:
				return false
			case *commandDep:
				return lhs.name < rhs.name
			default:
				panic(fmt.Errorf("could not compare %T with %T", lhs, rhs))
			}
		}
		panic("should not get here")
	})
}

type missingDeps map[string]map[*pkg]bool

func (m missingDeps) add(ip string, p *pkg) {
	deps, ok := m[ip]
	if !ok {
		deps = make(map[*pkg]bool)
		m[ip] = deps
	}
	deps[p] = true
}

const (
	archiveDelim   = byte('|')
	archiveVersion = byte('1')
)

type archiveReader struct {
	file        *os.File
	und         *bufio.Reader
	readVersion bool
}

func newArchiveReader(path string) (*archiveReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open %v: %v", path, err)
	}
	return &archiveReader{
		file: f,
		und:  bufio.NewReader(f),
	}, nil
}

func (r *archiveReader) ExtractFile() (string, error) {
	if !r.readVersion {
		b, err := r.und.ReadByte()
		if err == nil {
			if b != archiveVersion {
				err = fmt.Errorf("read version %v; expected %v", string(b), string(archiveVersion))
			}
			r.readVersion = true
		}
		if err != nil {
			return "", err
		}
	}
	fn, err := r.und.ReadString(archiveDelim)
	if err != nil {
		if err == io.EOF {
			return "", err
		}
		return "", fmt.Errorf("could not read filename: %v", err)
	}
	fn = fn[:len(fn)-1]
	if len(fn) == 0 {
		return "", fmt.Errorf("invalid zero-length filename")
	}
	ls, err := r.und.ReadString(archiveDelim)
	if err != nil {
		return "", fmt.Errorf("failed to read length string: %v", err)
	}
	ls = ls[:len(ls)-1]
	l, err := strconv.ParseInt(ls, 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to read length of file: %v", err)
	}
	f, err := os.Create(fn)
	if err != nil {
		return "", fmt.Errorf("failed to create %v: %v", fn, err)
	}
	lr := io.LimitReader(r.und, l)
	if n, err := io.Copy(f, lr); err != nil || n != l {
		return "", fmt.Errorf("failed to extract %v bytes (read %v) to %v: %v", l, n, fn, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("failed to close %v: %v", fn, err)
	}
	return fn, nil
}

func (r *archiveReader) Close() error {
	return r.file.Close()
}

type archiveWriter struct {
	file           *os.File
	und            *bufio.Writer
	writtenVersion bool
}

func newArchiveWriter(dir string, pattern string) (*archiveWriter, error) {
	tf, err := ioutil.TempFile(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary file: %v", err)
	}
	return &archiveWriter{
		file: tf,
		und:  bufio.NewWriter(tf),
	}, nil
}

func (w *archiveWriter) PutFile(path string) error {
	if !w.writtenVersion {
		if err := w.und.WriteByte(archiveVersion); err != nil {
			return fmt.Errorf("failed to write archive version: %v", err)
		}
		w.writtenVersion = true
	}
	if _, err := w.und.WriteString(path + string(archiveDelim)); err != nil {
		return fmt.Errorf("failed to write file path %v: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat %v: %v", path, err)
	}
	if _, err := w.und.WriteString(fmt.Sprintf("%v%v", fi.Size(), string(archiveDelim))); err != nil {
		return fmt.Errorf("failed to write file length")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open %v for reading: %v", path, err)
	}
	n, err := io.Copy(w.und, f)
	f.Close()
	if err != nil || n != fi.Size() {
		return fmt.Errorf("failed to write %v to archive: wrote %v (expected %v): %v", path, n, fi.Size(), err)
	}
	return nil
}

func (w *archiveWriter) Close() error {
	if err := w.und.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer: %v", err)
	}
	return w.file.Close()
}
