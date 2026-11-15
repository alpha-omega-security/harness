//go:build pricecheck

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

const (
	priceFeedURL = "https://models.dev/api.json"
	// The feed is about 7 MB.
	priceFeedMaxBytes = 64 << 20
	priceFeedTimeout  = 2 * time.Minute
)

// TestPricingMatchesFeed fetches the live feed, so it runs only under the
// pricecheck build tag.
func TestPricingMatchesFeed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), priceFeedTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, priceFeedURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", priceFeedURL, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, priceFeedMaxBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > priceFeedMaxBytes {
		t.Fatalf("%s is larger than %d bytes", priceFeedURL, priceFeedMaxBytes)
	}
	var feed priceFeed
	if err := json.Unmarshal(raw, &feed); err != nil {
		t.Fatalf("decoding %s: %v", priceFeedURL, err)
	}
	for _, provider := range feedProviders {
		if len(feed[provider].Models) == 0 {
			t.Fatalf("%s lists no %s models", priceFeedURL, provider)
		}
	}

	for _, line := range pricingDrift(feed, acceptedPriceDrift) {
		t.Error(line)
	}
}
