package engine

import "math/rand/v2"

// jitter returns a value in [0,1). It is a variable so tests can pin it.
var jitter = func() float64 { return rand.Float64() }
