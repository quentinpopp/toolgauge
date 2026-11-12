package cache

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

type counting struct {
	calls atomic.Int32
	err   error
}

func (c *counting) Name() string { return "fake" }

func (c *counting) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	c.calls.Add(1)
	if c.err != nil {
		return provider.Response{}, c.err
	}
	return provider.Response{
		ToolCalls: []provider.ToolCall{{Name: "t", Args: map[string]any{"seed": float64(req.Seed)}}},
		Usage:     provider.Usage{InputTokens: 10},
	}, nil
}

var req = provider.Request{Model: "m", Prompt: "p", Seed: 1, Tools: []spec.Tool{{Name: "t", Description: "d"}}}

func TestHitAvoidsCall(t *testing.T) {
	inner := &counting{}
	c, err := Wrap(inner, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := c.Complete(context.Background(), req)
	second, _ := c.Complete(context.Background(), req)
	if inner.calls.Load() != 1 {
		t.Fatalf("want 1 provider call, got %d", inner.calls.Load())
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cached answer differs: %+v vs %+v", first, second)
	}
	if h, m := c.Stats(); h != 1 || m != 1 {
		t.Errorf("stats = %d hits, %d misses", h, m)
	}
}

func TestKeyCoversWhatTheModelSees(t *testing.T) {
	inner := &counting{}
	c, _ := Wrap(inner, t.TempDir())
	variants := []provider.Request{req, req, req, req, req}
	variants[1].Seed = 2
	variants[2].Prompt = "other"
	variants[3].Tools = []spec.Tool{{Name: "t", Description: "reworded"}}
	variants[4].Model = "other-model"
	for _, v := range variants {
		c.Complete(context.Background(), v)
	}
	if n := inner.calls.Load(); n != 5 {
		t.Fatalf("each change must miss the cache: %d calls, want 5", n)
	}
	// The oracle is not sent to real models, so it must not split the cache.
	withOracle := req
	withOracle.Oracle = &provider.ToolCall{Name: "x"}
	c.Complete(context.Background(), withOracle)
	if n := inner.calls.Load(); n != 5 {
		t.Fatal("oracle must not be part of the key")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	inner := &counting{err: errors.New("429")}
	dir := t.TempDir()
	c, _ := Wrap(inner, dir)
	c.Complete(context.Background(), req)
	c.Complete(context.Background(), req)
	if inner.calls.Load() != 2 {
		t.Fatal("a failed request must be retried, not served from cache")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("nothing should be written for errors, found %d entries", len(entries))
	}
}

func TestPersistsAcrossInstancesAndIgnoresCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	inner := &counting{}
	c1, _ := Wrap(inner, dir)
	c1.Complete(context.Background(), req)
	c2, _ := Wrap(inner, dir)
	c2.Complete(context.Background(), req)
	if inner.calls.Load() != 1 {
		t.Fatal("a new run should reuse the cache on disk")
	}
	entries, _ := os.ReadDir(dir)
	os.WriteFile(dir+"/"+entries[0].Name(), []byte("{broken"), 0o644)
	if _, err := c2.Complete(context.Background(), req); err != nil || inner.calls.Load() != 2 {
		t.Fatalf("corrupt entry should be a miss, got err=%v calls=%d", err, inner.calls.Load())
	}
}

func TestConcurrentWriters(t *testing.T) {
	inner := &counting{}
	c, _ := Wrap(inner, t.TempDir())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(context.Background(), req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if resp, err := c.Complete(context.Background(), req); err != nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("entry unreadable after concurrent writes: %+v %v", resp, err)
	}
}
