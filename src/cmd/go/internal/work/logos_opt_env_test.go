// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"os"
	"slices"
	"testing"

	"cmd/go/internal/cfg"
)

func resetArchEnvState(t *testing.T, goarch, initAMD64, initARM64 string) {
	t.Helper()
	oldLogos, oldGoarch := cfg.Logos, cfg.Goarch
	oldAMD64, oldARM64 := cfg.GOAMD64, cfg.GOARM64
	oldInitAMD64, oldInitARM64 := logosInitGOAMD64, logosInitGOARM64
	oldTargetArchEnv := logosTargetArchEnv
	oldCfgChangedEnv := cfgChangedEnv
	oldEnvAMD64, hasAMD64 := os.LookupEnv("GOAMD64")
	oldEnvARM64, hasARM64 := os.LookupEnv("GOARM64")
	t.Cleanup(func() {
		cfg.Logos, cfg.Goarch = oldLogos, oldGoarch
		cfg.GOAMD64, cfg.GOARM64 = oldAMD64, oldARM64
		logosInitGOAMD64, logosInitGOARM64 = oldInitAMD64, oldInitARM64
		logosTargetArchEnv = oldTargetArchEnv
		cfgChangedEnv = oldCfgChangedEnv
		if hasAMD64 {
			os.Setenv("GOAMD64", oldEnvAMD64)
		} else {
			os.Unsetenv("GOAMD64")
		}
		if hasARM64 {
			os.Setenv("GOARM64", oldEnvARM64)
		} else {
			os.Unsetenv("GOARM64")
		}
	})
	cfg.Goarch = goarch
	logosInitGOAMD64, logosInitGOARM64 = initAMD64, initARM64
	logosTargetArchEnv = nil
	cfgChangedEnv = nil
}

func withArchEnvState(t *testing.T, goarch, initAMD64, initARM64 string) {
	t.Helper()
	resetArchEnvState(t, goarch, initAMD64, initARM64)
	cfg.Logos = true
}

func withPlainArchEnvState(t *testing.T, goarch, initAMD64, initARM64 string) {
	t.Helper()
	resetArchEnvState(t, goarch, initAMD64, initARM64)
	cfg.Logos = false
}

func TestLogosTargetArchEnvPinned(t *testing.T) {
	withArchEnvState(t, "amd64", "", "")
	logosSetArchLevel("GOAMD64", "v3")
	logosSetArchLevel("GOARM64", "v8.2")
	got := LogosTargetArchEnv()
	want := []string{"GOAMD64=v3"}
	if !slices.Equal(got, want) {
		t.Fatalf("LogosTargetArchEnv() = %v, want %v", got, want)
	}

	withArchEnvState(t, "arm64", "", "")
	logosSetArchLevel("GOAMD64", "v3")
	logosSetArchLevel("GOARM64", "v8.2")
	got = LogosTargetArchEnv()
	want = []string{"GOARM64=v8.2"}
	if !slices.Equal(got, want) {
		t.Fatalf("LogosTargetArchEnv() = %v, want %v", got, want)
	}
}

func TestLogosTargetArchEnvUserSet(t *testing.T) {
	withArchEnvState(t, "amd64", "v1", "")
	logosSetArchLevel("GOAMD64", "v3")
	logosSetArchLevel("GOARM64", "v8.2")
	if got := LogosTargetArchEnv(); len(got) != 0 {
		t.Fatalf("LogosTargetArchEnv() = %v, want empty when user set level", got)
	}

	withArchEnvState(t, "arm64", "", "v8.0")
	logosSetArchLevel("GOAMD64", "v3")
	logosSetArchLevel("GOARM64", "v8.2")
	if got := LogosTargetArchEnv(); len(got) != 0 {
		t.Fatalf("LogosTargetArchEnv() = %v, want empty when user set level", got)
	}
}

func TestLogosTargetArchEnvPlainMode(t *testing.T) {
	withPlainArchEnvState(t, "amd64", "", "")
	logosSetArchLevel("GOAMD64", "v3")
	logosSetArchLevel("GOARM64", "v8.2")
	if got := LogosTargetArchEnv(); len(got) != 0 {
		t.Fatalf("LogosTargetArchEnv() = %v in plain mode, want empty", got)
	}
}
