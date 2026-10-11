package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
)

// LoadWithOverlay loads the packaged catalog, then merges every *.yaml file in
// dir (when set). A bad overlay file is logged through logf and skipped; a
// missing or unreadable dir is an error.
func LoadWithOverlay(dir string, logf func(format string, args ...any)) (*Catalog, error) {
	c, err := Load()
	if err != nil || dir == "" {
		return c, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("catalog overlay dir: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".yaml" {
			continue
		}
		if err := c.mergeFile(filepath.Join(dir, f.Name())); err != nil {
			logf("catalog overlay file skipped: %v", err)
		}
	}
	return c, nil
}

func (c *Catalog) mergeFile(path string) error {
	// Checked before opening: opening a FIFO for reading blocks start-up.
	if st, err := os.Stat(path); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	buf := make([]byte, MaxFileBytes+1)
	n, _ := fh.Read(buf) // ponytail: one Read; a short read just fails the parse below
	o, err := parse(path, buf[:n], true)
	if err != nil {
		return err
	}
	if err := c.merge(o); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (c *Catalog) merge(o *Provider) error {
	base, ok := c.byID[o.ID]
	if !ok {
		return fmt.Errorf("provider %q has no packaged definition (a new provider needs an adapter in the build)", o.ID)
	}
	if o.Name != "" && o.Name != base.Name {
		return errors.New("an overlay cannot change the provider name")
	}
	if len(o.Credentials) != 0 && !reflect.DeepEqual(o.Credentials, base.Credentials) {
		return errors.New("an overlay cannot change credentials or assemble")
	}
	next := *base
	next.Services = map[string]*Service{}
	for n, s := range base.Services {
		cp := *s
		cp.Models, cp.Voices, cp.Languages = slices.Clone(s.Models), slices.Clone(s.Voices), slices.Clone(s.Languages)
		next.Services[n] = &cp
	}
	for n, os := range o.Services {
		bs, ok := next.Services[n]
		if !ok {
			return fmt.Errorf("service %q is not declared by the packaged %s", n, o.ID)
		}
		if len(os.Credential) != 0 && !slices.Equal(os.Credential, bs.Credential) || len(os.Needs) != 0 && !slices.Equal(os.Needs, bs.Needs) {
			return errors.New("an overlay cannot change a service's credential or needs")
		}
		bs.Models, bs.Voices, bs.Languages = mergeEntries(bs.Models, os.Models), mergeEntries(bs.Voices, os.Voices), mergeEntries(bs.Languages, os.Languages)
		if os.Custom != nil {
			bs.Custom = os.Custom
		}
	}
	if err := next.validate(false); err != nil {
		return err
	}
	c.byID[o.ID] = &next
	return nil
}

// mergeEntries replaces entries by id and appends new ones; an overlay default
// takes over from the packaged one.
func mergeEntries(base, over []Entry) []Entry {
	if slices.ContainsFunc(over, func(e Entry) bool { return e.Default }) {
		for i := range base {
			base[i].Default = false
		}
	}
	for _, e := range over {
		if i := slices.IndexFunc(base, func(b Entry) bool { return b.ID == e.ID }); i >= 0 {
			e.Default = e.Default || base[i].Default
			base[i] = e
		} else {
			base = append(base, e)
		}
	}
	return base
}
