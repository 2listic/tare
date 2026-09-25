package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is stamped by the linker for a built release:
//
//	go build -ldflags "-X main.version=v0.3.1" ./cmd/kubectl-tare
//
// Left empty, it is recovered from the build info the Go toolchain embeds, so
// a binary from `go install` reports its module version and one built from a
// working tree reports its commit and whether that tree was dirty.
//
// It exists because a recommendation is an argument, and an argument has to be
// attributable to the code that made it. Binaries get passed around by hand;
// when somebody says the number was wrong, the first question is which build
// produced it, and "I think I built it in September" is not an answer.
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, dirty, when string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		case "vcs.time":
			when = s.Value
		}
	}
	switch {
	case rev != "":
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if when != "" {
			return rev + dirty + " (" + when + ")"
		}
		return rev + dirty
	case info.Main.Version != "" && info.Main.Version != "(devel)":
		return info.Main.Version
	}
	return "devel"
}

func printVersion() {
	fmt.Printf("kubectl-tare %s\n%s %s/%s\n",
		buildVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
