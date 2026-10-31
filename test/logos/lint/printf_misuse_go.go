// errorcheck

// correctness::printf_misuse on a Go package: validates format string verbs
// against argument count and operand types for formatted I/O calls.
// In Go packages, forbid lowers to deny, so //logos:allow can silence it.

package p

import (
	"fmt"
	"os"
)

func badArgCount() {
	fmt.Printf("user %s count %d\n", "alice") // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
	fmt.Sprintf("user %s count %d\n")         // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]" "\[nogo::unusedresult\]"
	fmt.Fprintf(os.Stdout, "%s %s\n", "one")  // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
	fmt.Printf("count %d\n", 1, 2)            // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
}

func badType() {
	fmt.Printf("%d\n", "not an int") // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
	fmt.Printf("%s\n", 42)           // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
	fmt.Printf("%t\n", 123)          // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
}

func badIndex() {
	fmt.Printf("%[0]d\n", 1) // ERROR "\[correctness::printf_misuse\]" "\[nogo::printf\]"
}

func cleanForms(dynamic string) {
	fmt.Printf("user %s count %d\n", "alice", 42)
	fmt.Sprintf("val: %v, str: %s, int: %d\n", true, "ok", 10) // ERROR "\[nogo::unusedresult\]"
	fmt.Fprintf(os.Stdout, "name: %s\n", "bob")
	fmt.Printf("%v %+v %#v %T\n", "s", 1, true, struct{}{})
	fmt.Printf(dynamic, 1, 2, 3)
}

//logos:allow(correctness::printf_misuse)
//logos:allow(nogo::printf)
func allowedInGo() {
	fmt.Printf("count %d\n", 1, 2)
}
