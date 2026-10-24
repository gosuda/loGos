// errorcheck

// correctness::structtag_malformed on a Go package: validates struct field tag
// literals against reflection conventions defined by reflect.StructTag.
// In Go packages, forbid lowers to deny, so //logos:allow can silence it.

package p

type BadTags struct {
	Name   string `json:name,omitempty`      // ERROR "\[correctness::structtag_malformed\]"
	Age    int32  `json:"age" json:"years"`  // ERROR "\[correctness::structtag_malformed\]"
	secret string `json:"secret"`            // ERROR "\[correctness::structtag_malformed\]"
}

type CleanTags struct {
	Name    string `json:"name,omitempty" xml:"name"`
	ignored string `json:"-"`
}

//logos:allow(correctness::structtag_malformed)
type Allowed struct {
	Field string `json:bad`
}
