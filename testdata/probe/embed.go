// Package probe embeds the public synthetic capability-probe fixtures.
package probe

import _ "embed"

// PDF is the synthetic capability-probe PDF.
//
//go:embed capability.pdf
var PDF []byte

// PNG is the synthetic capability-probe PNG.
//
//go:embed capability.png
var PNG []byte
