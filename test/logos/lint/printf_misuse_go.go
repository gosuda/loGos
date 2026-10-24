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
	fmt.Printf("user %s count %d\n", "alice") // ERROR "\[correctness::printf_misuse\]"
	fmt.Sprintf("user %s count %d\n")         // ERROR "\[correctness::printf_misuse\]"
	fmt.Fprintf(os.Stdout, "%s %s\n", "one")  // ERROR "\[correctness::printf_misuse\]"
	fmt.Printf("count %d\n", 1, 2)            // ERROR "\[correctness::printf_misuse\]"
}

func badType() {
	fmt.Printf("%d\n", "not an int") // ERROR "\[correctness::printf_misuse\]"
	fmt.Printf("%s\n", 42)           // ERROR "\[correctness::printf_misuse\]"
	fmt.Printf("%t\n", 123)          // ERROR "\[correctness::printf_misuse\]"
}

func badIndex() {
	fmt.Printf("%[0]d\n", 1) // ERROR "\[correctness::printf_misuse\]"
}

func cleanForms(dynamic string) {
	fmt.Printf("user %s count %d\n", "alice", 42)
	fmt.Sprintf("val: %v, str: %s, int: %d\n", true, "ok", 10)
	fmt.Fprintf(os.Stdout, "name: %s\n", "bob")
	fmt.Printf("%v %+v %#v %T\n", "s", 1, true, struct{}{})
	fmt.Printf(dynamic, 1, 2, 3)
}

//logos:allow(correctness::printf_misuse)
func allowedInGo() {
	fmt.Printf("count %d\n", 1, 2)
}
