// errorcheck -0

// suspicious::string_intconv on a Go package (LINT-RULES.md Sec 1.2): detects
// conversions of the form string(x) where x has an integer type other than
// rune or byte.

package p

import "strconv"

func badConversions(port int, id int64) {
	_ = string(port) // ERROR "\[suspicious::string_intconv\]"
	_ = string(1234) // ERROR "\[suspicious::string_intconv\]"
	_ = string(id)   // ERROR "\[suspicious::string_intconv\]"
}

func formatDecimal(x int) string {
	return strconv.Itoa(x)
}

func cleanConversions(port int, r rune, b byte, bs []byte) {
	_ = strconv.Itoa(port)
	_ = string(r)
	_ = string('A')
	_ = string(b)
	_ = string(rune(port))
	_ = string(bs)
	_ = formatDecimal(port)
}

//logos:allow(suspicious::string_intconv)
func allowedInGo(port int) string {
	return string(port)
}
