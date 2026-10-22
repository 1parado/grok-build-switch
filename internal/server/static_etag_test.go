package server

import (
	"fmt"
	"sync"
	"testing"
)

func TestStaticETagConcurrentDistinctNames(t *testing.T) {
	const n = 64
	var wg sync.WaitGroup
	errCh := make(chan string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("ui/asset-%d.js", i)
			body := []byte(name)
			got := staticETag(name, body)
			if got != staticETag(name, body) {
				errCh <- name + " changed"
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}
