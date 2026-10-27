package demos_test

import (
	"errors"
	"strings"
	"testing"

	"govoice/demos"
)

func TestGetReturnsBothRegisteredDemos(t *testing.T) {
	for _, want := range []string{"dental", "atlas"} {
		got, err := demos.Get(want)
		if err != nil {
			t.Fatalf("Get(%q): %v", want, err)
		}
		if got.Name() != want {
			t.Errorf("Get(%q).Name() = %q", want, got.Name())
		}
	}
}

func TestGetNamesTheUnknownDemo(t *testing.T) {
	d, err := demos.Get("nope")
	if d != nil {
		t.Fatalf("Get(\"nope\") = %T, want nil", d)
	}
	if !errors.Is(err, demos.ErrUnknownDemo) {
		t.Fatalf("Get(\"nope\") error = %v, want ErrUnknownDemo", err)
	}
	for _, want := range []string{`"nope"`, "dental", "atlas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
