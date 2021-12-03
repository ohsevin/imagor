package filestore

import (
	"context"
	"github.com/cshum/imagor"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var dotFileRegex = regexp.MustCompile("/\\.")

type FileStore struct {
	BaseDir    string
	PathPrefix string
	Blacklists []*regexp.Regexp `json:"-"`
}

func New(baseDir string, options ...Option) *FileStore {
	s := &FileStore{
		BaseDir:    baseDir,
		PathPrefix: "/",
		Blacklists: []*regexp.Regexp{dotFileRegex},
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *FileStore) Path(image string) (string, bool) {
	image = "/" + strings.TrimPrefix(path.Clean(
		strings.ReplaceAll(image, ":/", "%3A"),
	), "/")
	for _, blacklist := range s.Blacklists {
		if blacklist.MatchString(image) {
			return "", false
		}
	}
	if !strings.HasPrefix(image, s.PathPrefix) {
		return "", false
	}
	return filepath.Join(s.BaseDir, strings.TrimPrefix(image, s.PathPrefix)), true
}

func (s *FileStore) Load(_ *http.Request, image string) ([]byte, error) {
	image, ok := s.Path(image)
	if !ok {
		return nil, imagor.ErrPass
	}
	r, err := os.Open(image)
	if os.IsNotExist(err) {
		return nil, imagor.ErrNotFound
	}
	return io.ReadAll(r)
}

func (s *FileStore) Save(_ context.Context, image string, buf []byte) (err error) {
	if _, err = os.Stat(s.BaseDir); err != nil {
		return
	}
	image, ok := s.Path(image)
	if !ok {
		return imagor.ErrPass
	}
	if err = os.MkdirAll(filepath.Dir(image), 0755); err != nil {
		return
	}
	w, err := os.Create(image)
	if err != nil {
		return
	}
	defer w.Close()
	if _, err = w.Write(buf); err != nil {
		return
	}
	return
}
