package optimizer

import (
	"context"
	"net/http"
)

func httpRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, "GET", url, nil)
}
