// run -gc=greentea

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// M19-S2 (SPEC Sec 19, T-301.2): go build -gc=greentea of a Go program runs
// the elastic pacing, with the same Go source that gc_pacing_go.go builds with
// no -gc. The program re-executes itself as "child" with
// GODEBUG=gcpacertrace=1; the child starts two collections by hand, and a
// cycle start under elastic pacing prints a "pacer: elastic" line beside the
// upstream "pacer: assist ratio" line.

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		runtime.GC()
		runtime.GC()
		return
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Printf("FAIL self: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(self, "child")
	cmd.Env = append(os.Environ(), "GODEBUG=gcpacertrace=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("FAIL child: %v\n%s", err, out)
		os.Exit(1)
	}
	trace := string(out)
	fmt.Println("elastic pacing:", strings.Contains(trace, "pacer: elastic u="))
	fmt.Println("assist ratio traced:", strings.Contains(trace, "pacer: assist ratio="))
}
