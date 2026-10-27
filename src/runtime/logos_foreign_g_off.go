// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.logosgcmoving

package runtime

// LogosForeignGBytes is the size logosForeignG adds to g under the
// moving experiment; zero off-experiment where the struct is empty.
const LogosForeignGBytes = 0

// logosForeignG is empty off-experiment: no per-g foreign-call frame
// state exists where no frame can be admitted. It is embedded in g ahead
// of a non-zero-sized field, so the empty shape costs nothing and g
// returns to its pre-foreign floor. Off-experiment stubs must not touch
// the promoted fields (they do not exist here); see
// zgcForeignTransportEnter/Exit in logos_zgc_boundary_off.go.
type logosForeignG struct{}
