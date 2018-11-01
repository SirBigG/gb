package gb

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SirBigG/gb/internal/fileutils"
	"github.com/SirBigG/gb/internal/version"
)

// Build builds each of pkgs in succession. If pkg is a command, then the results of build include
// linking the final binary into pkg.Context.Bindir().
func Build(pkgs ...*Package) error {
	build, err := BuildPackages(pkgs...)
	if err != nil {
		return err
	}
	return ExecuteConcurrent(build, runtime.NumCPU(), nil)
}

// BuildPackages produces a tree of *Actions that can be executed to build
// a *Package.
// BuildPackages walks the tree of *Packages and returns a corresponding
// tree of *Actions representing the steps required to build *Package
// and any of its dependencies
func BuildPackages(pkgs ...*Package) (*Action, error) {
	if len(pkgs) < 1 {
		return nil, errors.New("no packages supplied")
	}

	targets := make(map[string]*Action) // maps package importpath to build action

	names := func(pkgs []*Package) []string {
		var names []string
		for _, pkg := range pkgs {
			names = append(names, pkg.ImportPath)
		}
		return names
	}

	// create top level build action to unify all packages
	t0 := time.Now()
	build := Action{
		Name: fmt.Sprintf("build: %s", strings.Join(names(pkgs), ",")),
		Run: func() error {
			pkgs[0].debug("build duration: %v %v", time.Since(t0), pkgs[0].Statistics.String())
			return nil
		},
	}

	for _, pkg := range pkgs {
		if len(pkg.GoFiles)+len(pkg.CgoFiles) == 0 {
			pkg.debug("skipping %v: no go files", pkg.ImportPath)
			continue
		}
		a, err := BuildPackage(targets, pkg)
		if err != nil {
			return nil, err
		}
		if a == nil {
			// nothing to do
			continue
		}
		build.Deps = append(build.Deps, a)
	}
	return &build, nil
}

// BuildPackage returns an Action representing the steps required to
// build this package.
func BuildPackage(targets map[string]*Action, pkg *Package) (*Action, error) {

	// if this action is already present in the map, return it
	// rather than creating a new action.
	if a, ok := targets[pkg.ImportPath]; ok {
		return a, nil
	}

	// step 0. are we stale ?
	// if this package is not stale, then by definition none of its
	// dependencies are stale, so ignore this whole tree.
	if pkg.NotStale {
		return nil, nil
	}

	// step 1. build dependencies
	deps, err := BuildDependencies(targets, pkg)
	if err != nil {
		return nil, err
	}

	// step 2. build this package
	build, err := Compile(pkg, deps...)
	if err != nil {
		return nil, err
	}

	if build == nil {
		panic("build action was nil") // shouldn't happen
	}

	// record the final action as the action that represents
	// building this package.
	targets[pkg.ImportPath] = build
	return build, nil
}

