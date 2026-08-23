package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// errStopPagination ends a page walk early without failing it.
var errStopPagination = errors.New("stop GitHub pagination")

// paginate walks the GitHub collection at path, decoding each page into T and
// handing it to visit. The page count is bounded and cyclic Link headers are
// rejected. visit may return errStopPagination to end the walk without error.
func paginate[T any](ctx context.Context, c *Client, path, collection string, visit func(T) error) error {
	seen := map[string]bool{}
	for pages := 0; path != ""; pages++ {
		if pages >= maxPages || seen[path] {
			return fmt.Errorf("GitHub pagination is bounded or cyclic")
		}
		seen[path] = true
		res, err := c.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var page T
		err = json.NewDecoder(res.Body).Decode(&page)
		link := res.Header.Get("Link")
		if closeErr := res.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := visit(page); err != nil {
			if errors.Is(err, errStopPagination) {
				return nil
			}
			return err
		}
		next, err := c.paginationPathFor(link, path, collection)
		if err != nil {
			return err
		}
		path = next
	}
	return nil
}
