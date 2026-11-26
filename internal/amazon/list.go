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
	Total       int // items Amazon says exist
	Listed      int // distinct items we could enumerate
	Unreachable int // Total - Listed, when the paging cap could not be worked around
}

func (c *Client) searchPage(ctx context.Context, filter string, offset, limit int) (nodeList, error) {
	var page nodeList
	err := c.getJSON(ctx, "/search", url.Values{
		"filters":         {filter},
		"limit":           {strconv.Itoa(limit)},
		"offset":          {strconv.Itoa(offset)},
		"lowResThumbnail": {"true"},
		"searchContext":   {"customer"},
		"sort":            {"['createdDate DESC']"},
	}, &page)
	return page, err
}

// collector gathers nodes, ignoring the duplicates that overlapping pages
// and slices produce.
type collector struct {
	seen  map[string]bool
	nodes []Node
}

func (col *collector) add(nodes []Node) {
	for _, n := range nodes {
		if n.ID == "" || col.seen[n.ID] {
			continue
		}
		col.seen[n.ID] = true
		col.nodes = append(col.nodes, n)
	}
}

// pageRest fetches everything after the first page of a query, up to the
// paging cap.
func (c *Client) pageRest(ctx context.Context, filter string, first nodeList, col *collector) error {
	col.add(first.Data)
	end := min(first.Count, maxWindow)
	for offset := pageSize; offset < end; offset += pageSize {
		if offset+pageSize > maxWindow {
			offset = maxWindow - pageSize // last window allowed; overlaps the previous page
		}
		page, err := c.searchPage(ctx, filter, offset, pageSize)
		if err != nil {
			return err
		}
		if len(page.Data) == 0 {
			break
		}
		col.add(page.Data)
	}
	return nil
}

// listSlice lists one date slice, cutting it into smaller slices while it
// holds more items than one query can return: year → months → days.
func (c *Client) listSlice(ctx context.Context, filter string, depth int, col *collector) error {
	first, err := c.searchPage(ctx, filter, 0, pageSize)
	if err != nil {
		return err
	}
	if first.Count <= maxWindow || depth == 2 {
		return c.pageRest(ctx, filter, first, col)
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
	if total <= maxWindow {
		if err := c.pageRest(ctx, mediaFilter, first, col); err != nil {
			return nil, ListReport{}, err
		}
	} else {
		for year := time.Now().Year() + 1; year >= earliestYear; year-- {
			if err := c.listSlice(ctx, fmt.Sprintf("%s AND timeYear:(%d)", mediaFilter, year), 0, col); err != nil {
				return nil, ListReport{}, err
			}
		}
		if len(col.nodes) < total {
			// Items without a usable date escape the date slices. The most
			// recently added ones are still reachable without any filter.
			if err := c.pageRest(ctx, mediaFilter, first, col); err != nil {
				return nil, ListReport{}, err
			}
		}
	}
	report := ListReport{Total: total, Listed: len(col.nodes)}
	if report.Listed < total {
		report.Unreachable = total - report.Listed
	}
	return col.nodes, report, nil
}
