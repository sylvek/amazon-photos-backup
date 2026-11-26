package amazon_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/amazon/amazontest"
)

// addMany registers n nodes whose dates are produced by dateOf(i).
func addMany(f *amazontest.Fake, prefix string, n int, dateOf func(i int) time.Time) {
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s%06d", prefix, i)
		f.AddNode(id, id+".jpg", dateOf(i).UTC().Format(time.RFC3339), []byte(id))
	}
}

func listAll(t *testing.T, f *amazontest.Fake) ([]amazon.Node, amazon.ListReport) {
	t.Helper()
	c, _ := newClient(t, f, f.Cookies())
	nodes, report, err := c.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.ID] {
			t.Fatalf("node %s listed twice", n.ID)
		}
		seen[n.ID] = true
	}
	f.Update(func(f *amazontest.Fake) {
		if f.BadRequests != 0 {
			t.Errorf("client made %d requests the API rejected (paging cap?)", f.BadRequests)
		}
	})
	return nodes, report
}

func TestListSmallLibrary(t *testing.T) {
	f := amazontest.New(t)
	addMany(f, "n", 450, func(i int) time.Time { return time.Date(2023, 5, 1, 0, 0, i, 0, time.UTC) })
	nodes, report := listAll(t, f)
	if len(nodes) != 450 || report != (amazon.ListReport{Total: 450, Listed: 450}) {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
	if n := len(f.RequestLog()); n != 3 {
		t.Errorf("made %d requests, want 3 pages", n)
	}
	if nodes[0].Content.MD5 == "" || nodes[0].Content.Size == 0 || nodes[0].Name == "" {
		t.Errorf("node metadata not decoded: %+v", nodes[0])
	}
}

func TestListEmptyLibrary(t *testing.T) {
	f := amazontest.New(t)
	nodes, report := listAll(t, f)
	if len(nodes) != 0 || report != (amazon.ListReport{}) {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListExactlyAtCap(t *testing.T) {
	f := amazontest.New(t)
	addMany(f, "n", 9999, func(i int) time.Time { return time.Date(2023, 5, 1, 0, 0, i, 0, time.UTC) })
	nodes, report := listAll(t, f)
	if len(nodes) != 9999 || report.Unreachable != 0 {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListPartitionsByYear(t *testing.T) {
	f := amazontest.New(t)
	for _, year := range []int{2021, 2022, 2023} {
		addMany(f, fmt.Sprintf("y%d-", year), 4000, func(i int) time.Time { return time.Date(year, 3, 1, 0, 0, i, 0, time.UTC) })
	}
	nodes, report := listAll(t, f)
	if len(nodes) != 12000 || report != (amazon.ListReport{Total: 12000, Listed: 12000}) {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListSplitsBigYearByMonth(t *testing.T) {
	f := amazontest.New(t)
	addMany(f, "big", 15000, func(i int) time.Time { return time.Date(2022, time.Month(1+i%12), 5, 0, 0, i, 0, time.UTC) })
	addMany(f, "old", 100, func(i int) time.Time { return time.Date(1998, 7, 1, 0, 0, i, 0, time.UTC) })
	nodes, report := listAll(t, f)
	if len(nodes) != 15100 || report.Unreachable != 0 {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListSplitsBigMonthByDay(t *testing.T) {
	f := amazontest.New(t)
	addMany(f, "jan", 12000, func(i int) time.Time { return time.Date(2022, 1, 1+i%28, 0, 0, i, 0, time.UTC) })
	nodes, report := listAll(t, f)
	if len(nodes) != 12000 || report.Unreachable != 0 {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListRecoversUndatedViaFallback(t *testing.T) {
	f := amazontest.New(t)
	addMany(f, "dated", 10050, func(i int) time.Time { return time.Date(2020, time.Month(1+i%12), 1, 0, 0, i, 0, time.UTC) })
	// Recent uploads the date filters cannot see (no capture date).
	addMany(f, "undated", 30, func(i int) time.Time { return time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC) })
	f.Update(func(f *amazontest.Fake) {
		for i := range f.Nodes {
			if len(f.Nodes[i].ID) > 7 && f.Nodes[i].ID[:7] == "undated" {
				f.Nodes[i].Content.ContentDate = ""
			}
		}
	})
	nodes, report := listAll(t, f)
	if len(nodes) != 10080 || report != (amazon.ListReport{Total: 10080, Listed: 10080}) {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListReportsUnreachable(t *testing.T) {
	f := amazontest.New(t)
	// More items on a single day than the API lets anyone page through.
	addMany(f, "burst", 10500, func(i int) time.Time { return time.Date(2022, 6, 15, 0, 0, 0, i, time.UTC) })
	nodes, report := listAll(t, f)
	if len(nodes) != 9999 || report != (amazon.ListReport{Total: 10500, Listed: 9999, Unreachable: 501}) {
		t.Fatalf("listed %d, report %+v", len(nodes), report)
	}
}

func TestListAuthError(t *testing.T) {
	f := amazontest.New(t)
	bad := f.Cookies()
	bad["at-acbfr"] = "expired"
	c, _ := newClient(t, f, bad)
	if _, _, err := c.ListAll(context.Background()); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}
