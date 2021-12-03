package imagor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cshum/hybridcache"
	"go.uber.org/zap"
	"net/http"
	"strconv"
	"time"
)

const (
	Name      = "Imagor"
	Version   = "0.1.0"
	UserAgent = Name + "/" + Version
)

type LoadFunc func(string) ([]byte, error)

// Loader Load image from image source
type Loader interface {
	Load(r *http.Request, image string) ([]byte, error)
}

// Storage save image buffer
type Storage interface {
	Save(ctx context.Context, image string, buf []byte) error
}

// Store both a Loader and Storage
type Store interface {
	Loader
	Storage
}

// Processor process image buffer
type Processor interface {
	Startup(ctx context.Context) error
	Process(ctx context.Context, buf []byte, params Params, load LoadFunc) ([]byte, *Meta, error)
	Shutdown(ctx context.Context) error
}

// Imagor image resize HTTP handler
type Imagor struct {
	// Cache is meant to be a short-lived buffer and call suppression.
	// For actual caching please place this under a reverse-proxy and CDN
	Unsafe         bool
	Secret         string
	Loaders        []Loader
	Storages       []Storage
	Processors     []Processor
	RequestTimeout time.Duration
	Cache          cache.Cache `json:"-"`
	CacheTTL       time.Duration
	Logger         *zap.Logger `json:"-"`
	Debug          bool
}

func New(options ...Option) *Imagor {
	app := &Imagor{
		Logger:         zap.NewNop(),
		Cache:          cache.NewMemory(1000, 1<<28, time.Minute),
		CacheTTL:       time.Minute,
		RequestTimeout: time.Second * 30,
	}
	for _, option := range options {
		option(app)
	}
	if app.Debug {
		app.Logger.Debug("config", zap.Any("imagor", app))
	}
	return app
}

func (app *Imagor) Startup(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Startup(ctx); err != nil {
			return
		}
	}
	return
}

func (app *Imagor) Shutdown(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Shutdown(ctx); err != nil {
			return
		}
	}
	return
}

func (app *Imagor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.EscapedPath()
	if uri == "/" {
		resJSON(w, map[string]string{
			"app":     Name,
			"version": Version,
		})
		return
	}
	params := ParseParams(uri)
	if app.Debug {
		app.Logger.Debug("params", zap.Any("params", params), zap.String("uri", uri))
	}
	buf, meta, err := app.Do(r, params)
	ln := len(buf)
	if meta != nil {
		if params.Meta {
			resJSON(w, meta)
			return
		} else {
			w.Header().Set("Content-Type", meta.ContentType)
		}
	} else if ln > 0 {
		w.Header().Set("Content-Type", http.DetectContentType(buf))
	}
	if err != nil {
		if e, ok := WrapError(err).(Error); ok {
			if e == ErrPass {
				// passed till the end means not found
				e = ErrNotFound
			}
			w.WriteHeader(e.Code)
		}
		if ln > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(ln))
			w.Write(buf)
			return
		}
		resJSON(w, err)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(ln))
	w.Write(buf)
	return
}

func (app *Imagor) Do(r *http.Request, params Params) (buf []byte, meta *Meta, err error) {
	var cancel func()
	ctx := r.Context()
	if app.RequestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.RequestTimeout)
		defer cancel()
	}
	if !(app.Unsafe && params.Unsafe) && !params.Verify(app.Secret) {
		err = ErrHashMismatch
		if app.Debug {
			app.Logger.Debug("hash mismatch", zap.Any("params", params), zap.String("expected", Hash(params.Path, app.Secret)))
		}
		return
	}
	if buf, err = app.load(r, params.Image); err != nil {
		app.Logger.Info("load", zap.Any("params", params), zap.Error(err))
		return
	}
	load := func(image string) ([]byte, error) {
		return app.load(r, image)
	}
	for _, processor := range app.Processors {
		b, m, e := processor.Process(ctx, buf, params, load)
		if e == nil {
			buf = b
			meta = m
			if app.Debug {
				app.Logger.Debug("processed", zap.Any("params", params), zap.Any("meta", meta), zap.Int("size", len(buf)))
			}
			break
		} else {
			if e == ErrPass {
				if len(b) > 0 {
					// pass to next processor
					buf = b
				}
				if app.Debug {
					app.Logger.Debug("process", zap.Any("params", params), zap.Error(e))
				}
			} else {
				err = e
				app.Logger.Error("process", zap.Any("params", params), zap.Error(e))
			}
		}
	}
	return
}

func (app *Imagor) load(r *http.Request, image string) (buf []byte, err error) {
	buf, err = cache.NewFunc(app.Cache, app.RequestTimeout, app.CacheTTL, app.CacheTTL).
		DoBytes(r.Context(), image, func(ctx context.Context) (buf []byte, err error) {
			dr := r.WithContext(ctx)
			for _, loader := range app.Loaders {
				b, e := loader.Load(dr, image)
				if len(b) > 0 {
					buf = b
				}
				if e == nil {
					err = nil
					break
				}
				// should not log expected error as of now, as it has not reached the end
				if e != nil && e != ErrPass && e != ErrNotFound && !errors.Is(e, context.Canceled) {
					app.Logger.Error("load", zap.String("image", image), zap.Error(e))
				} else if app.Debug {
					app.Logger.Debug("load", zap.String("image", image), zap.Error(e))
				}
				err = e
			}
			if err == nil {
				if app.Debug {
					app.Logger.Debug("loaded", zap.String("image", image), zap.Int("size", len(buf)))
				}
				if len(app.Storages) > 0 {
					app.save(ctx, app.Storages, image, buf)
				}
			} else if !errors.Is(err, context.Canceled) {
				if err == ErrPass {
					err = ErrNotFound
				}
				// log non user-initiated error finally
				app.Logger.Error("load", zap.String("image", image), zap.Error(err))
			}
			return
		})
	err = WrapError(err)
	return
}

func (app *Imagor) save(
	ctx context.Context, storages []Storage, image string, buf []byte,
) {
	for _, storage := range storages {
		var cancel func()
		sCtx := DetachContext(ctx)
		if app.RequestTimeout > 0 {
			sCtx, cancel = context.WithTimeout(sCtx, app.RequestTimeout)
		}
		go func(s Storage) {
			defer cancel()
			if err := s.Save(sCtx, image, buf); err != nil {
				app.Logger.Error("save", zap.String("image", image), zap.Error(err))
			} else if app.Debug {
				app.Logger.Debug("saved", zap.String("image", image), zap.Int("size", len(buf)))
			}
		}(storage)
	}
}

func resJSON(w http.ResponseWriter, v interface{}) {
	buf, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.Write(buf)
	return
}
