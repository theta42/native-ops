package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Limits for an uploaded configuration tree. A real native-ops-conf is a few hundred KB.
const (
	MaxArchiveBytes   = 32 << 20  // the compressed upload
	MaxExtractedBytes = 128 << 20 // everything it unpacks to
	MaxArchiveFiles   = 5000
)

// ArchiveError is a problem with an uploaded archive. Its message is safe to return to the
// caller: it names the offending entry (quoted) and never a path on this host.
type ArchiveError struct{ msg string }

func (e *ArchiveError) Error() string { return e.msg }

func badArchive(format string, a ...any) error { return &ArchiveError{msg: fmt.Sprintf(format, a...)} }

// The limits in force. They are variables only so tests can lower them.
var (
	maxArchiveBytes   int64 = MaxArchiveBytes
	maxExtractedBytes int64 = MaxExtractedBytes
	maxArchiveFiles         = MaxArchiveFiles
)

// ExtractTarGz unpacks a gzip-compressed tar into dest, which must be an existing, empty
// directory that nothing else writes to. The archive is untrusted, so it is held to a small
// set of rules and anything else is refused, rather than skipped:
//
//   - only regular files and directories: no symlinks, hard links, devices or FIFOs, so
//     nothing in the tree can point outside it, and no file can be written through a link;
//   - every name is relative and stays inside dest (no absolute path, no "..");
//   - a file is never created over an existing entry and a directory never over a file (so a
//     name used twice, or as both, is refused);
//   - at most MaxArchiveFiles entries and MaxExtractedBytes of content (counted as bytes
//     actually written, not as the headers claim), so a small upload cannot expand into a full disk.
//
// Files are created 0600 and directories 0700 whatever the archive says: the tree is only
// ever read by this process, and permission bits or setuid flags from the archive mean nothing here.
func ExtractTarGz(r io.Reader, dest string) error {
	lr := &limitedReader{r: r, n: maxArchiveBytes}
	gz, err := gzip.NewReader(lr)
	if err != nil {
		return badArchive("the upload is not a gzip-compressed tar archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var files int
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return badArchive("the upload is larger than %s", humanBytes(maxArchiveBytes))
			}
			return badArchive("the upload is not a valid tar archive")
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := cleanEntryName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue // the root directory itself
		}
		files++
		if files > maxArchiveFiles {
			return badArchive("the archive has more than %d entries", maxArchiveFiles)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirs(target, name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := mkdirs(filepath.Dir(target), name); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if os.IsExist(err) {
					return badArchive("the archive contains %q twice, or as both a file and a directory", name)
				}
				return fmt.Errorf("extract: %w", err)
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, maxExtractedBytes-written+1))
			closeErr := f.Close()
			written += n
			if written > maxExtractedBytes {
				return badArchive("the archive expands to more than %s", humanBytes(maxExtractedBytes))
			}
			if copyErr != nil {
				if errors.Is(copyErr, errTooLarge) {
					return badArchive("the upload is larger than %s", humanBytes(maxArchiveBytes))
				}
				return badArchive("the upload is not a valid tar archive")
			}
			if closeErr != nil {
				return fmt.Errorf("extract: %w", closeErr)
			}
		default:
			return badArchive("the archive entry %q is a %s; only files and directories are allowed", name, entryKind(hdr.Typeflag))
		}
	}
}

// mkdirs creates a directory the archive needs. A component that is already a file is the
// archive contradicting itself; that is reported as such, without the path on this host.
func mkdirs(dir, name string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, fs.ErrExist) {
			return badArchive("the archive entry %q conflicts with another entry", name)
		}
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

// cleanEntryName returns the slash-separated, cleaned, relative name of an entry, or an
// ArchiveError if it is absolute, climbs out of the tree, or is not a plain name.
func cleanEntryName(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", badArchive("the archive has an entry with an invalid name %q", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", badArchive("the archive entry %q is an absolute path", raw)
	}
	c := path.Clean(raw)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", badArchive("the archive entry %q climbs out of the directory", raw)
	}
	return c, nil
}

func entryKind(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symbolic link"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "FIFO"
	}
	return "special entry"
}

func humanBytes(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

var errTooLarge = errors.New("too large")

// limitedReader stops with errTooLarge once more than n bytes were read (unlike io.LimitReader,
// which would end the stream quietly and let a truncated upload look complete).
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n, errTooLarge
	}
	return n, err
}
