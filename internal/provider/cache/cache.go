// Package cache stores model responses on disk so that repeating a run does
// not repeat paid API calls.
//
// The key covers everything that is sent to the model: provider, model,
// system prompt, user prompt, tool definitions and seed. Change any of them,
// for example a tool description, and the request is sent again. Only
// successful responses are stored.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Provider wraps another provider with a disk cache.
type Provider struct {
	next         provider.Provider
	dir          string
	hits, misses atomic.Int64
}

// Wrap returns p with responses cached in dir, which is created if needed.
func Wrap(p provider.Provider, dir string) (*Provider, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	return &Provider{next: p, dir: dir}, nil
}

// Name implements provider.Provider. It reports the wrapped provider, since
// cached answers are that provider's answers.
func (c *Provider) Name() string { return c.next.Name() }

// Stats returns how many requests were served from the cache and how many
// went to the provider.
func (c *Provider) Stats() (hits, misses int64) { return c.hits.Load(), c.misses.Load() }

// Complete implements provider.Provider.
func (c *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	path := filepath.Join(c.dir, c.key(req)+".json")
	if data, err := os.ReadFile(path); err == nil {
		var resp provider.Response
		if json.Unmarshal(data, &resp) == nil {
			c.hits.Add(1)
			return resp, nil
		}
		// A corrupt entry is treated as a miss and overwritten below.
	} else if !errors.Is(err, fs.ErrNotExist) {
		return provider.Response{}, fmt.Errorf("cache: %w", err)
	}

	c.misses.Add(1)
	resp, err := c.next.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	if data, err := json.Marshal(resp); err == nil {
		// Write to a temp file and rename, so a crash or a parallel worker
		// never leaves a half-written entry.
		tmp, err := os.CreateTemp(c.dir, ".tmp-*")
		if err == nil {
			_, werr := tmp.Write(data)
			cerr := tmp.Close()
			if werr == nil && cerr == nil {
				err = os.Rename(tmp.Name(), path)
			}
			if werr != nil || cerr != nil || err != nil {
				os.Remove(tmp.Name())
			}
		}
	}
	return resp, nil
}

// key hashes the parts of the request the model sees. Oracle is left out on
// purpose: it is test-double input that real models never receive.
func (c *Provider) key(req provider.Request) string {
	b, _ := json.Marshal(struct {
		Provider string
		Model    string
		System   string
		Prompt   string
		Tools    []spec.Tool
		Seed     uint64
	}{c.next.Name(), req.Model, req.System, req.Prompt, req.Tools, req.Seed})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
