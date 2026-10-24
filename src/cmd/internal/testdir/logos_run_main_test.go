// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testdir_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scenarioDefinition struct {
	goFile  string
	wantOut string
	src     string
}

var scenarioTable = map[string]scenarioDefinition{
	"mismatch": {
		goFile:  "run/regress_main_marker_test.lgo",
		wantOut: "regress-main-marker-expected\n",
		src:     "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"regress-main-marker-actual\")\n}\n",
	},
	"panic": {
		goFile:  "run/regress_main_panic_test.lgo",
		wantOut: "",
		src:     "package main\n\nfunc main() {\n\tpanic(\"regress-main-panic-marker\")\n}\n",
	},
	"conventional_pass": {
		goFile:  "run/regress_testx_pass_test.lgo",
		wantOut: "",
		src:     "package main\n\nimport \"testing\"\n\nfunc TestRegressPass(t *testing.T) {\n\tt.Log(\"decoy func main text; TestX runs\")\n}\n",
	},
	"conventional_fail": {
		goFile:  "run/regress_testx_fail_test.lgo",
		wantOut: "",
		src:     "package main\n\nimport \"testing\"\n\nfunc TestRegressFail(t *testing.T) {\n\tt.Fatal(\"regress-conventional-fail-marker\")\n}\n",
	},
}

type scenarioOutcome struct {
	success bool
	errMsg  string
}

func runScenario(t *testing.T, name string) scenarioOutcome {
	t.Helper()
	resPath := filepath.Join(t.TempDir(), "outcome.txt")
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestLogosRunScenarioChild$")
	cmd.Env = append(cmd.Environ(),
		"LOGOS_TESTDIR_CHILD=1",
		"LOGOS_TESTDIR_SCENARIO="+name,
		"LOGOS_TESTDIR_RESULT="+resPath,
	)
	cmdOut, err := cmd.CombinedOutput()
	resultPayload, readErr := os.ReadFile(resPath)
	if readErr != nil {
		t.Fatalf("scenario child process did not record outcome (exec error: %v):\n%s", err, string(cmdOut))
	}
	text := string(resultPayload)
	if text == "PASS" {
		return scenarioOutcome{success: true}
	}
	if msg, ok := strings.CutPrefix(text, "FAIL:\n"); ok {
		return scenarioOutcome{success: false, errMsg: msg}
	}
	t.Fatalf("unrecognized scenario child outcome: %q\nraw output:\n%s", text, string(cmdOut))
	return scenarioOutcome{}
}

func TestLogosRunScenarioChild(t *testing.T) {
	if os.Getenv("LOGOS_TESTDIR_CHILD") != "1" {
		return
	}
	name := os.Getenv("LOGOS_TESTDIR_SCENARIO")
	resPath := os.Getenv("LOGOS_TESTDIR_RESULT")
	scenarioSpec, ok := scenarioTable[name]
	if !ok {
		t.Fatalf("unknown scenario %q", name)
	}

	if goTool == "" {
		goTool = testenv.GoToolPath(t)
	}
	if tmpDir == "" {
		tmpDir = t.TempDir()
	}

	root := t.TempDir()
	outPath := filepath.Join(root, "logos", testOutputStem(scenarioSpec.goFile)+".out")
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte(scenarioSpec.wantOut), 0644); err != nil {
		t.Fatal(err)
	}

	ts := test{
		testCommon: testCommon{gorootTestDir: root},
		dir:        "logos",
		goFile:     scenarioSpec.goFile,
		T:          t,
	}
	err := ts.runLogos(logosCase{
		action: "run",
		src:    []byte(scenarioSpec.src),
		tim:    120,
	})

	var resultPayload []byte
	if err == nil {
		resultPayload = []byte("PASS")
	} else {
		resultPayload = []byte("FAIL:\n" + err.Error())
	}
	if wErr := os.WriteFile(resPath, resultPayload, 0644); wErr != nil {
		t.Fatal(wErr)
	}
}

func TestLogosRunMainTestLgoOutputChecked(t *testing.T) {
	outcome := runScenario(t, "mismatch")
	if outcome.success {
		t.Fatal("runLogos on main-program *_test.lgo with mismatched output returned success")
	}
	if !strings.Contains(outcome.errMsg, "output does not match") || !strings.Contains(outcome.errMsg, "regress-main-marker-actual") {
		t.Fatalf("runLogos error does not report output mismatch marker: %s", outcome.errMsg)
	}
}

func TestLogosRunMainTestLgoPanicFails(t *testing.T) {
	outcome := runScenario(t, "panic")
	if outcome.success {
		t.Fatal("runLogos on panicking main-program *_test.lgo returned success")
	}
	if !strings.Contains(outcome.errMsg, "regress-main-panic-marker") {
		t.Fatalf("runLogos error does not report panic marker: %s", outcome.errMsg)
	}
}

func TestLogosRunNonMainTestLgoUsesTesting(t *testing.T) {
	passOutcome := runScenario(t, "conventional_pass")
	if !passOutcome.success {
		t.Fatalf("runLogos on conventional passing *_test.lgo failed: %s", passOutcome.errMsg)
	}

	failOutcome := runScenario(t, "conventional_fail")
	if failOutcome.success {
		t.Fatal("runLogos on conventional failing *_test.lgo returned success")
	}
	if !strings.Contains(failOutcome.errMsg, "regress-conventional-fail-marker") {
		t.Fatalf("runLogos error does not report failing TestX marker: %s", failOutcome.errMsg)
	}
}
