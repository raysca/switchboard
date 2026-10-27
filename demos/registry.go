// Package demos is the registry of installable demo definitions. Every demo
// package imports demos/spec only; this registry is the one place that imports
// the demo packages, so the seed client can look a demo up by name.
package demos

import (
	"errors"
	"fmt"
	"strings"

	"govoice/demos/atlas"
	"govoice/demos/dental"
	"govoice/demos/spec"
)

// ErrUnknownDemo is returned by Get for a name that is not registered.
var ErrUnknownDemo = errors.New("unknown demo")

// all is the registry in stable order: the dental demo first, then Atlas.
var all = []spec.Demo{dental.Demo{}, atlas.Demo{}}

// Get returns the demo registered under name.
func Get(name string) (spec.Demo, error) {
	for _, d := range all {
		if d.Name() == name {
			return d, nil
		}
	}
	known := make([]string, 0, len(all))
	for _, d := range all {
		known = append(known, d.Name())
	}
	return nil, fmt.Errorf("%w %q (known demos: %s)", ErrUnknownDemo, name, strings.Join(known, ", "))
}
