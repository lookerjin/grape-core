package coding

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// FileImage captures a regular file including its permissions, or its absence.
type FileImage struct {
	Exists bool
	Data   []byte
	Mode   os.FileMode
}

const MaxImageBytes = 1 << 20

var ErrImageTooLarge = errors.New("file image exceeds size limit")

func (i FileImage) Digest() string {
	if !i.Exists {
		return "absent"
	}
	return fmt.Sprintf("%o:%x", i.Mode.Perm(), sha256.Sum256(i.Data))
}
func (r *Directory) Path() string { return r.root.Name() }
func (r *Directory) Image(path string) (FileImage, error) {
	var image FileImage
	name, err := cleanRel(path)
	if err != nil {
		return image, err
	}
	info, err := r.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return image, nil
	}
	if err != nil {
		return image, err
	}
	if !info.Mode().IsRegular() {
		return image, fmt.Errorf("snapshot requires a regular file")
	}
	f, err := r.root.Open(name)
	if err != nil {
		return image, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return image, err
	}
	if !os.SameFile(info, actual) {
		return image, fmt.Errorf("file changed during snapshot")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return image, err
	}
	if len(data) > MaxImageBytes {
		return image, fmt.Errorf("%w: %d bytes", ErrImageTooLarge, MaxImageBytes)
	}
	return FileImage{true, data, actual.Mode().Perm()}, nil
}

// ReplaceImage rechecks the expected image immediately before replacing the
// path. An independent writer can still change it between the check and rename.
func (r *Directory) ReplaceImage(path, expected string, next FileImage) error {
	name, err := cleanRel(path)
	if err != nil {
		return err
	}
	check := func() error {
		i, e := r.Image(name)
		if e != nil {
			return e
		}
		if i.Digest() != expected {
			return fmt.Errorf("file changed since preview")
		}
		return nil
	}
	if !next.Exists {
		if e := check(); e != nil {
			return e
		}
		return r.root.Remove(name)
	}
	if len(next.Data) > MaxImageBytes {
		return fmt.Errorf("%w: %d bytes", ErrImageTooLarge, MaxImageBytes)
	}
	dir := filepath.Dir(name)
	if err = r.root.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".grape-restore-"+uuid.NewString())
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer r.root.Remove(tmp)
	if _, err = f.Write(next.Data); err == nil {
		err = f.Chmod(next.Mode.Perm())
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = check(); err != nil {
		return err
	}
	return r.root.Rename(tmp, name)
}
