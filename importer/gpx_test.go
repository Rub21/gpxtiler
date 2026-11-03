package importer

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, r io.Reader, skip func(int64) bool) (*Reader, []*Trace) {
	t.Helper()
	rd, err := NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	var out []*Trace
	for {
		tr, err := rd.Next(skip)
		if err == io.EOF {
			return rd, out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tr)
	}
}

func TestReaderFixture(t *testing.T) {
	f, err := os.Open("../testdata/trackable.gpx")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd, traces := readAll(t, f, nil)

	if want := time.Date(2013, 8, 1, 12, 0, 0, 0, time.UTC); !rd.DumpTime.Equal(want) {
		t.Errorf("dump time = %s", rd.DumpTime)
	}
	if len(traces) != 4 {
		t.Fatalf("got %d traces, want 4", len(traces))
	}
	t1 := traces[0]
	if t1.ID != 1 || *t1.Name != "Morning ride" || *t1.Description != "Vaduz <loop> & back" {
		t.Errorf("trace 1 = %d %q %q", t1.ID, *t1.Name, *t1.Description)
	}
	if strings.Join(t1.Tags, ",") != "vaduz,bike" {
		t.Errorf("tags = %v", t1.Tags)
	}
	if len(t1.Segments) != 2 || len(t1.Segments[0]) != 4 || len(t1.Segments[1]) != 1 {
		t.Errorf("segments = %v", t1.Segments)
	}
	if !t1.Segments[0][3].Time.IsZero() || t1.Segments[0][0].Time.IsZero() {
		t.Errorf("point times wrong")
	}
	if traces[1].ID != 2 || traces[1].Name != nil || traces[1].Description != nil {
		t.Errorf("trace 2 should have no name or desc")
	}
	if traces[3].Tags[0] != "a<b" {
		t.Errorf("tag = %q", traces[3].Tags[0])
	}
}

func TestReaderSkipGeometry(t *testing.T) {
	f, err := os.Open("../testdata/trackable.gpx")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, traces := readAll(t, f, func(id int64) bool { return id == 1 })
	if len(traces[0].Segments) != 0 || len(traces[0].Tags) != 2 {
		t.Errorf("trace 1 should keep tags and skip segments")
	}
	if len(traces[1].Segments) != 1 {
		t.Errorf("trace 2 should keep segments")
	}
}

func TestReaderErrors(t *testing.T) {
	cases := map[string]string{
		"no time":   `<gpx><trk><osm:id xmlns:osm="` + osmNS + `">1</osm:id></trk></gpx>`,
		"truncated": `<gpx><time>2020-01-01T00:00:00Z</time><trk><osm:id xmlns:osm="` + osmNS + `">1</osm:id><trkseg>`,
		"no id":     `<gpx><time>2020-01-01T00:00:00Z</time><trk><trkseg></trkseg></trk></gpx>`,
	}
	for name, doc := range cases {
		rd, err := NewReader(strings.NewReader(doc))
		for err == nil {
			_, err = rd.Next(nil)
		}
		if err == io.EOF {
			t.Errorf("%s: expected an error", name)
		}
	}
}
