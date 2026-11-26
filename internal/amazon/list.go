package amazon

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const (
	pageSize = 200
	// maxWindow is a hard limit of the search API: "Offset + limit cannot be
	// greater than 9999". No query can return more than this many items, so
	// larger libraries are listed in date slices.
	maxWindow = 9999

	mediaFilter  = "type:(PHOTOS OR VIDEOS)"
	earliestYear = 1900
)

// ListReport says how complete an inventory is.
type ListReport struct {
	Total       int      // items that exist, as far as Amazon tells
	Listed      int      // distinct items we could enumerate
	Unreachable int      // Total - Listed: items we know of but could not list
	Malformed   int      // items Amazon sent in a form we could not read
	SliceErrors []string // date slices Amazon refused to list
}

func (c *Client) searchPage(ctx context.Context, filter string, offset, limit int) (nodeList, error) {
	return c.getNodes(ctx, "/search", url.Values{
		"filters":         {filter},
		"limit":           {strconv.Itoa(limit)},
		"offset":          {strconv.Itoa(offset)},
		"lowResThumbnail": {"true"},
		"searchContext":   {"customer"},
		"sort":            {"['createdDate DESC']"},
	})
}

// collector gathers nodes, ignoring the duplicates that overlapping pages
// and slices produce.
type collector struct {
	seen      map[string]bool
	nodes     []Node
	malformed int
	skipped   []string
}

func (col *collector) add(page nodeList) {
	col.malformed += page.Malformed
	for _, n := range page.Data {
		if col.seen[n.ID] {
			continue
		}
		col.seen[n.ID] = true
		col.nodes = append(col.nodes, n)
	}
}

// pageRest fetches everything after the first page of a query, up to the
// paging cap. The announced count is a hint, not a promise: paging goes on
// for as long as pages come back full. It reports whether the cap was hit
// with items possibly left behind it.
func (c *Client) pageRest(ctx context.Context, filter string, first nodeList, col *collector) (capped bool, err error) {
	col.add(first)
	last := first.Raw
	for offset := pageSize; offset < maxWindow; offset += pageSize {
		if offset >= first.Count && last < pageSize {
			return false, nil
		}
		if offset+pageSize > maxWindow {
			offset = maxWindow - pageSize // last window allowed; overlaps the previous page
		}
		page, err := c.searchPage(ctx, filter, offset, pageSize)
		if err != nil {
			return false, err
		}
		if page.Raw == 0 {
			return false, nil
		}
		col.add(page)
		last = page.Raw
	}
	return last == pageSize, nil
}

// listSlice lists one date slice, cutting it into smaller slices while it
// holds more items than one query can return: year → months → days. A slice
// Amazon refuses outright is noted and skipped, so that one odd filter does
// not cost the whole inventory.
func (c *Client) listSlice(ctx context.Context, filter string, depth int, col *collector) error {
	first, err := c.searchPage(ctx, filter, 0, pageSize)
	if err != nil {
		if Permanent(err) {
			col.skipped = append(col.skipped, fmt.Sprintf("%s: %v", filter, err))
			return nil
		}
		return err
	}
	if first.Count <= maxWindow || depth == 2 {
		capped, err := c.pageRest(ctx, filter, first, col)
		if err != nil || !capped || depth == 2 || first.Count == maxWindow {
			return err
		}
		// The slice was fuller than its count admitted: cut it up after all.
	}
	key, n := "timeMonth", 12
	if depth == 1 {
		key, n = "timeDay", 31
	}
	for i := 1; i <= n; i++ {
		if err := c.listSlice(ctx, fmt.Sprintf("%s AND %s:(%d)", filter, key, i), depth+1, col); err != nil {
			return err
		}
	}
	return nil
}

// ListAll enumerates every photo and video of the account.
func (c *Client) ListAll(ctx context.Context) ([]Node, ListReport, error) {
	col := &collector{seen: map[string]bool{}}
	first, err := c.searchPage(ctx, mediaFilter, 0, pageSize)
	if err != nil {
		return nil, ListReport{}, err
	}
	total := first.Count
	sliced := total > maxWindow
	if !sliced {
		capped, err := c.pageRest(ctx, mediaFilter, first, col)
		if err != nil {
			return nil, ListReport{}, err
		}
		// A full last window with a count that does not explain it means
		// the library is bigger than announced.
		sliced = capped && total != maxWindow
	}
	if sliced {
		for year := time.Now().Year() + 1; year >= earliestYear; year-- {
			if err := c.listSlice(ctx, fmt.Sprintf("%s AND timeYear:(%d)", mediaFilter, year), 0, col); err != nil {
				return nil, ListReport{}, err
			}
		}
		if len(col.nodes) < total {
			// Items without a usable date escape the date slices. The most
			// recently added ones are still reachable without any filter.
			malformed := col.malformed
			if _, err := c.pageRest(ctx, mediaFilter, first, col); err != nil {
				return nil, ListReport{}, err
			}
			col.malformed = malformed // this pass re-reads pages already counted
		}
	}
	report := ListReport{Total: max(total, len(col.nodes)+col.malformed), Listed: len(col.nodes), Malformed: col.malformed, SliceErrors: col.skipped}
	if report.Listed < report.Total {
		report.Unreachable = report.Total - report.Listed
	}
	return col.nodes, report, nil
}
