// +build mage

package main

import (
	"os"
	"path"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

const (
	packageName = "github.com/tspivey/books/cmd/books"
	ldflags     = "-X " + packageName + "/commands.Version=$VERSION"
	outDir      = "bin"
)

var Default = Build
var vars map[string]string

// allow user to override go executable by running as GOEXE=xxx make ... on unix-like systems
var goexe = "go"

func init() {
	if exe := os.Getenv("GOEXE"); exe != "" {
		goexe = exe
	}

	// We want to use Go 1.11 modules even if the source lives inside GOPATH.
	// The default is "auto".
	os.Setenv("GO111MODULE", "on")
}

// Build builds Books.
func Build() error {
	mg.Deps(mkBin)
	return sh.RunWith(getVars(), goexe, "build", "-ldflags", ldflags, "-o", path.Join(outDir, "$BIN_NAME"), packageName)
}

// Install installs Books.
func Install() error {
	return sh.RunWith(getVars(), goexe, "install", "-ldflags", ldflags, packageName)
}

// Clean removes all files and directories created by mage targets.
func Clean() error {
	return os.RemoveAll(outDir)
}

func mkBin() error {
	if _, err := os.Stat(outDir); err == nil {
		return nil
	}
	return os.Mkdir(outDir, 0755)
}

func getVars() map[string]string {
	if vars != nil {
		return vars
	}

	vars = make(map[string]string)
	version, err := sh.Output("git", "describe", "--always", "--long", "--dirty")
	if err != nil {
		version = "unset"
	}
	vars["VERSION"] = version

	vars["BIN_NAME"] = "books"
	if os.Getenv("GOOS") == "windows" {
		vars["BIN_NAME"] += ".exe"
	}

	return vars
}