// Compile returns an Action representing the steps required to compile this package.
func Compile(pkg *Package, deps ...*Action) (*Action, error) {
	var gofiles []string
	gofiles = append(gofiles, pkg.GoFiles...)

	// step 1. are there any .c files that we have to run cgo on ?
	var ofiles []string // additional ofiles to pack
	if len(pkg.CgoFiles) > 0 {
		cgoACTION, cgoOFILES, cgoGOFILES, err := cgo(pkg)
		if err != nil {
			return nil, err
		}

		gofiles = append(gofiles, cgoGOFILES...)
		ofiles = append(ofiles, cgoOFILES...)
		deps = append(deps, cgoACTION)
	}

	if len(gofiles) == 0 {
		return nil, fmt.Errorf("compile %q: no go files supplied", pkg.ImportPath)
	}

	// step 2. compile all the go files for this package, including pkg.CgoFiles
	compile := Action{
		Name: fmt.Sprintf("compile: %s", pkg.ImportPath),
		Deps: deps,
		Run:  func() error { return gc(pkg, gofiles) },
	}

	// step 3. are there any .s files to assemble.
	var assemble []*Action
	for _, sfile := range pkg.SFiles {
		sfile := sfile
		ofile := filepath.Join(pkg.Context.Workdir(), pkg.ImportPath, stripext(sfile)+".6")
		assemble = append(assemble, &Action{
			Name: fmt.Sprintf("asm: %s/%s", pkg.ImportPath, sfile),
			Run: func() error {
				t0 := time.Now()
				err := pkg.tc.Asm(pkg, ofile, filepath.Join(pkg.Dir, sfile))
				pkg.Record("asm", time.Since(t0))
				return err
			},
			// asm depends on compile because compile will generate the local go_asm.h
			Deps: []*Action{&compile},
		})
		ofiles = append(ofiles, ofile)
	}

	// step 4. add system object files.
	for _, syso := range pkg.SysoFiles {
		ofiles = append(ofiles, filepath.Join(pkg.Dir, syso))
	}

	build := &compile

	// Do we need to pack ? Yes, replace build action with pack.
	if len(ofiles) > 0 {
		pack := Action{
			Name: fmt.Sprintf("pack: %s", pkg.ImportPath),
			Deps: []*Action{
				&compile,
			},
			Run: func() error {
				// collect .o files, ofiles always starts with the gc compiled object.
				// TODO(dfc) objfile(pkg) should already be at the top of this set
				ofiles = append(
					[]string{pkg.objfile()},
					ofiles...,
				)

				// pack
				t0 := time.Now()
				err := pkg.tc.Pack(pkg, ofiles...)
				pkg.Record("pack", time.Since(t0))
				return err
			},
		}
		pack.Deps = append(pack.Deps, assemble...)
		build = &pack
	}

	// should this package be cached
	if pkg.Install && !pkg.TestScope {
		build = &Action{
			Name: fmt.Sprintf("install: %s", pkg.ImportPath),
			Deps: []*Action{build},
			Run:  func() error { return fileutils.Copyfile(pkg.installpath(), pkg.objfile()) },
		}
	}

	// if this is a main package, add a link stage
	if pkg.Main {
		build = &Action{
			Name: fmt.Sprintf("link: %s", pkg.ImportPath),
			Deps: []*Action{build},
			Run:  func() error { return pkg.link() },
		}
	}
	if !pkg.TestScope {
		// if this package is not compiled in test scope, then
		// log the name of the package when complete.
		build.Run = logInfoFn(build.Run, pkg.ImportPath)
	}
	return build, nil
}

func logInfoFn(fn func() error, s string) func() error {
	return func() error {
		err := fn()
		fmt.Println(s)
		return err
	}
}

// BuildDependencies returns a slice of Actions representing the steps required
// to build all dependant packages of this package.
func BuildDependencies(targets map[string]*Action, pkg *Package) ([]*Action, error) {
	var deps []*Action
	pkgs := pkg.Imports

	var extra []string
	switch {
	case pkg.Main:
		// all binaries depend on runtime, even if they do not
		// explicitly import it.
		extra = append(extra, "runtime")
		if pkg.race {
			// race binaries have extra implicit depdendenceis.
			extra = append(extra, "runtime/race")
		}
	case len(pkg.CgoFiles) > 0 && pkg.ImportPath != "runtime/cgo":
		// anything that uses cgo has a dependency on runtime/cgo which is
		// only visible after cgo file generation.
		// TODO(dfc) what about pkg.Main && pkg.CgoFiles > 0 ??
		extra = append(extra, "runtime/cgo")
	}
	if pkg.TestScope {
		extra = append(extra, "regexp")
		if version.Version > 1.7 {
			// since Go 1.8 tests have additional implicit dependencies
			extra = append(extra, "testing/internal/testdeps")
		}
	}
	for _, i := range extra {
		p, err := pkg.ResolvePackage(i)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	for _, i := range pkgs {
		a, err := BuildPackage(targets, i)
		if err != nil {
			return nil, err
		}
		if a == nil {
			// no action required for this Package
			continue
		}
		deps = append(deps, a)
	}
	return deps, nil
}

func gc(pkg *Package, gofiles []string) error {
	t0 := time.Now()
	err := pkg.tc.Gc(pkg, gofiles)
	pkg.Record("gc", time.Since(t0))
	return err
}

func (pkg *Package) link() error {
	t0 := time.Now()
	err := pkg.tc.Ld(pkg)
	pkg.Record("link", time.Since(t0))
	return err
}
