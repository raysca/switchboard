package record

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Sink stores finished recordings. Implementations must be safe for concurrent
// Create calls. An S3-compatible sink implements this interface by staging the
// stream (multipart upload or temp file) and completing it in Finalize.
type Sink interface {
	// Create opens a new, not-yet-visible recording named id (see ValidID).
	Create(id string) (Object, error)
}

// Object is one in-progress recording. The first Write is a 44-byte WAV header
// with zero sizes; Finalize supplies the real header, which replaces it.
type Object interface {
	io.Writer
	// Finalize atomically publishes the recording: header replaces the first 44
	// bytes and sidecar is stored next to it. After Finalize or Abort the
	// Object is unusable.
	Finalize(header, sidecar []byte) error
	// Abort discards everything written.
	Abort() error
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ErrInvalidID rejects recording names that could escape the sink directory.
var ErrInvalidID = errors.New("record: invalid recording id")

// ValidID reports whether id is a safe recording name (letters, digits, '-' and '_').
func ValidID(id string) bool { return idRE.MatchString(id) }

const (
	partExt = ".part"
	wavExt  = ".wav"
	jsonExt = ".json"
	// A .wav.part not touched for this long belongs to a dead process.
	stalePartial = 10 * time.Minute
)

// LocalSink stores recordings in one directory (0700) with 0600 files. Files
// are written as <id>.wav.part and renamed on Finalize.
type LocalSink struct{ dir string }

// NewLocalSink creates dir (0700) if needed.
func NewLocalSink(dir string) (*LocalSink, error) {
	if dir == "" {
		return nil, errors.New("record: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Refuse a symlinked or non-directory target: Chmod and every file below
	// would follow it somewhere the operator did not configure.
	if fi, err := os.Lstat(dir); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("record: %s is not a directory (symlinks are refused)", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &LocalSink{dir: dir}, nil
}

func (l *LocalSink) Create(id string) (Object, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	f, err := os.OpenFile(filepath.Join(l.dir, id+wavExt+partExt), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &localObject{f: f, base: filepath.Join(l.dir, id)}, nil
}

type localObject struct {
	f    *os.File
	base string
}

func (o *localObject) Write(p []byte) (int, error) { return o.f.Write(p) }

func (o *localObject) Finalize(header, sidecar []byte) error {
	if _, err := o.f.WriteAt(header, 0); err != nil {
		_ = o.Abort()
		return err
	}
	if err := o.f.Sync(); err != nil {
		_ = o.Abort()
		return err
	}
	if err := o.f.Close(); err != nil {
		_ = os.Remove(o.base + wavExt + partExt)
		return err
	}
	if err := os.Rename(o.base+wavExt+partExt, o.base+wavExt); err != nil {
		_ = os.Remove(o.base + wavExt + partExt)
		return err
	}
	tmp := o.base + jsonExt + partExt
	_ = os.Remove(tmp) // a leftover from a crash; O_EXCL never follows a planted link
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(sidecar)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, o.base+jsonExt)
}

func (o *localObject) Abort() error {
	_ = o.f.Close()
	return os.Remove(o.base + wavExt + partExt)
}

// Sweep deletes recordings (and sidecars) last modified before now-maxAge and
// finalizes partial recordings abandoned by a crashed process. It returns the
// number of files removed. Only regular files named <valid id><ext> with this
// package's extensions are touched; symlinks and anything else are left alone.
func (l *LocalSink) Sweep(now time.Time, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !ownFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(l.dir, name)
		age := now.Sub(info.ModTime())
		switch {
		case age > maxAge:
			if err := os.Remove(path); err == nil {
				removed++
			} else if firstErr == nil {
				firstErr = err
			}
		case strings.HasSuffix(name, wavExt+partExt) && age > stalePartial:
			if err := recoverPartial(path); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return removed, firstErr
}

// ownFile reports whether name is one this sink writes.
func ownFile(name string) bool {
	for _, ext := range []string{wavExt + partExt, jsonExt + partExt, wavExt, jsonExt} {
		if id, ok := strings.CutSuffix(name, ext); ok {
			return ValidID(id)
		}
	}
	return false
}

// recoverPartial patches the sizes of an abandoned recording from its length
// and publishes it.
func recoverPartial(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < wavHeaderLen {
		_ = f.Close()
		return os.Remove(path)
	}
	hdr := make([]byte, wavHeaderLen)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return err
	}
	// Only patch what this package wrote; anything else ages out untouched.
	rate := binary.LittleEndian.Uint32(hdr[24:])
	if st.Size() > maxFileBytes || !bytes.Equal(hdr, wavHeader(int(rate), 0)) {
		return nil
	}
	data := (st.Size() - wavHeaderLen) / 4 * 4 // whole stereo frames
	if err := f.Truncate(wavHeaderLen + data); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+data))
	binary.LittleEndian.PutUint32(hdr[40:], uint32(data))
	if _, err := f.WriteAt(hdr, 0); err != nil {
		return err
	}
	return os.Rename(path, strings.TrimSuffix(path, partExt))
}

// Janitor sweeps now and then every interval until ctx ends.
func (l *LocalSink) Janitor(ctx context.Context, maxAge, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = time.Hour
	}
	run := func() {
		n, err := l.Sweep(time.Now(), maxAge)
		if err != nil && log != nil {
			log.Warn("recording sweep", "err", err.Error())
		}
		if n > 0 && log != nil {
			log.Info("recordings expired", "removed", n)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func wavHeader(rate int, dataLen uint32) []byte {
	h := make([]byte, wavHeaderLen)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+dataLen)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 2) // L=user, R=bot
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*4))
	binary.LittleEndian.PutUint16(h[32:], 4)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], dataLen)
	return h
}

const wavHeaderLen = 44
